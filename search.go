package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 搜索结果里在线条目的来源标记。只出现在搜索结果列表的标题上，
// 目的是让用户一眼看出"这条不是本地曲库"。收藏、历史、歌词等出口一律用
// 干净标题，业务逻辑永远不吃这个标记。
const sourceTagMeow = "[meow] "

// TrackVO 是下发给飞牛客户端的曲目结构。
//
// 字段冗余到近乎啰嗦，这是刻意的：飞牛 Web 与 App 是两套前端，解构同一个
// 响应时取用的键名并不一致（guid/id、duration/duration_ms/durationMs、
// cover_url/coverUrl/coverURL 都有人用）。少给一个键，某一端就会显示空白，
// 而这在前端是完全看不出来的——只能靠把两端都覆盖来规避。
type TrackVO struct {
	GUID      string     `json:"guid"`
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Name      string     `json:"name"`
	Artist    string     `json:"artist"`
	Artists   []ArtistVO `json:"artists"`
	Album     AlbumVO    `json:"album"`
	AlbumName string     `json:"albumName"`

	AudioSpec AudioSpecVO `json:"audioSpec"`

	Duration   int     `json:"duration"`
	DurationMs int     `json:"duration_ms"`
	DurationMS int     `json:"durationMs"`
	DurationS  float64 `json:"duration_s"`

	Codec     string `json:"codec"`
	CodecName string `json:"codecName"`
	Format    string `json:"format"`
	Ext       string `json:"ext"`

	Size     int64 `json:"size"`
	FileSize int64 `json:"file_size"`

	CoverID   string `json:"coverId"`
	CoverURL  string `json:"cover_url"`
	CoverURL2 string `json:"coverUrl"`
	CoverURL3 string `json:"coverURL"`

	Source       string   `json:"source"`
	IsOnline     bool     `json:"is_online"`
	IsFavorite   bool     `json:"isFavorite"`
	IsCue        bool     `json:"isCue"`
	HasLyric     bool     `json:"hasLyric"`
	Genres       []string `json:"genres"`
	AccessStatus int      `json:"accessStatus"`
}

type ArtistVO struct {
	Name string `json:"name"`
	GUID string `json:"guid"`
}

type AlbumVO struct {
	Name    string     `json:"name"`
	GUID    string     `json:"guid"`
	Artists []ArtistVO `json:"artists"`
	CoverID string     `json:"coverId"`
}

type AudioSpecVO struct {
	Path      string `json:"path"`
	Format    string `json:"format"`
	Codec     string `json:"codec"`
	Container string `json:"container"`
	Duration  int    `json:"duration"`
	Size      int64  `json:"size"`
}

// buildTrackVO 把一条 Meow 搜索结果转成飞牛客户端的曲目结构。
//
// fakeID 是已经过 32-hex 伪装的 id：官方 App 会按 id 形态过滤条目，
// 非 32-hex 的 id 会被整条丢弃，所以这里只认伪装值。
func buildTrackVO(meowID, fakeID string, t MeowTrack) TrackVO {
	title := strings.TrimSpace(t.Title)
	artist := strings.TrimSpace(t.Artist)
	if title == "" {
		title = "未知曲目"
	}

	// audioSpec.path 必须是个像文件名的东西：客户端会拿它判断格式、
	// 也会拼进缓存路径。用 id 的哈希保证稳定且不含斜杠——
	// 带斜杠会被飞牛 1.1.19 误认为可在文件夹视图里定位的真实目录。
	sum := sha256.Sum256([]byte(meowID))
	specName := hex.EncodeToString(sum[:]) + ".mp3"

	durationS := 0
	if t.Duration > 0 {
		durationS = t.Duration
	}
	durationMs := durationS * 1000

	artists := make([]ArtistVO, 0, 1)
	if artist != "" {
		artists = append(artists, ArtistVO{Name: artist, GUID: fakeID + ":artist"})
	}

	return TrackVO{
		GUID: fakeID,
		ID:   fakeID,
		// 来源标记只在搜索结果里出现，因此这里直接带上；
		// 若日后要给"收藏列表"复用这个构造，需按出口区分。
		Title:   sourceTagMeow + title,
		Name:    sourceTagMeow + title,
		Artist:  artist,
		Artists: artists,
		Album: AlbumVO{
			Name:    "",
			GUID:    fakeID + ":album",
			Artists: artists,
			CoverID: fakeID,
		},
		AlbumName: "",
		AudioSpec: AudioSpecVO{
			Path:      specName,
			Format:    "mp3",
			Codec:     "mp3",
			Container: "mp3",
			Duration:  durationMs,
			Size:      0,
		},
		Duration:   durationMs,
		DurationMs: durationMs,
		DurationMS: durationMs,
		DurationS:  float64(durationS),
		Codec:      "mp3",
		CodecName:  "mp3",
		Format:     "mp3",
		Ext:        "mp3",
		CoverID:    fakeID,
		CoverURL:   t.CoverURL,
		CoverURL2:  t.CoverURL,
		CoverURL3:  t.CoverURL,
		Source:     "meow",
		IsOnline:   true,
		// 歌词按需拉取：Meow 有 /api/lyric，绝大多数曲目都能拿到；
		// 声明为 true 客户端才会显示歌词入口，真没词时端点返回空串。
		HasLyric:     true,
		Genres:       []string{},
		AccessStatus: 0,
	}
}

