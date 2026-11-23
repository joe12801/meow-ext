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
	//
	// 标题保持干净：以前在标题前加 "[meow] " 当来源标记，既难看又占宽度，
	// 现在来源改由封面位上的音源 LOGO 表达（见 cover_logo.go）。
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
		Title:   title,
		Name:    title,
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

// splitYoutubeSource 从 sources 里摘出 youtube，剩下的原样交给 Meow。
//
// youtube 不是 Meow 的音源：把它混进 source= 参数，Meow 会返回空数组
// （和传逗号列表一样是静默失败），所以必须在发请求前就摘掉。
func splitYoutubeSource(sources []string) (meow []string, youtube bool) {
	for _, v := range sources {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.EqualFold(v, youtubeSourceKey) {
			youtube = true
			continue
		}
		meow = append(meow, v)
	}
	return meow, youtube
}

// interleave 交替合并两个列表，YouTube 与 Meow 各占一半开头。
//
// 直接 a + b 拼接会让 Youtube 永远排在官方和 Meow 之后，用户翻不到；
// 交替排能让两边都在第一屏出现，但各自的数量仍由配额决定。
func interleave(a, b []MeowTrack) []MeowTrack {
	out := make([]MeowTrack, 0, len(a)+len(b))
	for i := 0; i < len(a) || i < len(b); i++ {
		if i < len(a) {
			out = append(out, a[i])
		}
		if i < len(b) {
			out = append(out, b[i])
		}
	}
	return out
}

// searchOnline 汇总所有在线源：Meow 的多音源，加上可选的 YouTube。
//
// 两者并发：YouTube 走公网 + yt-dlp，比本机 Meow 慢得多，串行执行会把
// 整个搜索拖到超时。任一侧失败都不影响另一侧——用户至少能搜到东西。
func (s *Server) searchOnline(ctx context.Context, keyword string) ([]MeowTrack, error) {
	cfg := s.Config()
	meowSources, ytOn := splitYoutubeSource(cfg.Sources)
	yt := s.youtubeClient()
	if yt == nil {
		ytOn = false
	}
	limit := cfg.SearchLimit
	if limit <= 0 {
		limit = 30
	}

	if !ytOn {
		return s.meowClient().Search(ctx, keyword, limit, meowSources)
	}

	// YouTube 与 Meow 分名额：不分的话，YouTube 拿 limit 条、Meow 也拿 limit 条，
	// 列表会翻倍且以 YouTube 的长标题为主，国内歌反而被挤到后面。
	n := len(meowSources) + 1
	ytQuota := limit / n
	if ytQuota < 4 {
		ytQuota = 4
	}
	if ytQuota > 12 {
		ytQuota = 12
	}
	meowLimit := limit - ytQuota
	if meowLimit < 6 {
		meowLimit = 6
	}

	type ytResult struct {
		tracks []MeowTrack
		err    error
	}
	ytCh := make(chan ytResult, 1)
	go func() {
		tracks, err := yt.Search(ctx, keyword, ytQuota)
		ytCh <- ytResult{tracks: tracks, err: err}
	}()

	var (
		meowTracks []MeowTrack
		meowErr    error
	)
	// 只勾了 youtube：跳过 Meow。否则 meowSources 为空会被理解成
	// "不传 source = 用 Meow 的全部音源"，等于把用户没勾的源也搜了。
	if len(cfg.Sources) > 0 && len(meowSources) == 0 {
		// 不请求 Meow。
	} else {
		meowTracks, meowErr = s.meowClient().Search(ctx, keyword, meowLimit, meowSources)
	}

	res := <-ytCh
	if res.err != nil {
		log.Printf("[search] YouTube 搜索失败 keyword=%q: %v", keyword, res.err)
	}
	if meowErr != nil {
		log.Printf("[search] Meow 搜索失败 keyword=%q: %v", keyword, meowErr)
	}
	out := interleave(meowTracks, res.tracks)
	if len(out) > 0 {
		log.Printf("[search] %q 在线合计 %d 条（Meow %d + YouTube %d）",
			keyword, len(out), len(meowTracks), len(res.tracks))
	}
	if len(out) == 0 && meowErr != nil {
		return nil, meowErr
	}
	return out, nil
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
	type searchResult struct {
		tracks []MeowTrack
		err    error
	}
	ch := make(chan searchResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.SearchTimeout)*time.Second)
		defer cancel()
		tracks, err := s.searchOnline(ctx, keyword)
		ch <- searchResult{tracks: tracks, err: err}
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
	parseErr := json.Unmarshal(up.body, &parsed)

	onlineRes := <-ch
	if onlineRes.err != nil {
		// 在线搜索失败不影响本地结果，但要留下可诊断的日志。
		log.Printf("[search] 在线搜索失败 keyword=%q: %v", keyword, onlineRes.err)
	}

	if parseErr != nil || parsed.Code != 0 {
		// 官方这次没给出可用结果：可能是 token 失效（实测有的机器上
		// /search/track 会返回 {"code":99999,"msg":"INVALID TOKEN","data":null}），
		// 也可能是接口结构变了。这时若照旧透传官方 body，在线结果会被
		// 一并丢掉，用户看到的是"官方空 + 在线也没了"的双重空列表。
		// 所以只要在线侧有结果，就自己组装一个 code=0 的响应顶上。
		if !s.assembleOnlineOnly(w, up.header, onlineRes.tracks, keyword, parsed.Code, up.status) {
			writeHeaderFrom(w, up.header)
			w.WriteHeader(up.status)
			w.Write(up.body)
		}
		return
	}

	// 先在合并前记下官方条数：合并会就地改写 parsed.Data["list"]，
	// 事后再数就只剩合计，日志会误导排障。
	officialCount := len(officialList(parsed.Data))
	merged, added := s.mergeOnline(parsed, onlineRes.tracks)
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

// assembleOnlineOnly 在官方搜索不可用时，用在线结果单独组一个正常响应。
//
// 返回 false 表示没有可下发的在线结果，调用方应原样透传官方响应。
// 退化的做法（透传官方 body）在这里是错的：用户搜不到东西时，
// 在线源是他唯一能用的出口，不能因为官方 token 掉了就一起沉默。
func (s *Server) assembleOnlineOnly(w http.ResponseWriter, header http.Header,
	tracks []MeowTrack, keyword string, officialCode int, officialStatus int) bool {
	list := make([]any, 0, len(tracks))
	seen := make(map[string]struct{}, len(tracks))
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
	}
	if len(list) == 0 {
		return false
	}

	// 分页字段给足几种叫法：前端取 total 还是 totalCount 无从确认（两套前端
	// 各自实现），少一个就可能导致"共 0 首"或滚不到底。
	data := map[string]any{
		"list":       list,
		"total":      len(list),
		"totalCount": len(list),
		"page":       1,
		"pageSize":   len(list),
		"hasMore":    false,
	}
	out, err := json.Marshal(searchResponse{Code: 0, Data: data})
	if err != nil {
		return false
	}
	log.Printf("[search] keyword=%q 官方不可用（code=%d status=%d），仅下发在线 %d 条",
		keyword, officialCode, officialStatus, len(list))
	writeHeaderFrom(w, header)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	return true
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
