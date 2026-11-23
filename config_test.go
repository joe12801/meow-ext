package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultsWhenMissing(t *testing.T) {
	// 首次安装时配置文件还不存在，这属于正常路径而不是错误。
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("配置不存在时不应报错：%v", err)
	}
	if cfg.MeowURL != "http://127.0.0.1:2233" {
		t.Fatalf("默认 Meow 地址不对：%s", cfg.MeowURL)
	}
	if !cfg.OnlineEnabled {
		t.Fatal("默认应启用在线搜索")
	}
	if cfg.StreamTimeout < 120 {
		t.Fatalf("取流超时默认值过小（冷启动转码要几十秒）：%d", cfg.StreamTimeout)
	}
}

func TestLoadConfigReadsFileAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"meow_url":"http://192.168.2.220:2233","search_limit":42,"sources":["kuwo","netease"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeowURL != "http://192.168.2.220:2233" || cfg.SearchLimit != 42 {
		t.Fatalf("配置文件未生效：%+v", cfg)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("音源列表未生效：%v", cfg.Sources)
	}

	// 环境变量优先级最高：排障时临时改地址不该污染用户配置。
	t.Setenv("MEOW_EXT_URL", "http://127.0.0.1:9999")
	t.Setenv("MEOW_EXT_SOURCES", "kugou, bilibili")
	cfg2, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.MeowURL != "http://127.0.0.1:9999" {
		t.Fatalf("环境变量未覆盖配置文件：%s", cfg2.MeowURL)
	}
	if len(cfg2.Sources) != 2 || cfg2.Sources[0] != "kugou" {
		t.Fatalf("音源环境变量未生效：%v", cfg2.Sources)
	}
}

func TestLoadConfigRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte("{oops"), 0o600)
	// 配置坏了必须报错并指出是哪个文件，不能静默回退到默认值——
	// 否则用户会以为地址填错了，实际是 JSON 少了个引号。
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"空地址", func(c *Config) { c.MeowURL = "" }, true},
		{"缺少 scheme", func(c *Config) { c.MeowURL = "192.168.2.220:2233" }, true},
		{"不支持的协议", func(c *Config) { c.MeowURL = "ftp://x/y" }, true},
		{"去掉结尾斜杠", func(c *Config) { c.MeowURL = "http://h:2233///" }, false},
		{"两个 socket 相同", func(c *Config) { c.UpstreamSocket = c.TargetSocket }, true},
		{"超时为 0 应回退默认", func(c *Config) { c.SearchTimeout = 0 }, false},
		{"条数为负应回退默认", func(c *Config) { c.SearchLimit = -5 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("期望报错但没有")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应报错：%v", err)
			}
		})
	}
}

func TestConfigValidateNormalizesValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MeowURL = "http://h:2233//"
	cfg.SearchTimeout = 0
	cfg.SearchLimit = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.MeowURL != "http://h:2233" {
		t.Fatalf("结尾斜杠未清理：%q", cfg.MeowURL)
	}
	if cfg.SearchTimeout != 5 || cfg.SearchLimit != 30 {
		t.Fatalf("零值未回退默认：timeout=%d limit=%d", cfg.SearchTimeout, cfg.SearchLimit)
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")
	cfg := DefaultConfig()
	cfg.MeowURL = "http://10.0.0.5:2233"
	cfg.MeowToken = "tok"
	cfg.Sources = []string{"kuwo", "bilibili"}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 配置里有上游令牌，不能给同组其他用户读。
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("配置文件权限过宽：%o", perm)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MeowURL != cfg.MeowURL || loaded.MeowToken != cfg.MeowToken {
		t.Fatalf("往返不一致：%+v", loaded)
	}
}

func TestIsTruthyAndSplitList(t *testing.T) {
	for _, s := range []string{"1", "true", "YES", "on", "y"} {
		if !isTruthy(s) {
			t.Fatalf("%q 应判为真", s)
		}
	}
	for _, s := range []string{"0", "false", "no", ""} {
		if isTruthy(s) {
			t.Fatalf("%q 应判为假", s)
		}
	}
	got := splitList(" kuwo, netease ;kugou ")
	if len(got) != 3 || got[0] != "kuwo" || got[2] != "kugou" {
		t.Fatalf("列表解析不对：%v", got)
	}
}
