package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.StateDir = t.TempDir()
	return NewServer(cfg, cfg.DataDir+"/config.json")
}

func TestMergeOnlineAppendsAndDedupes(t *testing.T) {
	s := newTestServer(t)

	official := []any{
		map[string]any{"guid": "aaaa", "title": "晴天", "artist": "周杰伦"},
		map[string]any{"guid": "bbbb", "title": "稻香", "artist": "周杰伦"},
	}
	parsed := searchResponse{
		Code: 0,
		Data: map[string]any{"list": official, "total": float64(2)},
	}
	tracks := []MeowTrack{
		// 与官方重复：必须被丢掉，否则同一首歌会出现两次。
		{Title: "晴天", Artist: "周杰伦", URL: "http://m/stream_live?song=晴天&singer=周杰伦"},
		{Title: "夜曲", Artist: "周杰伦", URL: "http://m/stream_live?song=夜曲&singer=周杰伦", Source: "kuwo"},
		{Title: "以父之名", Artist: "周杰伦", URL: "http://m/stream_live?song=以父之名&singer=周杰伦", Source: "netease"},
	}

	merged, added := s.mergeOnline(parsed, tracks)
	if added != 2 {
		t.Fatalf("应只追加 2 条（去掉重复），实际 %d", added)
	}
	list := officialList(merged.Data)
	if len(list) != 4 {
		t.Fatalf("合并后应有 4 条，实际 %d", len(list))
	}
	// 官方条目必须原位保留、字段不动。
	first := list[0].(map[string]any)
	if first["guid"] != "aaaa" {
		t.Fatalf("官方条目被改动：%v", first)
	}
	total, _ := merged.Data["total"].(int)
	if total != 4 {
		t.Fatalf("total 应与列表长度一致（4），实际 %v", merged.Data["total"])
	}
}

func TestMergeOnlineProducesFakeHexGUID(t *testing.T) {
	s := newTestServer(t)
	parsed := searchResponse{Code: 0, Data: map[string]any{"list": []any{}}}
	tracks := []MeowTrack{{Title: "夜曲", Artist: "周杰伦", Source: "kuwo"}}

	merged, _ := s.mergeOnline(parsed, tracks)
	vo, ok := officialList(merged.Data)[0].(TrackVO)
	if !ok {
		t.Fatalf("在线条目应是 TrackVO，实际 %T", officialList(merged.Data)[0])
	}
	if len(vo.GUID) != 32 {
		t.Fatalf("guid 必须是 32 位十六进制，实际 %q", vo.GUID)
	}
	if vo.ID != vo.GUID {
		t.Fatalf("id 与 guid 必须一致（客户端两种键名都用），got id=%q guid=%q", vo.ID, vo.GUID)
	}
	if !strings.HasPrefix(vo.Title, sourceTagMeow) {
		t.Fatalf("搜索结果应带来源标记，实际 %q", vo.Title)
	}
	if !vo.IsOnline || vo.Source != "meow" {
		t.Fatalf("在线条目标记缺失：%+v", vo)
	}
	// 拖动进度条依赖 audioSpec.path 像文件名且不含斜杠。
	if strings.Contains(vo.AudioSpec.Path, "/") || !strings.HasSuffix(vo.AudioSpec.Path, ".mp3") {
		t.Fatalf("audioSpec.path 形态不对：%q", vo.AudioSpec.Path)
	}
	// 客户端要能从这里点进歌词。
	if !vo.HasLyric {
		t.Fatal("在线条目应声明 hasLyric，否则歌词入口不显示")
	}
}

func TestMergeOnlineKeepsOfficialShapeIntact(t *testing.T) {
	s := newTestServer(t)
	// 官方响应里可能有一堆我们不认识的字段，合并后必须原样带出去。
	parsed := searchResponse{
		Code: 0,
		Data: map[string]any{
			"list":     []any{map[string]any{"guid": "aaaa", "title": "本地歌"}},
			"total":    float64(1),
			"pageSize": float64(50),
			"extra":    map[string]any{"cursor": "xyz"},
		},
	}
	merged, added := s.mergeOnline(parsed, []MeowTrack{{Title: "新歌", Artist: "谁"}})
	if added != 1 {
		t.Fatalf("应追加 1 条，实际 %d", added)
	}
	if merged.Data["pageSize"] != float64(50) {
		t.Fatalf("未知字段 pageSize 被丢弃：%v", merged.Data["pageSize"])
	}
	if _, ok := merged.Data["extra"]; !ok {
		t.Fatal("未知字段 extra 被丢弃")
	}
}

