package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 是扩展的全部可调项。
//
// 三层来源，后者覆盖前者：内置默认值 → 配置文件 → 环境变量。环境变量优先级最高
// 是刻意的：飞牛应用中心的启动脚本、systemd 单元、临时排障都用环境变量注入，
// 不必回写用户的配置文件（否则排障时手改的临时值会被永久记录下来）。
type Config struct {
	// ---- Meow 上游 ----
	// MeowURL 是 MeowMusicServer 的根地址，例如 http://192.168.2.220:2233。
	MeowURL string `json:"meow_url"`
	// MeowToken 仅在上游启用了鉴权时需要；空表示匿名访问。
	MeowToken string `json:"meow_token"`
	// Sources 是向 Meow 请求的音源键名列表（kuwo/netease/kugou/...）。
	// 留空表示不指定，由 Meow 自己决定（等于它的全部启用音源）。
	Sources []string `json:"sources"`
	// SearchLimit 是单次从 Meow 拉取的候选条数。飞牛客户端一页通常是 50 条，
	// 官方结果会占掉一部分，所以这里取 30 左右即可，不必贪多。
	SearchLimit int `json:"search_limit"`

	// ---- 行为开关 ----
	// OnlineEnabled 关闭后扩展退化为纯透传（相当于临时停用在线搜索）。
	OnlineEnabled bool `json:"online_enabled"`
	// StreamDirectRanges 决定 Range 请求是否原样转给 Meow。
	// Meow 的 /stream_live 命中缓存时用 http.ServeContent 提供完整 Range 支持，
	// 所以默认直传即可；置 false 仅用于排障（强制整段拉取）。
	StreamDirectRanges bool `json:"stream_direct_ranges"`

	// ---- 超时（秒）----
	// SearchTimeout 要小于飞牛客户端的等待上限，否则用户看到的是客户端超时
	// 而不是"在线结果暂不可用"。3~5 秒是实测比较舒服的区间。
	SearchTimeout int `json:"search_timeout_s"`
	// StreamTimeout 必须放得很宽：Meow 首次点歌要走"取直链 + ffmpeg 转码落盘"，
	// 冷启动几十秒是常态，之后才走缓存。这里偏保守地给 300 秒。
	StreamTimeout int `json:"stream_timeout_s"`
	LyricTimeout  int `json:"lyric_timeout_s"`

	// ---- YouTube（自建 mp3api，非 Meow 内置源）----
	//
	// Meow 本身不支持 YouTube，这里接的是"YouTube 链接 → MP3"的转换服务
	// （yt-dlp + ffmpeg，/api/search|resolve|music|lyrics）。地址留空 = 不启用，
	// 此时 sources 里即使勾了 youtube 也会被忽略，不会拖慢其他音源的搜索。
	YoutubeAPIURL string `json:"youtube_api_url"`
	// YoutubeAPIKey 走 X-API-Key 头。空表示服务端没开鉴权。
	YoutubeAPIKey string `json:"youtube_api_key"`
	// YoutubeBitrate 是转 MP3 的目标码率（kbps），服务端允许 64~320，默认 192。
	// 码率也是缓存键的一部分：改了会重新下载一遍。
	YoutubeBitrate int `json:"youtube_bitrate"`
	// YoutubeMaxDuration 过滤搜索结果里过长的视频（秒），0 表示不过滤。
	// YouTube 搜歌回来的常是几十分钟的合集/MV，播起来体验很差；
	// 服务端自己也有下载时长上限（默认 900 秒），超了会直接失败，
	// 所以在搜索阶段就挡掉，别让用户点了才看到错误。
	YoutubeMaxDuration int `json:"youtube_max_duration_s"`

	// ---- 路径 ----
	// UpstreamSocket 是官方音乐的原始 socket，接管时会被移到这里。
	UpstreamSocket string `json:"upstream_socket"`
	// TargetSocket 是 nginx 与飞牛客户端连接的入口，接管后由本扩展监听。
	TargetSocket string `json:"target_socket"`
	// GatewaySocket 是飞牛桌面网关连接的 socket（管理页入口）。
	// 应用中心按 app/ui/config 里的 gatewaySocket 找它，文件名必须叫 <appname>.sock。
	GatewaySocket string `json:"gateway_socket"`
	// StateDir 放锁文件与临时监听 socket，属于运行期状态，重启即失效。
	StateDir string `json:"state_dir"`
	// DataDir 放需要跨重启保留的东西（id 伪装映射）。
	DataDir string `json:"data_dir"`
}

const (
	// 与飞牛音乐扩展的既有约定一致：官方 socket 是 /var/run/trim_music.socket，
	// 接管后官方被挪到 *_upstream.socket，仍可通过它直连排障。
	DefaultTargetSocket   = "/var/run/trim_music.socket"
	DefaultUpstreamSocket = "/var/run/trim_music_upstream.socket"
	DefaultGatewaySocket  = "/var/run/meow-ext.sock"
	DefaultStateDir       = "/run/meow-ext"
	DefaultDataDir        = "/var/lib/meow-ext"
	DefaultConfigPath     = "/etc/meow-ext/config.json"
)

