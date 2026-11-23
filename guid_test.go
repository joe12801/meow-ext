package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		ref  MeowRef
	}{
		{"只有歌名歌手", MeowRef{Song: "晴天", Singer: "周杰伦"}},
		{"带来源", MeowRef{Song: "晴天", Singer: "周杰伦", Source: "kuwo"}},
		{"喜马拉雅 xid", MeowRef{Song: "三体", Singer: "广播剧", XID: "123456", Source: "ximalaya"}},
		{"B 站 bv", MeowRef{Song: "晴天", Singer: "UP主", BV: "BV1BZbSzZEGT", Source: "bilibili"}},
		{
			"外部源整段地址",
			MeowRef{
				Song: "某视频", Singer: "某人", Source: "ext",
				Path: "/api/ext_audio?url=https%3A%2F%2Fexample.com%2Fa.mp3&sig=abc",
			},
		},
		{"歌名带冒号与括号", MeowRef{Song: "晴天 (Live: 2019 版)", Singer: "周杰伦/杨瑞代"}},
		{"歌名带单元分隔符以外的特殊字符", MeowRef{Song: "a\x00b", Singer: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := EncodeGUID(tc.ref)
			if !strings.HasPrefix(id, guidPrefix) {
				t.Fatalf("id 未带前缀：%q", id)
			}
			got, ok := DecodeGUID(id)
			if !ok {
				t.Fatalf("解码失败：%q", id)
			}
			if got != tc.ref {
				t.Fatalf("往返不一致\n got=%+v\nwant=%+v", got, tc.ref)
			}
		})
	}
}

func TestDecodeRejectsForeignIDs(t *testing.T) {
	// 飞牛官方 id 是 32 位十六进制，绝不能被我方逻辑认领，
	// 否则官方曲目会被送进 Meow 取流。
	for _, id := range []string{
		"",
		"5f2d1c0e9b8a7d6c5b4a39281706f5e4",
		"online:netease:12345",
		"online:meow:",
		"online:meow:not-base64!!",
	} {
		if _, ok := DecodeGUID(id); ok {
			t.Fatalf("不应识别为 Meow id：%q", id)
		}
	}
}

func TestDecodeToleratesPaddedBase64(t *testing.T) {
	ref := MeowRef{Song: "晴天", Singer: "周杰伦", XID: "42"}
	id := EncodeGUID(ref)
	// 某些客户端会重新编码 id 并补上 padding，需要能容忍。
	padded := strings.TrimRight(id, "=") + "=="
	got, ok := DecodeGUID(padded)
	if !ok || got != ref {
		t.Fatalf("带 padding 的 id 解码失败：%q -> %+v ok=%v", padded, got, ok)
	}
}

func TestRegistryFakeIsDeterministic(t *testing.T) {
	reg := NewGUIDRegistry("")
	real := EncodeGUID(MeowRef{Song: "晴天", Singer: "周杰伦"})

	first := reg.Fake(real)
	second := reg.Fake(real)
	if first != second {
		t.Fatalf("同一首歌两次搜索结果得到了不同 id：%s vs %s", first, second)
	}
	if len(first) != 32 {
		t.Fatalf("伪装 id 必须是 32 位十六进制，得到 %q（%d 位）", first, len(first))
	}
	for _, r := range first {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("伪装 id 含非十六进制字符：%q", first)
		}
	}
}

func TestRegistryResolveAcceptsBothForms(t *testing.T) {
	reg := NewGUIDRegistry("")
	real := EncodeGUID(MeowRef{Song: "晴天", Singer: "周杰伦", BV: "BV1xx"})
	fake := reg.Fake(real)

	for _, id := range []string{fake, real} {
		got, ok := reg.Resolve(id)
		if !ok || got != real {
			t.Fatalf("无法解回真实 id：%q -> %q ok=%v", id, got, ok)
		}
	}
	if _, ok := reg.Resolve("5f2d1c0e9b8a7d6c5b4a39281706f5e4"); ok {
		t.Fatal("官方 id 不应被解析成 Meow id")
	}
}

func TestRegistryDoesNotAliasUnrelatedRealIDs(t *testing.T) {
	reg := NewGUIDRegistry("")
	a := EncodeGUID(MeowRef{Song: "A", Singer: "X"})
	b := EncodeGUID(MeowRef{Song: "B", Singer: "Y"})
	if reg.Fake(a) == reg.Fake(b) {
		t.Fatal("不同曲目得到了相同伪装 id，会导致点 A 播 B")
	}
}

func TestRegistryPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guid-map.json")

	reg := NewGUIDRegistry(path)
	real := EncodeGUID(MeowRef{Song: "稻香", Singer: "周杰伦", Source: "netease"})
	fake := reg.Fake(real)
	if err := reg.Save(); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	// 模拟进程重启：新实例从磁盘加载，必须还能认出客户端手里的旧 id。
	rebooted := NewGUIDRegistry(path)
	if err := rebooted.Load(); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	got, ok := rebooted.Resolve(fake)
	if !ok || got != real {
		t.Fatalf("重启后旧 id 失效：%q -> %q ok=%v", fake, got, ok)
	}
}

func TestRegistrySurvivesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guid-map.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewGUIDRegistry(path)
	// 映射表损坏不能阻断启动：丢的只是旧 id，重新搜索即可恢复。
	if err := reg.Load(); err != nil {
		t.Fatalf("损坏的映射表不应导致加载报错：%v", err)
	}
	if reg.Size() != 0 {
		t.Fatalf("损坏文件应被忽略，实际载入 %d 条", reg.Size())
	}
}

func TestRegistryEvictsOldestEntries(t *testing.T) {
	reg := NewGUIDRegistry("")
	reg.maxEntries = 3
	ids := make([]string, 5)
	for i := range ids {
		ids[i] = EncodeGUID(MeowRef{Song: string(rune('a' + i)), Singer: "s"})
		reg.Fake(ids[i])
	}
	if reg.Size() != 3 {
		t.Fatalf("超出上限后应裁剪到 3 条，实际 %d", reg.Size())
	}
	// 最新的条目必须还在，否则"刚搜完就点播放"会失败。
	if _, ok := reg.Resolve(reg.Fake(ids[4])); !ok {
		t.Fatal("最新登记的条目不慎被淘汰")
	}
}
