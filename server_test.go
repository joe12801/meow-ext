package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// integrationEnv 造一套"假官方 + 假 Meow + 真 Server"，用于验证端到端行为。
type integrationEnv struct {
	server      *Server
	official    *http.Server
	officialHit []string
	meow        *httptest.Server
	meowHits    []string
	mu          sync.Mutex
}

func (e *integrationEnv) recordOfficial(path string) {
	e.mu.Lock()
	e.officialHit = append(e.officialHit, path)
	e.mu.Unlock()
}

func (e *integrationEnv) officialCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.officialHit...)
}

func (e *integrationEnv) recordMeow(path string) {
	e.mu.Lock()
	e.meowHits = append(e.meowHits, path)
	e.mu.Unlock()
}

func (e *integrationEnv) meowCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.meowHits...)
}

func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()
	dir := shortDir(t)
	env := &integrationEnv{}

	// 假官方：搜索返回一条本地曲目，其余接口原样回显路径。
	sockPath := filepath.Join(dir, "official.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("建立假官方失败：%v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		env.recordOfficial(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/music/api/v1/search/track") {
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"list": []any{
						map[string]any{"guid": "aaaa1111", "title": "晴天", "artist": "周杰伦"},
					},
					"total": float64(1),
				},
			})
			return
		}
		w.Write([]byte(`{"code":0,"from":"official","path":"` + r.URL.Path + `"}`))
	})
	env.official = &http.Server{Handler: mux}
	go env.official.Serve(l)
	t.Cleanup(func() { env.official.Close() })

	// 假 Meow：搜索返回两条，其中一条与官方重复。
	env.meow = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.recordMeow(r.URL.RequestURI())
		switch r.URL.Path {
		case "/healthz":
			io.WriteString(w, "ok")
		case "/api/search":
			json.NewEncoder(w).Encode([]MeowTrack{
				{Title: "晴天", Artist: "周杰伦", Source: "kuwo", URL: "http://h/stream_live?song=x&singer=y"},
				{Title: "夜曲", Artist: "周杰伦", Source: "netease", URL: "http://h/stream_live?song=a&singer=b&bv=BV1TEST", Duration: 200},
			})
		case "/api/lyric":
			io.WriteString(w, "[00:00.00]测试歌词")
		case "/stream_live":
			// song=QUOTA 用来模拟 Meow 的游客额度闸门：
			// 它返回 429 + 一句人话 + Retry-After，这些都必须原样到达客户端。
			if strings.Contains(r.URL.RawQuery, "song=QUOTA") {
				w.Header().Set("Retry-After", "3600")
				w.Header().Set("X-Guest-Quota", "exhausted")
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, "游客每日试听 20 首已用完，登录后不限")
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Accept-Ranges", "bytes")
			io.WriteString(w, "MP3DATA")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(env.meow.Close)

	cfg := DefaultConfig()
	cfg.UpstreamSocket = sockPath
	cfg.MeowURL = env.meow.URL
	cfg.DataDir = dir
	cfg.StateDir = dir
	env.server = NewServer(cfg, filepath.Join(dir, "config.json"))
	return env
}