// searchResponse 只解析合并需要的最小字段，其余部分用 map 原样保留，
// 避免官方响应里我们不认识的字段在序列化时被丢掉。
type searchResponse struct {
	Code int            `json:"code"`
	Data map[string]any `json:"data"`
}

// officialList 从官方响应里找出曲目数组。
// 官方未必永远用 data.list，按常见键名兜底，全部没有就当作空列表。
func officialList(data map[string]any) []any {
	if data == nil {
		return nil
	}
	if v, ok := data["list"].([]any); ok {
		return v
	}
	for _, key := range []string{"items", "tracks", "records", "songs"} {
		if v, ok := data[key].([]any); ok {
			return v
		}
	}
	return nil
}

// trackKey 用于跨源去重。官方条目与在线条目字段名不同，统一取歌名+歌手。
func trackKey(item map[string]any) string {
	title := firstString(item, "title", "name")
	artist := ""
	switch v := item["artist"].(type) {
	case string:
		artist = v
	case map[string]any:
		artist = firstString(v, "name")
	}
	if artist == "" {
		if list, ok := item["artists"].([]any); ok && len(list) > 0 {
			switch a := list[0].(type) {
			case map[string]any:
				artist = firstString(a, "name")
			case string:
				artist = a
			}
		}
	}
	if artist == "" {
		artist = firstString(item, "albumName")
	}
	return strings.ToLower(strings.TrimSpace(title)) + "\x00" + strings.ToLower(strings.TrimSpace(artist))
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// handleSearchTrack 是 /music/api/v1/search/track 的实现。
//
// 原则：官方结果先拿到、先保证，在线结果只是追加。任何在线侧的失败
// （Meow 挂了、超时、限流）都不能让用户搜不到自己的本地曲库。
func (s *Server) handleSearchTrack(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(r.URL.Query().Get("q"))
	}
	// 没有关键词、或显式关闭在线搜索：完全走官方，一个字节都不改。
	if keyword == "" || !s.cfg.OnlineEnabled {
		s.upstream.ServeHTTP(w, r)
		return
	}

	// 分页请求只透传官方：在线结果固定挂在第一页，翻页时重复注入
	// 会让同一首歌在每一页都出现一次。
	if page, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && page > 1 {
		s.upstream.ServeHTTP(w, r)
		return
	}

	// 官方与 Meow 并发。官方失败就直接把它的响应原样交出去——
	// 那说明飞牛音乐本身有问题，此时伪造一个"只有在线结果"的响应
	// 只会让用户以为本地曲库被清空了。
	type upstreamResult struct {
		body   []byte
		header http.Header
		status int
		err    error
	}
	upCh := make(chan upstreamResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		path := r.URL.RequestURI()
		resp, err := s.upstream.Do(ctx, http.MethodGet, path, r.Header)
		if err != nil {
			upCh <- upstreamResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		upCh <- upstreamResult{body: body, header: resp.Header.Clone(), status: resp.StatusCode, err: err}
	}()

	// Meow 侧有自己的硬超时。搜索是同步接口，用户等不了 30 秒——
	// 宁可不给在线结果，也不能让搜索框转圈。
	type meowResult struct {
		tracks []MeowTrack
		err    error
	}
	meowCh := make(chan meowResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.SearchTimeout)*time.Second)
		defer cancel()
		tracks, err := s.meow.Search(ctx, keyword, s.cfg.SearchLimit, s.cfg.Sources)
		meowCh <- meowResult{tracks: tracks, err: err}
	}()

	up := <-upCh
	if up.err != nil || up.status != http.StatusOK {
		// 官方异常：原样透传（包括状态码与响应体）。
		if up.err != nil {
			status, msg := classifyUpstreamError(up.err)
			http.Error(w, msg, status)
			return
		}
		writeHeaderFrom(w, up.header)
		w.WriteHeader(up.status)
		w.Write(up.body)
		return
	}

	var parsed searchResponse
	if err := json.Unmarshal(up.body, &parsed); err != nil || parsed.Code != 0 {
		// 官方响应不是预期结构：不动它。
		writeHeaderFrom(w, up.header)
		w.WriteHeader(up.status)
		w.Write(up.body)
		return
	}

	meowRes := <-meowCh
	if meowRes.err != nil {
		// 在线搜索失败不影响本地结果，但要留下可诊断的日志。
		log.Printf("[search] Meow 搜索失败 keyword=%q: %v", keyword, meowRes.err)
	}

	// 先在合并前记下官方条数：合并会就地改写 parsed.Data["list"]，
	// 事后再数就只剩合计，日志会误导排障。
	officialCount := len(officialList(parsed.Data))
	merged, added := s.mergeOnline(parsed, meowRes.tracks)
	if added == 0 {
		writeHeaderFrom(w, up.header)
		w.WriteHeader(up.status)
		w.Write(up.body)
		return
	}

	out, err := json.Marshal(merged)
	if err != nil {
		writeHeaderFrom(w, up.header)
		w.WriteHeader(up.status)
		w.Write(up.body)
		return
	}
	log.Printf("[search] keyword=%q 官方 %d 条 + 在线新增 %d 条", keyword, officialCount, added)
	writeHeaderFrom(w, up.header)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(up.status)
	w.Write(out)
}

