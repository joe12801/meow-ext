package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 在线曲目的封面与歌词补全。
//
// 为什么需要这一层：Meow 的 /api/search 只给歌名/歌手/时长，cover_url 恒为空
// （只有喜马拉雅、5sing 这类按 id 直取的源才带封面），缓存目录里也只有
// music.mp3，没有 cover.jpg / lyric.lrc。而飞牛的海报位和歌词面板完全靠扩展
// 自己出图出词，缺了这一层就只剩音源 LOGO 和空白的歌词页。
//
// 取数来源选网易云公开接口，原因是实测口径：
//   - POST /api/cloudsearch/pc 仍是明文 JSON（GET /api/search/get 已失效，
//     会对「晴天」返回一首叫「93」的歌），字段是 ar/al/dt；
//   - al.picUrl 是专辑封面，加 ?param=NNNyNNN 就能按客户端要的尺寸取缩略图，
//     600x600 只有 300KB，原图却有 7MB，不裁剪会把流量和内存都吃满；
//   - /api/song/lyric 给标准 LRC，能直接喂给飞牛。
//
// 全流程是"尽力而为"：超时、风控、搜不到都只是静默降级成 LOGO / 空歌词，
// 绝不允许它影响播放与搜索。
const neteaseBase = "https://music.163.com"

const neteaseUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// metaInfo 是一条曲目的补全结果。
type metaInfo struct {
	Cover string // 专辑封面直链（已去掉尺寸参数，使用时再拼 ?param=）
	Album string
	Lyric string
	// NeID 是匹配到的网易云歌曲 id，歌词按需用它再取一次（不预先拉，
	// 绝大多数曲目只被"看"不被"听"，预取歌词等于给外网白送请求）。
	NeID int64
}

// metaEntry 是缓存项。ok=false 表示"这次没查到"，用短 TTL 让它有机会重试，
// 而不是把一次网络抖动记成七天的定论。
type metaEntry struct {
	info metaInfo
	ok   bool
	at   time.Time
}

const (
	metaHitTTL  = 7 * 24 * time.Hour
	metaMissTTL = 10 * time.Minute
	// metaMaxEntries 限制内存占用：一条几百字节，四千条也就几 MB。
	metaMaxEntries = 4096
	// metaWorkers 限制同时打外网的请求数。搜索列表一次可能来几十个封面请求，
	// 不限流会把网易云的接口打出 429，反而全都拿不到。
	metaWorkers = 4
)

// metaResolver 给"歌名+歌手 → 封面/歌词"做缓存、单飞与并发限流。
//
// 单飞（singleflight）是必须的：一次列表渲染会对同一首歌连发 2~3 个封面请求
// （列表 120/160、播放页 400/600），不合并就会对网易云搜同一遍三次。
type metaResolver struct {
	client *http.Client

	mu       sync.Mutex
	cache    map[string]metaEntry
	inflight map[string]*metaCall
	sem      chan struct{}
}

type metaCall struct {
	done chan struct{}
	info metaInfo
	ok   bool
}

func newMetaResolver() *metaResolver {
	return &metaResolver{
		client: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 6 * time.Second,
			},
		},
		cache:    make(map[string]metaEntry),
		inflight: make(map[string]*metaCall),
		sem:      make(chan struct{}, metaWorkers),
	}
}

// metaKey 把 (歌名, 歌手) 归一成缓存键。大小写与空白差异不该造成两次查询。
func metaKey(song, singer string) string {
	return strings.ToLower(strings.TrimSpace(song)) + "\x1f" +
		strings.ToLower(strings.TrimSpace(singer))
}

// Get 取补全结果。
//
// wait 是"愿意为它等多久"：封面接口给几百毫秒（拿不到就先回 LOGO，
// 后台继续解析），歌词接口给几秒（用户正盯着歌词页）。
// wait<=0 表示只查缓存/触发后台解析，立即返回。
func (m *metaResolver) Get(ctx context.Context, key string, wait time.Duration,
	fetch func(context.Context) (metaInfo, bool)) metaInfo {
	if key == "" {
		return metaInfo{}
	}
	m.mu.Lock()
	if e, ok := m.cache[key]; ok && time.Since(e.at) < entryTTL(e.ok) {
		m.mu.Unlock()
		return e.info
	}
	call, running := m.inflight[key]
	if !running {
		call = &metaCall{done: make(chan struct{})}
		m.inflight[key] = call
		go m.run(key, call, fetch)
	}
	m.mu.Unlock()

	if wait <= 0 {
		return metaInfo{}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-call.done:
		return call.info
	case <-timer.C:
		return metaInfo{}
	case <-ctx.Done():
		return metaInfo{}
	}
}