func TestMergeOnlineNoTracksReturnsUnchanged(t *testing.T) {
	s := newTestServer(t)
	parsed := searchResponse{Code: 0, Data: map[string]any{"list": []any{}, "total": float64(0)}}
	_, added := s.mergeOnline(parsed, nil)
	if added != 0 {
		t.Fatalf("没有在线结果时不应追加，实际 %d", added)
	}
}

func TestTrackKeyMatchesAcrossShapes(t *testing.T) {
	// 官方条目与在线条目字段形态不同，去重键必须能对齐，
	// 否则同一首歌会被当成两条（一条本地、一条在线）。
	official := map[string]any{
		"title":   "晴天",
		"artists": []any{map[string]any{"name": "周杰伦"}},
	}
	asPlain := map[string]any{"name": "晴天", "artist": "周杰伦"}
	online := strings.ToLower("晴天") + "\x00" + strings.ToLower("周杰伦")

	if trackKey(official) != online {
		t.Fatalf("官方 artists 数组形态未对齐：%q", trackKey(official))
	}
	if trackKey(asPlain) != online {
		t.Fatalf("官方字符串形态未对齐：%q", trackKey(asPlain))
	}
}

func TestTrackToRefExtractsPlayParams(t *testing.T) {
	cases := []struct {
		name string
		in   MeowTrack
		want MeowRef
	}{
		{
			name: "普通音源",
			in:   MeowTrack{Title: "晴天", Artist: "周杰伦", Source: "kuwo", URL: "http://h:2233/stream_live?song=x&singer=y"},
			want: MeowRef{Song: "晴天", Singer: "周杰伦", Source: "kuwo"},
		},
		{
			name: "喜马拉雅 xid",
			in:   MeowTrack{Title: "三体", Artist: "广播剧", Source: "ximalaya", URL: "http://h:2233/stream_live?song=x&singer=y&xid=12345"},
			want: MeowRef{Song: "三体", Singer: "广播剧", XID: "12345", Source: "ximalaya"},
		},
		{
			name: "B 站 bv",
			in:   MeowTrack{Title: "晴天", Artist: "UP", Source: "bilibili", URL: "http://h:2233/stream_live?song=x&singer=y&bv=BV1BZbSzZEGT"},
			want: MeowRef{Song: "晴天", Singer: "UP", BV: "BV1BZbSzZEGT", Source: "bilibili"},
		},
		{
			name: "外部源整段地址",
			in:   MeowTrack{Title: "视频", Artist: "谁", Source: "ext", URL: "http://h:2233/api/ext_audio?url=https%3A%2F%2Fa.b%2Fc.mp3&k=1"},
			want: MeowRef{Song: "视频", Singer: "谁", Source: "ext", Path: "/api/ext_audio?url=https%3A%2F%2Fa.b%2Fc.mp3&k=1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trackToRef(tc.in)
			if got != tc.want {
				t.Fatalf("解析结果不符\n got=%+v\nwant=%+v", got, tc.want)
			}
		})
	}
}

func TestPlayPathRoundTrip(t *testing.T) {
	// 取流地址必须能把取流参数完整还原，尤其是 xid/bv——
	// 丢了它们就会退化成"按歌名重新解析"，放到同名的另一条音频。
	ref := MeowRef{Song: "晴天", Singer: "UP主", BV: "BV1BZbSzZEGT", Source: "bilibili"}
	path := ref.playPath()
	for _, want := range []string{"/stream_live?", "song=", "singer=", "bv=BV1BZbSzZEGT"} {
		if !strings.Contains(path, want) {
			t.Fatalf("取流地址缺少 %q：%s", want, path)
		}
	}
	// 没有 xid 时不应凭空造一个空参数出来。
	if strings.Contains(path, "xid=") {
		t.Fatalf("未设置 xid 却出现在地址里：%s", path)
	}
}

func TestPlayPathPrefersExplicitPath(t *testing.T) {
	ref := MeowRef{Song: "a", Singer: "b", Path: "/api/ext_audio?url=x"}
	if got := ref.playPath(); got != "/api/ext_audio?url=x" {
		t.Fatalf("应优先使用原始地址，实际 %q", got)
	}
}

func TestSearchResponsePreservesUnknownFields(t *testing.T) {
	// 合并走 map[string]any 中转，验证序列化后未知字段没被吃掉。
	raw := `{"code":0,"data":{"list":[],"total":0,"weird":{"a":1}},"msg":"ok"}`
	var parsed searchResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	merged, _ := s.mergeOnline(parsed, nil)
	out, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"weird"`) {
		t.Fatalf("未知字段在合并后丢失：%s", out)
	}
}
