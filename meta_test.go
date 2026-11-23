package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 歌词接口是"有没有歌词"的唯一开关：飞牛 1.1.x 打的是 /lyric/list?trackGUID=，
// 响应结构必须与官方一致（list[].content + source=2 + preferred），
// 少一个字段客户端就当没有歌词，界面上就是一片空白。
func TestLyricListReturnsLRC(t *testing.T) {
	env := newIntegrationEnv(t)

	ref := MeowRef{Song: "晴天", Singer: "周杰伦", Source: "kuwo"}
	fakeID := env.server.guids.Fake(EncodeGUID(ref))

	rec := env.do(t, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+fakeID+"&lan=zh-CN")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				GUID    string `json:"guid"`
				Content string `json:"content"`
				Source  int    `json:"source"`
				IsLRC   bool   `json:"isLRC"`
			} `json:"list"`
			Preferred string `json:"preferred"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期结构：%v body=%s", err, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("code 应为 0，实际 %d", body.Code)
	}
	if len(body.Data.List) != 1 {
		t.Fatalf("应有一条歌词，实际 %d：%s", len(body.Data.List), rec.Body.String())
	}
	item := body.Data.List[0]
	if item.Content != "[00:00.00]测试歌词" {
		t.Fatalf("歌词内容不对：%q", item.Content)
	}
	if item.Source != 2 || !item.IsLRC {
		t.Fatalf("source/isLRC 不对：%+v", item)
	}
	if body.Data.Preferred != fakeID+":lyric" || item.GUID != body.Data.Preferred {
		t.Fatalf("preferred 应指向该条歌词：preferred=%q guid=%q", body.Data.Preferred, item.GUID)
	}
}

// 非本扩展的 id 必须透传：本地曲目的歌词仍由官方提供。
func TestLyricListPassesThroughUnknownGUID(t *testing.T) {
	env := newIntegrationEnv(t)
	rec := env.do(t, http.MethodGet, "/music/api/v1/lyric/list?trackGUID=aaaa1111&lan=zh-CN")
	if !strings.Contains(rec.Body.String(), `"from":"official"`) {
		t.Fatalf("应透传给官方，实际 %s", rec.Body.String())
	}
}

// 没取到歌词时也要回 code:0 + 空列表：回错误码会让客户端弹提示，
// 而"这首歌没歌词"是正常状态。
func TestLyricListEmptyIsNotAnError(t *testing.T) {
	env := newIntegrationEnv(t)
	// 假 Meow 对未登记的曲目会返回 404，这里用一首歌名带特殊标记的曲子模拟。
	ref := MeowRef{Song: "纯音乐NO_LYRIC", Singer: "", Source: "kuwo"}
	fakeID := env.server.guids.Fake(EncodeGUID(ref))
	rec := env.do(t, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+fakeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int `json:"code"`
		Data struct {
			List      []any  `json:"list"`
			Preferred string `json:"preferred"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期结构：%v body=%s", err, rec.Body.String())
	}
	if body.Code != 0 || body.Data.Preferred != "" {
		t.Fatalf("空歌词应为 code:0 + 空 preferred，实际 %s", rec.Body.String())
	}
	if body.Data.List == nil {
		t.Fatalf("list 应为空数组而不是 null：%s", rec.Body.String())
	}
}

// 占位歌词（"纯音乐，请欣赏"）不该被当成歌词塞进面板。
func TestCleanLyricDropsPlaceholder(t *testing.T) {
	cases := map[string]string{
		"纯音乐，请欣赏":        "",
		"暂无歌词":           "",
		"[00:00.00]真实歌词": "[00:00.00]真实歌词",
		"":               "",
	}
	for in, want := range cases {
		if got := cleanLyric(in); got != want {
			t.Fatalf("cleanLyric(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// neTestSong 造一条 cloudsearch 结果。neSong.AR 是匿名结构体切片，
// 直接写字面量会把用例淹没在类型声明里。
func neTestSong(id int64, name string, artists ...string) neSong {
	s := neSong{ID: id, Name: name}
	for _, a := range artists {
		var item struct {
			Name string `json:"name"`
		}
		item.Name = a
		s.AR = append(s.AR, item)
	}
	return s
}

// 检索结果打分：同名同歌手要赢过同名翻唱，噪声标题要降权。
func TestPickSongPrefersOriginal(t *testing.T) {
	songs := []neSong{
		neTestSong(1, "晴天 (原唱 周杰伦)", "RyaVocal"),
		neTestSong(2, "晴天", "周杰伦"),
		neTestSong(3, "晴天 (钢琴版)", "周杰伦"),
	}
	got, ok := pickSong(songs, "晴天", "周杰伦", false)
	if !ok || got.ID != 2 {
		t.Fatalf("应选原始版本（id=2），实际 id=%d ok=%v", got.ID, ok)
	}

	// 完全对不上时宁可返回 false——错封面比没封面更刺眼。
	if _, ok := pickSong(songs, "完全不存在的歌", "无名", false); ok {
		t.Fatalf("对不上时不应返回候选")
	}

	// 严格模式（视频/有声书来源）只认完全同名，包含匹配一律不要。
	if _, ok := pickSong(songs, "晴天 现场", "", true); ok {
		t.Fatalf("严格模式不应接受包含匹配")
	}
	if got, ok := pickSong(songs, "晴天", "周杰伦", true); !ok || got.ID != 2 {
		t.Fatalf("严格模式仍应接受完全同名，实际 id=%d ok=%v", got.ID, ok)
	}
}

func TestCleanTitleStripsBrackets(t *testing.T) {
	cases := map[string]string{
		"晴天 (原唱 周杰伦)": "晴天",
		"【纯音乐】夜曲":     "夜曲",
		"起风了（Live）":   "起风了",
	}
	for in, want := range cases {
		if got := cleanTitle(in); got != want {
			t.Fatalf("cleanTitle(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// 网易云封面必须带上尺寸参数：不拼就是原图（实测单张 7MB）。
func TestWithCoverSize(t *testing.T) {
	base := "https://p1.music.126.net/abc==/123.jpg"
	if got := withCoverSize(base, 160); got != base+"?param=160y160" {
		t.Fatalf("尺寸参数拼错：%q", got)
	}
	if got := withCoverSize(base+"?x=1", 600); got != base+"?x=1&param=600y600" {
		t.Fatalf("已有查询串时应追加：%q", got)
	}
	// 别的源不认这个参数，不能加。
	yt := "https://i.ytimg.com/vi/abc/hqdefault.jpg"
	if got := withCoverSize(yt, 160); got != yt {
		t.Fatalf("非网易云地址不应被改写：%q", got)
	}
}

// LOGO 是"海报还没解析好"的占位，必须短缓存；标成 immutable 一周会把占位钉死。
func TestCoverLogoIsShortCached(t *testing.T) {
	env := newIntegrationEnv(t)
	ref := MeowRef{Song: "夜曲", Singer: "周杰伦", Source: "netease"}
	fakeID := env.server.guids.Fake(EncodeGUID(ref))
	rec := env.do(t, http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+fakeID+"&size=40")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	cc := rec.Header().Get("Cache-Control")
	if strings.Contains(cc, "immutable") || strings.Contains(cc, "max-age=604800") {
		t.Fatalf("LOGO 不应长缓存：%q", cc)
	}
	if !strings.Contains(cc, "max-age=60") {
		t.Fatalf("LOGO 应短缓存：%q", cc)
	}
}
