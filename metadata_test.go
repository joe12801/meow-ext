package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 播放链路的前置条件：客户端点播前会先拿 metadata，拿不到就直接中止，
// 连 /track/stream 都不会请求。这里锁住"必须是自家 id 才应答、应答结构对得上"。
func TestTrackMetadataForMeowGUID(t *testing.T) {
	env := newIntegrationEnv(t)

	ref := MeowRef{Song: "夜曲", Singer: "周杰伦", Source: "kuwo", Duration: 260}
	realID := EncodeGUID(ref)
	fakeID := env.server.guids.Fake(realID)

	rec := env.do(t, http.MethodGet, "/music/api/v1/track/metadata?guid="+fakeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Code int `json:"code"`
		Data struct {
			Track struct {
				GUID     string `json:"guid"`
				Title    string `json:"title"`
				Duration int    `json:"duration"`
				Artists  []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"track"`
			AudioSpec struct {
				Path      string `json:"path"`
				Container string `json:"container"`
				Duration  int    `json:"duration"`
			} `json:"audioSpec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期结构：%v body=%s", err, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("code 应为 0，实际 %d", body.Code)
	}
	// 必须是客户端拿到的伪 id：前端会把 track.guid 当播放用的 track.id，
	// 换成真 id 后续取流就查不到映射了。
	if body.Data.Track.GUID != fakeID {
		t.Fatalf("track.guid 应回原伪 id，实际 %q", body.Data.Track.GUID)
	}
	if body.Data.Track.Title != "夜曲" {
		t.Fatalf("标题不对：%q", body.Data.Track.Title)
	}
	if body.Data.Track.Duration != 260000 {
		t.Fatalf("时长应为毫秒（260000），实际 %d", body.Data.Track.Duration)
	}
	if len(body.Data.Track.Artists) != 1 || body.Data.Track.Artists[0].Name != "周杰伦" {
		t.Fatalf("歌手不对：%+v", body.Data.Track.Artists)
	}
	// 前端用 audioSpec.path 取扩展名、拼缓存路径，形态要和搜索条目一致。
	if !strings.HasSuffix(body.Data.AudioSpec.Path, ".mp3") || strings.Contains(body.Data.AudioSpec.Path, "/") {
		t.Fatalf("audioSpec.path 形态不对：%q", body.Data.AudioSpec.Path)
	}
	if body.Data.AudioSpec.Container != "mp3" {
		t.Fatalf("container 不对：%q", body.Data.AudioSpec.Container)
	}
}

// 非本扩展的 id 必须原样交给官方，不能抢答。
func TestTrackMetadataPassesThroughForOfficialGUID(t *testing.T) {
	env := newIntegrationEnv(t)
	rec := env.do(t, http.MethodGet, "/music/api/v1/track/metadata?guid=aaaa1111")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"from":"official"`) {
		t.Fatalf("应透传给官方，实际 %s", rec.Body.String())
	}
}

// 封面位：伪 coverId 要换成音源 LOGO，官方 coverId 仍走透传。
func TestCoverServesSourceLogoForMeowCoverID(t *testing.T) {
	env := newIntegrationEnv(t)

	ref := MeowRef{Song: "长城", Singer: "Beyond", Source: "netease"}
	fakeID := env.server.guids.Fake(EncodeGUID(ref))

	rec := env.do(t, http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+fakeID+"&size=160")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，实际 %q", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "\x89PNG") {
		t.Fatalf("不是 PNG：%x", rec.Body.Bytes()[:8])
	}

	// 认不出的 coverId 不能抢答。
	rec = env.do(t, http.MethodGet, "/music/api/v1/static/cover?coverId=track_ffffffffffffffffffffffffffffffff&size=160")
	if !strings.Contains(rec.Body.String(), `"from":"official"`) {
		t.Fatalf("未知 coverId 应透传，实际 %s", rec.Body.String())
	}
}

// 同源不同前缀（track_/album_）都要能认出来，前端在不同列表里拼法不一样。
func TestCoverStripsCategoryPrefixes(t *testing.T) {
	for _, p := range []string{"track_", "album_", "artist_", ""} {
		if got := stripCoverIDPrefixes(p + "abc123"); got != "abc123" {
			t.Fatalf("前缀 %q 未剥掉，得到 %q", p, got)
		}
	}
	// 纯粹以 track_ 开头的真 id 不能被剥成空串。
	if got := stripCoverIDPrefixes("track_"); got != "track_" {
		t.Fatalf("只剩前缀时不应剥离，得到 %q", got)
	}
}

func TestNormalizeSourceFallsBackToMeow(t *testing.T) {
	cases := map[string]string{
		"kuwo":        "kuwo",
		"NetEase":     "netease",
		"bili":        "bilibili",
		"5sing":       "fivesing",
		"unknown-src": "meow",
		"":            "meow",
		"qqmusic":     "qq",
	}
	for in, want := range cases {
		if got := normalizeSource(in); got != want {
			t.Fatalf("normalizeSource(%q)=%q，期望 %q", in, got, want)
		}
	}
}
