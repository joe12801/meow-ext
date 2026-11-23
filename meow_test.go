package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMeowSearchParsesTracks(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			t.Errorf("请求路径不对：%s", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"title":"晴天","artist":"周杰伦","url":"http://h/stream_live?song=x&singer=y","source":"kuwo","duration":269},
			{"title":"夜曲","artist":"周杰伦","url":"http://h/stream_live?song=a&singer=b&bv=BV1xx","source":"bilibili","duration":0}
		]`)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	c := NewMeowClient(cfg)

	tracks, err := c.Search(context.Background(), "晴天", 10, []string{"kuwo", "bilibili"})
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if gotQuery != "晴天" {
		t.Fatalf("query 未透传：%q", gotQuery)
	}
	if len(tracks) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d", len(tracks))
	}
	if tracks[0].Title != "晴天" || tracks[0].Source != "kuwo" || tracks[0].Duration != 269 {
		t.Fatalf("字段解析错误：%+v", tracks[0])
	}
}

func TestMeowSearchPropagatesUpstreamErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "guest quota exhausted")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	_, err := NewMeowClient(cfg).Search(context.Background(), "晴天", 10, nil)
	if err == nil {
		t.Fatal("上游 429 应作为错误返回")
	}
	if meowErrStatus(err) != http.StatusTooManyRequests {
		t.Fatalf("429 应原样映射，实际 %d", meowErrStatus(err))
	}
}

func TestMeowSearchReportsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>not json</html>")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	_, err := NewMeowClient(cfg).Search(context.Background(), "晴天", 10, nil)
	if err == nil {
		t.Fatal("非 JSON 响应应报错（通常意味着地址指向了另一个服务）")
	}
}

func TestMeowEmptyQuerySkipsRequest(t *testing.T) {
	// 空关键词不该打上游：Meow 在 query 与 artist 都为空时返回空数组，
	// 但飞牛的搜索框清空时会频繁触发，没必要每次都走一次网络。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("空关键词不应发起请求")
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	tracks, err := NewMeowClient(cfg).Search(context.Background(), "  ", 10, nil)
	if err != nil || tracks != nil {
		t.Fatalf("空关键词应直接返回空，got %v err=%v", tracks, err)
	}
}

func TestMeowStreamForwardsRange(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if r.URL.Path != "/stream_live" {
			t.Errorf("取流路径不对：%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "ID3x")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	resp, err := NewMeowClient(cfg).Stream(context.Background(), "GET", "/stream_live?song=a&singer=b", "bytes=0-3")
	if err != nil {
		t.Fatalf("取流失败：%v", err)
	}
	defer resp.Body.Close()
	if gotRange != "bytes=0-3" {
		t.Fatalf("Range 未透传到上游：%q", gotRange)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("206 应原样返回，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Range") != "bytes 0-3/100" {
		t.Fatalf("Content-Range 丢失：%q", resp.Header.Get("Content-Range"))
	}
}

func TestMeowStreamForwardsUpstream4xx(t *testing.T) {
	// 4xx 不能被包装成错误。上游那句"游客每日试听 20 首已用完，登录后不限"
	// 是用户唯一看得懂的解释，Retry-After 决定客户端该等一小时还是换源——
	// 自己编一句错误会把这些全丢掉，用户只看到一个没头没尾的 502。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("X-Guest-Quota", "exhausted")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "游客每日试听 20 首已用完，登录后不限")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	resp, err := NewMeowClient(cfg).Stream(context.Background(), "GET",
		"/stream_live?song=a&singer=b", "")
	if err != nil {
		t.Fatalf("4xx 应当原样返回而不是报错：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("429 应原样返回，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "3600" {
		t.Fatalf("Retry-After 丢失：%q", resp.Header.Get("Retry-After"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "登录后不限") {
		t.Fatalf("上游提示丢失：%q", body)
	}
}

func TestMeowStreamTreats5xxAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream boom")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	_, err := NewMeowClient(cfg).Stream(context.Background(), "GET",
		"/stream_live?song=a&singer=b", "")
	if err == nil {
		t.Fatal("上游 5xx 应作为错误返回")
	}
	if meowErrStatus(err) != http.StatusBadGateway {
		t.Fatalf("5xx 应映射成 502，实际 %d", meowErrStatus(err))
	}
	if !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("错误信息未包含上游原因：%v", err)
	}
}

func TestMeowStreamForwardsHeadAsHead(t *testing.T) {
	// HEAD 必须原样发出去。Meow 的游客额度只对"从头开始的那次请求"计数，
	// 若把 HEAD 探长改成 GET，每探一次就白白消耗一首，用户还没听到声音
	// 今日 20 首就被探没了。
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Length", "762765")
		w.Header().Set("Content-Type", "audio/mpeg")
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	resp, err := NewMeowClient(cfg).Stream(context.Background(), http.MethodHead,
		"/stream_live?song=a&singer=b", "")
	if err != nil {
		t.Fatalf("HEAD 取流失败：%v", err)
	}
	defer resp.Body.Close()
	if gotMethod != http.MethodHead {
		t.Fatalf("客户端 HEAD 被改写成了 %s", gotMethod)
	}
	if resp.Header.Get("Content-Length") != "762765" {
		t.Fatalf("Content-Length 丢失：%q", resp.Header.Get("Content-Length"))
	}
}

func TestMeowStreamRequiresPath(t *testing.T) {
	cfg := DefaultConfig()
	if _, err := NewMeowClient(cfg).Stream(context.Background(), "GET", "", ""); err == nil {
		t.Fatal("空地址应报错")
	}
}

func TestMeowLyricTreats404AsEmpty(t *testing.T) {
	// "这首纯音乐没有歌词"是正常状态，不该让客户端看到错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	text, err := NewMeowClient(cfg).Lyric(context.Background(), "纯音乐", "")
	if err != nil {
		t.Fatalf("404 不应当成错误：%v", err)
	}
	if text != "" {
		t.Fatalf("404 应返回空歌词，实际 %q", text)
	}
}

func TestMeowLyricReturnsLRC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "lrc" {
			t.Errorf("应请求 lrc 格式")
		}
		io.WriteString(w, "[00:01.00]故事的小黄花")
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	text, err := NewMeowClient(cfg).Lyric(context.Background(), "晴天", "周杰伦")
	if err != nil {
		t.Fatalf("取歌词失败：%v", err)
	}
	if !strings.Contains(text, "故事的小黄花") {
		t.Fatalf("歌词内容不对：%q", text)
	}
}

func TestMeowHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer ok.Close()
	cfg := DefaultConfig()
	cfg.MeowURL = ok.URL
	if err := NewMeowClient(cfg).Health(context.Background()); err != nil {
		t.Fatalf("健康检查应通过：%v", err)
	}

	// 地址指到一个根本不是 Meow 的服务（比如路由器后台）时必须判失败，
	// 否则管理页会显示"已连接"，用户以为配好了。
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello world")
	}))
	defer other.Close()
	cfg.MeowURL = other.URL
	if err := NewMeowClient(cfg).Health(context.Background()); err == nil {
		t.Fatal("非 Meow 服务应判为不可用")
	}
}

func TestMeowHealthReportsUnreachable(t *testing.T) {
	cfg := DefaultConfig()
	// 127.0.0.1:1 基本不会有人监听，用来模拟"地址填错/服务没起来"。
	cfg.MeowURL = "http://127.0.0.1:1"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := NewMeowClient(cfg).Health(ctx)
	if err == nil {
		t.Fatal("不可达地址应报错")
	}
	if !strings.Contains(err.Error(), "无法连接") {
		t.Fatalf("错误信息应明确是连不上，实际：%v", err)
	}
}

func TestMeowTokenIsSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, "[]")
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.MeowURL = srv.URL
	cfg.MeowToken = "secret-token"
	NewMeowClient(cfg).Search(context.Background(), "x", 5, nil)
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("令牌未带上：%q", gotAuth)
	}
}