func (e *integrationEnv) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSearchMergesOfficialAndOnline(t *testing.T) {
	env := newIntegrationEnv(t)
	rec := env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天&page=1&size=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Code int `json:"code"`
		Data struct {
			List  []TrackVO `json:"list"`
			Total int       `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期结构：%v body=%s", err, rec.Body.String())
	}
	if len(body.Data.List) != 2 {
		t.Fatalf("应合并成 2 条（官方 1 + 在线 1，去重 1），实际 %d：%s", len(body.Data.List), rec.Body.String())
	}
	// 官方条目必须排在最前面：本地曲库优先是用户预期。
	if body.Data.List[0].GUID != "aaaa1111" {
		t.Fatalf("官方条目应排首位，实际 %+v", body.Data.List[0])
	}
	// 在线条目 id 要伪装；标题不加来源前缀（来源由封面 LOGO 表达）。
	online := body.Data.List[1]
	if online.Title != "夜曲" {
		t.Fatalf("在线条目标题应保持干净：%q", online.Title)
	}
	if len(online.GUID) != 32 {
		t.Fatalf("在线 id 未伪装成 32-hex：%q", online.GUID)
	}
	if body.Data.Total != 2 {
		t.Fatalf("total 应等于列表长度，实际 %d", body.Data.Total)
	}
}

func TestSearchWithoutKeywordIsPurePassthrough(t *testing.T) {
	env := newIntegrationEnv(t)
	rec := env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=&page=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	// 没有关键词时 Meow 不应被调用。
	if calls := env.meowCalls(); len(calls) != 0 {
		t.Fatalf("空关键词不应请求 Meow，实际：%v", calls)
	}
	if calls := env.officialCalls(); len(calls) != 1 {
		t.Fatalf("应只请求官方一次，实际：%v", calls)
	}
}

func TestSearchPage2IsPassthrough(t *testing.T) {
	env := newIntegrationEnv(t)
	env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天&page=2&size=50")
	// 翻页时重复注入会让同一首歌每页都出现一次。
	if calls := env.meowCalls(); len(calls) != 0 {
		t.Fatalf("第二页不应请求 Meow，实际：%v", calls)
	}
}

func TestSearchSurvivesMeowOutage(t *testing.T) {
	env := newIntegrationEnv(t)
	env.meow.Close() // 在线源整个挂掉

	rec := env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天&page=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("Meow 挂掉时仍应返回官方结果，实际状态码 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "aaaa1111") {
		t.Fatalf("官方结果丢失：%s", rec.Body.String())
	}
}

func TestSearchDisabledPassesThrough(t *testing.T) {
	env := newIntegrationEnv(t)
	env.server.cfg.OnlineEnabled = false
	rec := env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天&page=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if calls := env.meowCalls(); len(calls) != 0 {
		t.Fatalf("关闭在线搜索后不应请求 Meow，实际：%v", calls)
	}
}

func TestStreamReplaysBVToMeow(t *testing.T) {
	env := newIntegrationEnv(t)

	// 先搜一次拿到下发给客户端的假 id——这正是真实客户端的完整流程。
	rec := env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天&page=1")
	var body struct {
		Data struct {
			List []TrackVO `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data.List) < 2 {
		t.Fatalf("搜索结果不足：%s", rec.Body.String())
	}
	// 第二条是在线条目（带 bv）。
	online := body.Data.List[1]

	env.mu.Lock()
	env.meowHits = nil
	env.mu.Unlock()

	streamRec := env.do(t, http.MethodGet, "/music/api/v1/track/stream?guid="+online.GUID)
	if streamRec.Code != http.StatusOK {
		t.Fatalf("取流状态码 %d body=%s", streamRec.Code, streamRec.Body.String())
	}
	if streamRec.Body.String() != "MP3DATA" {
		t.Fatalf("音频字节未透传：%q", streamRec.Body.String())
	}
	if ct := streamRec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type 丢失：%q", ct)
	}

	calls := env.meowCalls()
	if len(calls) != 1 {
		t.Fatalf("应只向 Meow 取流一次，实际 %v", calls)
	}
	// 核心断言：id 里携带的 bv 必须原样回到上游。
	// 丢了它，Meow 只能按歌名重新解析，用户点的是这条视频，响的可能是另一首同名歌。
	if !strings.Contains(calls[0], "bv=BV1TEST") {
		t.Fatalf("取流未携带 bv：%s", calls[0])
	}
	if !strings.Contains(calls[0], "song=") || !strings.Contains(calls[0], "singer=") {
		t.Fatalf("取流缺少歌名/歌手：%s", calls[0])
	}
}

func TestStreamRangeIsForwarded(t *testing.T) {
	env := newIntegrationEnv(t)
	reg := env.server.guids
	ref := MeowRef{Song: "晴天", Singer: "周杰伦"}
	fake := reg.Fake(EncodeGUID(ref))

	req := httptest.NewRequest(http.MethodGet, "/music/api/v1/track/stream?guid="+fake, nil)
	req.Header.Set("Range", "bytes=100-200")
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if !strings.Contains(strings.Join(env.meowCalls(), " "), "/stream_live") {
		t.Fatalf("未向 Meow 取流：%v", env.meowCalls())
	}
}

