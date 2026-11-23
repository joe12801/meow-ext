package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Version 由构建时的 -ldflags 覆盖，默认值只用于本地开发。
var Version = "dev"

// adminPrefix 是管理页的访问前缀，必须与 FPK 里 app/ui/config 的
// gatewayPrefix 保持一致：飞牛桌面按这个前缀转发到 gatewaySocket。
const adminPrefix = "/app/meow-ext"

// Server 把官方反代、Meow 客户端、id 映射与管理页串起来。
//
// 配置支持热更新（管理页保存即时生效），因此 cfg 与 meow 都通过读写锁访问，
// 不允许在别处直接引用字段——一旦有地方缓存了旧配置，改配置就会"部分生效"，
// 这种 bug 极难排查。
type Server struct {
	mu       sync.RWMutex
	cfg      Config
	cfgPath  string
	meow     *MeowClient
	youtube  *YouTubeClient
	guids    *GUIDRegistry
	upstream *Upstream
	images   *http.Client
	// meta 负责给在线曲目补封面与歌词（网易云公开接口）。
	// 与 cfg 无关地常驻：配置只决定用不用它，不决定要不要重建缓存。
	meta *metaResolver

	startedAt time.Time
}

func NewServer(cfg Config, cfgPath string) *Server {
	s := &Server{
		cfg:      cfg,
		cfgPath:  cfgPath,
		meow:     NewMeowClient(cfg),
		youtube:  NewYouTubeClient(cfg),
		guids:    NewGUIDRegistry(cfg.DataDir + "/guid-map.json"),
		upstream: NewUpstream(cfg.UpstreamSocket),
		images: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        8,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 8 * time.Second,
			},
		},
		meta:      newMetaResolver(),
		startedAt: time.Now(),
	}
	if err := s.guids.Load(); err != nil {
		log.Printf("[init] 读取 id 映射失败（不影响启动）：%v", err)
	}
	return s
}

// Config 返回当前配置快照。
func (s *Server) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) meowClient() *MeowClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meow
}

// youtubeClient 可能返回 nil（未配置 mp3api 地址），调用方必须判空。
func (s *Server) youtubeClient() *YouTubeClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.youtube
}

// ApplyConfig 热更新配置并落盘。
func (s *Server) ApplyConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	// 先落盘再切换：写盘失败时内存里还是旧配置，界面刷新看到的就是真实生效值，
	// 不会出现"显示已改、实际没改"的错觉。
	if err := cfg.Save(s.cfgPath); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.meow = NewMeowClient(cfg)
	s.youtube = NewYouTubeClient(cfg)
	s.mu.Unlock()
	return nil
}

// Handler 返回接管官方入口用的处理器。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.route)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/_ext/healthz":
		s.handleHealth(w, r)
		return
	case path == adminPrefix || strings.HasPrefix(path, adminPrefix+"/"):
		s.handleAdmin(w, r)
		return
	case path == "/music/api/v1/search/track" || strings.HasPrefix(path, "/music/api/v1/search/track/"):
		s.handleSearchTrack(w, r)
		return
	case path == "/music/api/v1/track/stream" || strings.HasPrefix(path, "/music/api/v1/track/stream/"):
		s.handleStream(w, r, tailSegment(path, "/music/api/v1/track/stream/"))
		return
	// 播放前的元数据。缺了它客户端会把这次播放判成"播放源不存在"，
	// 连取流请求都不会发出来——表现就是"搜得到、点了没声"。
	case path == "/music/api/v1/track/metadata" || strings.HasPrefix(path, "/music/api/v1/track/metadata/"):
		s.handleTrackMetadata(w, r)
		return
	case path == "/music/api/v1/track/lyrics" || strings.HasPrefix(path, "/music/api/v1/track/lyrics/"):
		s.handleLyrics(w, r, tailSegment(path, "/music/api/v1/track/lyrics/"))
		return
	// 飞牛 1.1.x 播放页真正打的是这个路径（见 lyric.go 顶部的说明）。
	case path == "/music/api/v1/lyric/list" || strings.HasPrefix(path, "/music/api/v1/lyric/list/"):
		s.handleLyricList(w, r, tailSegment(path, "/music/api/v1/lyric/list/"))
		return
	case strings.HasPrefix(path, "/music/api/v1/detail/lyrics/"):
		s.handleLyrics(w, r, tailSegment(path, "/music/api/v1/detail/lyrics/"))
		return
	case path == "/music/api/v1/static/cover" || strings.HasPrefix(path, "/music/api/v1/static/cover/"):
		s.handleCover(w, r)
		return
	}
	// 其余一切照旧交给官方。
	s.upstream.ServeHTTP(w, r)
}

// tailSegment 取前缀之后的路径。用于 /track/stream/<guid> 这种把 id 放在
// 路径里的调用方式——飞牛 1.1.19 两种传法都会用。
func tailSegment(path, prefix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.Trim(rest, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// handleHealth 同时检查官方与 Meow 两侧。
//
// 故意在 Meow 不可用时仍返回 200 且 ok:true——扩展的核心职责是把官方
// 业务原样透传，只要官方在，扩展就是可用的；在线搜索只是增强项。
// 把 Meow 断线算成整体故障，会让应用中心把健康的扩展当成崩溃反复重启。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	upErr := s.upstream.Probe(r.Context())

	meowErrText := ""
	if cfg.OnlineEnabled {
		ctx, cancel := timeoutCtx(r, 3*time.Second)
		defer cancel()
		if err := s.meowClient().Health(ctx); err != nil {
			meowErrText = err.Error()
		}
	}

	// YouTube 是可选源：未配置时给 ok:false + configured:false，
	// 别让健康检查把它当成故障。
	ytErrText := ""
	yt := s.youtubeClient()
	ytConfigured := yt != nil
	if ytConfigured && cfg.OnlineEnabled {
		ctx, cancel := timeoutCtx(r, 3*time.Second)
		defer cancel()
		if err := yt.Health(ctx); err != nil {
			ytErrText = err.Error()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       upErr == nil,
		"version":  Version,
		"uptime_s": int(time.Since(s.startedAt).Seconds()),
		"upstream": map[string]any{
			"ok":     upErr == nil,
			"socket": cfg.UpstreamSocket,
			"error":  errText(upErr),
		},
		"meow": map[string]any{
			"ok":    cfg.OnlineEnabled && meowErrText == "",
			"url":   cfg.MeowURL,
			"error": meowErrText,
		},
		"youtube": map[string]any{
			"configured": ytConfigured,
			"ok":         ytConfigured && cfg.OnlineEnabled && ytErrText == "",
			"url":        cfg.YoutubeAPIURL,
			"error":      ytErrText,
		},
		"online":   cfg.OnlineEnabled,
		"guid_map": s.guids.Size(),
	})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// timeoutCtx 给请求派生一个带超时的上下文。
func timeoutCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(body)
}
