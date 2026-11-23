package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 搜索结果是给人看的：时长超过上限的视频必须在这一步就丢掉，
// 否则用户点了才等到 mp3api 用"超过最大时长"把他拒掉。
func TestYoutubeSearchFiltersLongVideos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("q"); got != "晴天" {
			t.Errorf("搜索词应为 晴天，实际 %q", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "k123" {
			t.Errorf("X-API-Key 未带出：%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query": "晴天",
			"count": 3,
			"results": []map[string]any{
				{"id": "aaa", "title": "晴天", "channel": "周杰伦", "duration": 269, "thumbnail": "https://img/1.jpg"},
				{"id": "bbb", "title": "晴天 合集 3 小时", "channel": "某人", "duration": 10800},
				{"id": "ccc", "title": "晴天 翻唱", "channel": "翻唱歌手", "duration": 240},
			},
		})
	}))
	defer srv.Close()

	c := NewYouTubeClient(Config{YoutubeAPIURL: srv.URL, YoutubeAPIKey: "k123", YoutubeBitrate: 192, YoutubeMaxDuration: 900})
	tracks, err := c.Search(context.Background(), "晴天", 8)
	if err != nil {
		t.Fatalf("Search 出错：%v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("超长视频应被过滤，期望 2 条，实际 %d 条", len(tracks))
	}
	if tracks[0].Source != youtubeSourceKey {
		t.Fatalf("来源应为 youtube，实际 %q", tracks[0].Source)
	}
	if tracks[0].Artist != "周杰伦" {
		t.Fatalf("频道应映射到 artist，实际 %q", tracks[0].Artist)
	}
	if tracks[0].Duration != 269 {
		t.Fatalf("时长未带出：%d", tracks[0].Duration)
	}
	// 播放地址要能被 trackToRef 解析出 XID（video id），否则取流会走错链路。
	ref := trackToRef(tracks[0])
	if ref.XID != "aaa" {
		t.Fatalf("trackToRef 未解析出 video id，实际 xid=%q path=%q", ref.XID, ref.Path)
	}
	if ref.Source != youtubeSourceKey {
		t.Fatalf("ref.Source 应为 youtube，实际 %q", ref.Source)
	}
}

// 上限为 0 表示不过滤：有人就是要听长播客。
func TestYoutubeSearchKeepsLongVideosWhenUnlimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"id": "x", "title": "长播客", "channel": "主播", "duration": 7200},
		}})
	}))
	defer srv.Close()

	c := NewYouTubeClient(Config{YoutubeAPIURL: srv.URL, YoutubeMaxDuration: 0})
	tracks, err := c.Search(context.Background(), "播客", 5)
	if err != nil {
		t.Fatalf("Search 出错：%v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("不限时长时应保留，实际 %d 条", len(tracks))
	}
}

// 取流必须把 Range 原样带过去，且 method 跟着客户端走（HEAD 不能被改成 GET）。
func TestYoutubeStreamForwardsRangeAndMethod(t *testing.T) {
	var gotRange, gotMethod, gotVideo, gotBitrate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		gotMethod = r.Method
		gotVideo = r.URL.Query().Get("video_id")
		gotBitrate = r.URL.Query().Get("bitrate")
		w.Header().Set("Content-Range", "bytes 1000-1999/763953")
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("xx"))
	}))
	defer srv.Close()

	c := NewYouTubeClient(Config{YoutubeAPIURL: srv.URL, YoutubeBitrate: 256})
	resp, err := c.Stream(context.Background(), http.MethodHead, "DYptgVvkVLQ", "bytes=1000-1999")
	if err != nil {
		t.Fatalf("Stream 出错：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("应原样返回 206，实际 %d", resp.StatusCode)
	}
	if gotRange != "bytes=1000-1999" {
		t.Fatalf("Range 未透传：%q", gotRange)
	}
	if gotMethod != http.MethodHead {
		t.Fatalf("HEAD 被改写成了 %s", gotMethod)
	}
	if gotVideo != "DYptgVvkVLQ" || gotBitrate != "256" {
		t.Fatalf("video_id/bitrate 不对：%q %q", gotVideo, gotBitrate)
	}
}

// 5xx 才算故障；4xx 要原样交给客户端（里面是服务端给的解释）。
func TestYoutubeStreamPasses4xxButWraps5xx(t *testing.T) {
	code := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"detail":"视频时长超过上限"}`))
	}))
	defer srv.Close()
	c := NewYouTubeClient(Config{YoutubeAPIURL: srv.URL})

	resp, err := c.Stream(context.Background(), http.MethodGet, "abc", "")
	if err != nil {
		t.Fatalf("4xx 不应变成错误：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("应保留 400，实际 %d", resp.StatusCode)
	}

	code = http.StatusBadGateway
	if _, err := c.Stream(context.Background(), http.MethodGet, "abc", ""); err == nil {
		t.Fatal("5xx 应作为错误返回")
	}
}

func TestLinesToLRCSortsAndFormats(t *testing.T) {
	lrc := linesToLRC([]ytLine{
		{Start: 65.5, Text: "第二句"},
		{Start: 3.2, Text: "第一句"},
		{Start: 10, Text: "  "}, // 空行丢掉
	})
	want := "[00:03.20]第一句\n[01:05.50]第二句\n"
	if lrc != want {
		t.Fatalf("LRC 不符\n期望：%q\n实际：%q", want, lrc)
	}
}

func TestYoutubeLyricUnavailableReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/lyrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"available":false,"lines":[]}`))
	}))
	defer srv.Close()

	c := NewYouTubeClient(Config{YoutubeAPIURL: srv.URL})
	text, err := c.Lyric(context.Background(), "abc")
	if err != nil {
		t.Fatalf("没有歌词不该报错：%v", err)
	}
	if text != "" {
		t.Fatalf("没有歌词应返回空串，实际 %q", text)
	}
}

// youtube 必须从要发给 Meow 的 source 列表里摘掉：
// 混进去会让 Meow 静默返回空数组，连带把其他源也拖成 0 条。
func TestSplitYoutubeSource(t *testing.T) {
	meow, on := splitYoutubeSource([]string{"kuwo", "YouTube", "netease"})
	if !on {
		t.Fatal("应识别出 youtube（大小写不敏感）")
	}
	if strings.Join(meow, ",") != "kuwo,netease" {
		t.Fatalf("剩余音源不对：%v", meow)
	}

	meow, on = splitYoutubeSource([]string{"youtube"})
	if !on || len(meow) != 0 {
		t.Fatalf("只勾 youtube 时应得到空 Meow 列表：%v %v", meow, on)
	}

	meow, on = splitYoutubeSource(nil)
	if on || len(meow) != 0 {
		t.Fatalf("空列表不应启用 youtube：%v %v", meow, on)
	}
}

func TestInterleave(t *testing.T) {
	a := []MeowTrack{{Title: "a1"}, {Title: "a2"}, {Title: "a3"}}
	b := []MeowTrack{{Title: "b1"}}
	got := interleave(a, b)
	want := []string{"a1", "b1", "a2", "a3"}
	if len(got) != len(want) {
		t.Fatalf("长度不符：%d", len(got))
	}
	for i := range want {
		if got[i].Title != want[i] {
			t.Fatalf("第 %d 条应为 %s，实际 %s", i, want[i], got[i].Title)
		}
	}
}

// 未配置地址时整个 YouTube 分支必须消失，不能因为多一个音源键就报错。
func TestYoutubeDisabledWithoutURL(t *testing.T) {
	if NewYouTubeClient(Config{}) != nil {
		t.Fatal("未配置地址时应返回 nil")
	}
}