// Peek 只看缓存，永不阻塞、永不触发请求。
func (m *metaResolver) Peek(key string) metaInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.cache[key]; ok && time.Since(e.at) < entryTTL(e.ok) {
		return e.info
	}
	return metaInfo{}
}

// SetLyric 把后取的歌词写回缓存，避免每次开歌词页都重新拉一遍。
func (m *metaResolver) SetLyric(key, text string) {
	if key == "" || strings.TrimSpace(text) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.cache[key]
	if e.at.IsZero() {
		e.at = time.Now()
	}
	e.info.Lyric = text
	e.ok = true
	m.cache[key] = e
}

func entryTTL(ok bool) time.Duration {
	if ok {
		return metaHitTTL
	}
	return metaMissTTL
}

func (m *metaResolver) run(key string, call *metaCall, fetch func(context.Context) (metaInfo, bool)) {
	// 先在信号量外面判一次，避免排队排到超时还什么都没做。
	select {
	case m.sem <- struct{}{}:
	default:
		// 队列已满：直接记成"未命中"并放行，别再往里堆。等锁的那几个请求
		// 会立刻拿到零值走 LOGO，界面不会因为限流而卡住。
		m.finish(key, call, metaInfo{}, false)
		return
	}
	defer func() { <-m.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	info, ok := fetch(ctx)
	m.finish(key, call, info, ok)
}

func (m *metaResolver) finish(key string, call *metaCall, info metaInfo, ok bool) {
	m.mu.Lock()
	if len(m.cache) >= metaMaxEntries {
		m.evictLocked()
	}
	m.cache[key] = metaEntry{info: info, ok: ok, at: time.Now()}
	delete(m.inflight, key)
	call.info, call.ok = info, ok
	m.mu.Unlock()
	close(call.done)
}

// evictLocked 丢掉三分之一最旧的条目。全清会让正在被反复访问的热门曲目
// 反复重查，丢最旧的一批更平滑。
func (m *metaResolver) evictLocked() {
	drop := len(m.cache) / 3
	if drop < 1 {
		drop = 1
	}
	type pair struct {
		k  string
		at time.Time
	}
	oldest := make([]pair, 0, len(m.cache))
	for k, e := range m.cache {
		oldest = append(oldest, pair{k, e.at})
	}
	for i := 1; i < len(oldest); i++ {
		for j := i; j > 0 && oldest[j].at.Before(oldest[j-1].at); j-- {
			oldest[j], oldest[j-1] = oldest[j-1], oldest[j]
		}
	}
	for i := 0; i < drop && i < len(oldest); i++ {
		delete(m.cache, oldest[i].k)
	}
}

// ---------------------------------------------------------------- 网易云

type neSong struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Fee  int    `json:"fee"`
	AR   []struct {
		Name string `json:"name"`
	} `json:"ar"`
	AL struct {
		Name   string `json:"name"`
		PicURL string `json:"picUrl"`
	} `json:"al"`
	DT int64 `json:"dt"`
}

type neSearchResp struct {
	Code   int `json:"code"`
	Result struct {
		Songs []neSong `json:"songs"`
	} `json:"result"`
}

// nePost 用表单 POST 打网易云接口。
//
// 必须是 POST：GET /api/search/get 系列已经被风控改成返回不相干的结果，
// cloudsearch/pc 走 POST + Referer 仍是明文 JSON。
func (s *Server) nePost(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, neteaseBase+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", neteaseUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://music.163.com/")
	resp, err := s.meta.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &MeowError{Status: resp.StatusCode, Msg: "网易云返回非 200"}
	}
	return json.Unmarshal(body, out)
}

// fetchMetaInfo 搜一次网易云，挑一条最像的曲子回封面与专辑名。
//
// strict 为真时只接受标题完全一致的候选（见 metaKeyFor 的说明）。
func (s *Server) fetchMetaInfo(ctx context.Context, song, singer string, strict bool) (metaInfo, bool) {
	song = strings.TrimSpace(song)
	if song == "" {
		return metaInfo{}, false
	}
	keyword := song
	if singer != "" {
		keyword = song + " " + singer
	}
	form := url.Values{}
	form.Set("s", keyword)
	form.Set("type", "1")
	form.Set("offset", "0")
	form.Set("limit", "8")
	form.Set("total", "true")

	var resp neSearchResp
	if err := s.nePost(ctx, "/api/cloudsearch/pc", form, &resp); err != nil {
		log.Printf("[meta] 搜索失败 song=%q singer=%q: %v", song, singer, err)
		return metaInfo{}, false
	}
	best, ok := pickSong(resp.Result.Songs, song, singer, strict)
	if !ok {
		return metaInfo{}, false
	}
	cover := strings.TrimSpace(best.AL.PicURL)
	// 统一走 https：源站两种协议都支持，但客户端到扩展是 https 时，
	// 混用 http 图片在某些浏览器里会被当成混合内容拦掉。
	cover = strings.Replace(cover, "http://", "https://", 1)
	info := metaInfo{Cover: cover, Album: strings.TrimSpace(best.AL.Name), NeID: best.ID}
	return info, cover != ""
}