// mergeOnline 把在线结果追加到官方响应里，返回新响应与新增条数。
func (s *Server) mergeOnline(parsed searchResponse, tracks []MeowTrack) (searchResponse, int) {
	if parsed.Data == nil {
		parsed.Data = map[string]any{}
	}
	list := officialList(parsed.Data)
	if list == nil {
		list = []any{}
	}

	seen := make(map[string]struct{}, len(list)+len(tracks))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			seen[trackKey(m)] = struct{}{}
		}
	}

	added := 0
	for _, t := range tracks {
		if strings.TrimSpace(t.Title) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(t.Title)) + "\x00" + strings.ToLower(strings.TrimSpace(t.Artist))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		ref := trackToRef(t)
		realID := EncodeGUID(ref)
		fakeID := s.guids.Fake(realID)
		list = append(list, buildTrackVO(realID, fakeID, t))
		added++
	}
	if added == 0 {
		return parsed, 0
	}

	parsed.Data["list"] = list
	// total 要与实际下发条数一致，否则客户端的"共 N 首"和列表长度对不上，
	// 滚动到底也不会再触发加载。
	total := 0
	switch v := parsed.Data["total"].(type) {
	case float64:
		total = int(v)
	case int:
		total = v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			total = int(n)
		}
	}
	if total < len(list) {
		parsed.Data["total"] = len(list)
	} else {
		parsed.Data["total"] = total + added
	}
	return parsed, added
}

// writeHeaderFrom 复制上游响应头，剔除会让 Go 的 ResponseWriter 行为错乱的几个。
func writeHeaderFrom(w http.ResponseWriter, src http.Header) {
	for k, vals := range src {
		switch strings.ToLower(k) {
		case "content-length", "content-encoding", "transfer-encoding", "connection":
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
}