// DefaultConfig 返回内置默认值。Meow 地址刻意不预设任何具体主机：
// 硬编码一个内网 IP 只会让别人的安装静默指向一台不存在的机器。
func DefaultConfig() Config {
	return Config{
		MeowURL:            "http://127.0.0.1:2233",
		SearchLimit:        30,
		OnlineEnabled:      true,
		StreamDirectRanges: true,
		SearchTimeout:      5,
		StreamTimeout:      300,
		LyricTimeout:       8,
		UpstreamSocket:     DefaultUpstreamSocket,
		TargetSocket:       DefaultTargetSocket,
		GatewaySocket:      DefaultGatewaySocket,
		StateDir:           DefaultStateDir,
		DataDir:            DefaultDataDir,
		YoutubeBitrate:     192,
		YoutubeMaxDuration: 900,
	}
}

// ConfigPath 解析配置文件位置：--config 显式指定 > MEOW_EXT_CONFIG > 默认路径。
func ConfigPath(flagValue string) string {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("MEOW_EXT_CONFIG")); s != "" {
		return s
	}
	return DefaultConfigPath
}

// LoadConfig 读取配置文件并叠加环境变量。
//
// 配置文件不存在不算错误——首次安装、纯环境变量部署都属于正常路径，
// 这里返回默认值即可，让用户能在管理页里保存出第一份配置。
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("配置文件 %s 不是合法 JSON：%w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次运行：保持默认值。
	default:
		return cfg, fmt.Errorf("读取配置文件 %s 失败：%w", path, err)
	}
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

// applyEnv 用环境变量覆盖配置。只覆盖显式设置过的变量——
// 空字符串视为"未设置"，避免 systemd 里写了 `Environment=MEOW_URL=` 就把地址清空。
func applyEnv(cfg *Config) {
	setStr := func(env string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			*dst = v
		}
	}
	setStr("MEOW_EXT_URL", &cfg.MeowURL)
	setStr("MEOW_EXT_TOKEN", &cfg.MeowToken)
	setStr("MEOW_EXT_TARGET_SOCKET", &cfg.TargetSocket)
	setStr("MEOW_EXT_UPSTREAM_SOCKET", &cfg.UpstreamSocket)
	setStr("MEOW_EXT_GATEWAY_SOCKET", &cfg.GatewaySocket)
	setStr("MEOW_EXT_STATE_DIR", &cfg.StateDir)
	setStr("MEOW_EXT_DATA_DIR", &cfg.DataDir)
	setStr("MEOW_EXT_YOUTUBE_URL", &cfg.YoutubeAPIURL)
	setStr("MEOW_EXT_YOUTUBE_KEY", &cfg.YoutubeAPIKey)
	if v := strings.TrimSpace(os.Getenv("MEOW_EXT_YOUTUBE_BITRATE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.YoutubeBitrate = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("MEOW_EXT_SOURCES")); v != "" {
		cfg.Sources = splitList(v)
	}
	if v := strings.TrimSpace(os.Getenv("MEOW_EXT_ONLINE")); v != "" {
		cfg.OnlineEnabled = isTruthy(v)
	}
}

func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// Validate 拦住"能存进去但跑起来才炸"的配置。
// 校验点都是实测踩过的：URL 少 scheme 会让 http.Client 直接报 unsupported protocol，
// 超时设 0 会让请求永久挂住。
func (c *Config) Validate() error {
	c.MeowURL = strings.TrimRight(strings.TrimSpace(c.MeowURL), "/")
	if c.MeowURL == "" {
		return errors.New("meow_url 不能为空")
	}
	u, err := url.Parse(c.MeowURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("meow_url 必须是完整地址（含 http:// 或 https://），当前为 %q", c.MeowURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("meow_url 的协议只支持 http/https，当前为 %q", u.Scheme)
	}
	if c.SearchLimit <= 0 {
		c.SearchLimit = 30
	}
	if c.SearchLimit > 200 {
		c.SearchLimit = 200
	}
	if c.SearchTimeout <= 0 {
		c.SearchTimeout = 5
	}
	if c.StreamTimeout <= 0 {
		c.StreamTimeout = 300
	}
	if c.LyricTimeout <= 0 {
		c.LyricTimeout = 8
	}
	if c.TargetSocket == "" || c.UpstreamSocket == "" {
		return errors.New("target_socket 与 upstream_socket 不能为空")
	}
	if c.TargetSocket == c.UpstreamSocket {
		return errors.New("target_socket 与 upstream_socket 不能是同一个路径")
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	// YouTube 是可选增强：地址留空就整体关闭，不必报错。
	c.YoutubeAPIURL = strings.TrimRight(strings.TrimSpace(c.YoutubeAPIURL), "/")
	if c.YoutubeAPIURL != "" {
		u, err := url.Parse(c.YoutubeAPIURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("youtube_api_url 必须是完整地址（含 http:// 或 https://），当前为 %q", c.YoutubeAPIURL)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("youtube_api_url 的协议只支持 http/https，当前为 %q", u.Scheme)
		}
	}
	switch c.YoutubeBitrate {
	case 0:
		c.YoutubeBitrate = 192
	case 64, 96, 128, 192, 256, 320:
	default:
		// 服务端只认这几档，别的值会被它拒掉，别让用户存进去之后才发现。
		return fmt.Errorf("youtube_bitrate 只支持 64/96/128/192/256/320，当前为 %d", c.YoutubeBitrate)
	}
	if c.YoutubeMaxDuration < 0 {
		c.YoutubeMaxDuration = 0
	}
	return nil
}

// Save 原子写回配置文件。
//
// 管理页保存时可能正被别的进程读取（下次启动、排障用的 cat），
// 先写临时文件再 rename 才能保证读到的永远是完整 JSON 而不是半截。
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// 配置文件里可能有上游 token，只给属主读写。
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