// neteaseLyric 按歌曲 id 取 LRC 原文。
func (s *Server) neteaseLyric(ctx context.Context, id int64) string {
	if id <= 0 {
		return ""
	}
	form := url.Values{}
	form.Set("id", strconv.FormatInt(id, 10))
	form.Set("lv", "1")
	form.Set("kv", "1")
	form.Set("tv", "-1")
	var resp struct {
		LRC struct {
			Lyric string `json:"lyric"`
		} `json:"lrc"`
	}
	if err := s.nePost(ctx, "/api/song/lyric", form, &resp); err != nil {
		log.Printf("[meta] 取歌词失败 id=%d: %v", id, err)
		return ""
	}
	return cleanLyric(resp.LRC.Lyric)
}

// cleanLyric 过滤掉"纯音乐，请欣赏"这类占位，它们不是歌词，
// 塞给客户端只会得到一个满屏一行字的歌词面板。
func cleanLyric(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	head := text
	if len(head) > 80 {
		head = head[:80]
	}
	if !strings.Contains(head, "[") && (strings.Contains(head, "纯音乐") ||
		strings.Contains(head, "暂无歌词") || strings.Contains(head, "没有歌词")) {
		return ""
	}
	return text
}

// metaNoise 是标题里的翻唱/改编信号词。命中就降权，但不硬排除——
// 有些歌全网只有翻唱版，硬排除等于永远没有封面。
var metaNoise = []string{
	"原唱", "翻唱", "cover", "版", "live", "现场", "伴奏", "纯音乐", "dj",
	"remix", "女声", "钢琴", "弦乐", "史诗", "串烧", "试听", "demo",
	"instrumental", "karaoke", "remaster", "重置版",
}

var bracketRE = regexp.MustCompile(`[（(【\[].*?[）)】\]]`)

// cleanTitle 去掉括号备注（"晴天 (原唱 周杰伦)" → "晴天"）并归一空白与大小写。
func cleanTitle(v string) string {
	v = bracketRE.ReplaceAllString(v, "")
	v = strings.ReplaceAll(v, "\u3000", "")
	v = strings.Join(strings.Fields(v), "")
	return strings.ToLower(v)
}

// pickSong 给搜索结果打分，挑最像原曲的一条。
//
// 打分口径直接照抄参考项目 fnos_music_ext 的实测经验：歌名完全相同 +12，
// 互相包含 +4，歌手对得上 +6，标题里带翻唱信号的每个 -3。宁可选不出来
// （返回 false 走 LOGO）也不要选一条八竿子打不着的专辑封面——错封面比没封面更刺眼。
func pickSong(songs []neSong, song, singer string, strict bool) (neSong, bool) {
	target := cleanTitle(song)
	if target == "" {
		return neSong{}, false
	}
	wantSinger := strings.ToLower(strings.TrimSpace(singer))

	bestScore := 0
	var best neSong
	for _, it := range songs {
		name := cleanTitle(it.Name)
		if name == "" {
			continue
		}
		score := 0
		switch {
		case name == target:
			score = 12
		case strings.Contains(name, target), strings.Contains(target, name):
			if strict {
				// 宽松匹配只对音乐源开放：视频标题容易"包含"到一首真歌。
				continue
			}
			score = 4
		default:
			continue
		}
		artists := make([]string, 0, len(it.AR))
		for _, a := range it.AR {
			artists = append(artists, strings.ToLower(a.Name))
		}
		joined := strings.Join(artists, " ")
		if wantSinger != "" && strings.Contains(joined, wantSinger) {
			score += 6
		}
		low := strings.ToLower(it.Name)
		for _, w := range metaNoise {
			if strings.Contains(low, w) {
				score -= 3
			}
		}
		if score > bestScore {
			bestScore, best = score, it
		}
	}
	return best, bestScore > 0
}