func TestStreamForwardsGuestQuotaRejection(t *testing.T) {
	env := newIntegrationEnv(t)
	ref := MeowRef{Song: "QUOTA", Singer: "测试"}
	fake := env.server.guids.Fake(EncodeGUID(ref))

	rec := env.do(t, http.MethodGet, "/music/api/v1/track/stream?guid="+fake)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("额度用尽应原样返回 429，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "3600" {
		t.Fatalf("Retry-After 未透传：%q", got)
	}
	if got := rec.Header().Get("X-Guest-Quota"); got != "exhausted" {
		t.Fatalf("X-Guest-Quota 未透传：%q", got)
	}
	if !strings.Contains(rec.Body.String(), "登录后不限") {
		t.Fatalf("上游提示丢失：%s", rec.Body.String())
	}
	// 说明文字不能贴 audio/mpeg，否则客户端会当音频解出一堆噪音。
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "audio/") {
		t.Fatalf("429 的 Content-Type 不应是音频：%q", ct)
	}
}

func TestStreamUnknownGUIDFallsBackToOfficial(t *testing.T) {
	env := newIntegrationEnv(t)
	// 官方曲目的 32-hex id 必须原样交给官方后端，
	// 不能被扩展误认领后送到 Meow 去。
	rec := env.do(t, http.MethodGet, "/music/api/v1/track/stream?guid=5f2d1c0e9b8a7d6c5b4a39281706f5e4")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "official") {
		t.Fatalf("应透传到官方：%s", rec.Body.String())
	}
	if calls := env.meowCalls(); len(calls) != 0 {
		t.Fatalf("官方 id 不应打到 Meow：%v", calls)
	}
}

func TestLyricsReturnsLRC(t *testing.T) {
	env := newIntegrationEnv(t)
	fake := env.server.guids.Fake(EncodeGUID(MeowRef{Song: "晴天", Singer: "周杰伦"}))
	rec := env.do(t, http.MethodGet, "/music/api/v1/track/lyrics?guid="+fake)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "测试歌词") {
		t.Fatalf("歌词未返回：%s", rec.Body.String())
	}
	// 歌词字段名要给全：不同客户端读 lrc，也有读 lyric 的。
	if !strings.Contains(rec.Body.String(), `"lrc"`) || !strings.Contains(rec.Body.String(), `"lyric"`) {
		t.Fatalf("歌词字段不完整：%s", rec.Body.String())
	}
}

func TestLyricsForOfficialGUIDPassesThrough(t *testing.T) {
	env := newIntegrationEnv(t)
	rec := env.do(t, http.MethodGet, "/music/api/v1/track/lyrics?guid=5f2d1c0e9b8a7d6c5b4a39281706f5e4")
	if !strings.Contains(rec.Body.String(), "official") {
		t.Fatalf("官方歌词请求应透传：%s", rec.Body.String())
	}
}

func TestOtherPathsPassThrough(t *testing.T) {
	env := newIntegrationEnv(t)
	for _, path := range []string{
		"/music/api/v1/playlist/list",
		"/music/api/v1/favorite-track/list",
		"/music/api/v1/album?guid=x",
	} {
		rec := env.do(t, http.MethodGet, path)
		if !strings.Contains(rec.Body.String(), "official") {
			t.Fatalf("%s 应透传到官方，实际：%s", path, rec.Body.String())
		}
	}
}

func TestHealthReportsBothSides(t *testing.T) {
	env := newIntegrationEnv(t)
	rec := env.do(t, http.MethodGet, "/_ext/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("健康检查应返回 200（Meow 断线也算扩展可用），实际 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	up, _ := body["upstream"].(map[string]any)
	if up["ok"] != true {
		t.Fatalf("官方侧应正常：%v", up)
	}
	meow, _ := body["meow"].(map[string]any)
	if meow["ok"] != true {
		t.Fatalf("Meow 侧应正常：%v", meow)
	}
}
