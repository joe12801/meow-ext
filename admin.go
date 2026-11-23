package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed ui/index.html
var adminHTML []byte

// knownSource 是 MeowMusicServer 支持的音源键名与显示名。
//
// 这份清单只是给管理页做勾选项的"建议值"，不是白名单：用户在配置文件里
// 手写一个上游新增的键名同样会被透传给 Meow。写死键名却没写死校验，
// 是因为上游加源不该要求扩展跟着发版。
type knownSource struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Note 说明这个源的已知限制，直接显示在管理页上，省得用户以为扩展坏了。
	Note string `json:"note,omitempty"`
}

var knownSources = []knownSource{
	{Key: "kuwo", Label: "酷我音乐"},
	{Key: "netease", Label: "网易云音乐"},
	{Key: "kugou", Label: "酷狗音乐"},
	{Key: "qq", Label: "QQ 音乐", Note: "上游常返回空结果"},
	{Key: "ximalaya", Label: "喜马拉雅", Note: "热门内容多为 VIP，需上游账号会员"},
	{Key: "fivesing", Label: "5sing", Note: "原创与翻唱为主"},
	{Key: "lizhi", Label: "荔枝 FM", Note: "上游解析不稳定"},
	{Key: "bilibili", Label: "哔哩哔哩"},
	{Key: "qishui", Label: "汽水音乐"},
	{Key: "ext", Label: "扩展源", Note: "需在 Meow 侧另行配置"},
}

// handleAdmin 处理管理页与其 API。
//
// 路由刻意用 /app/meow-ext/ 前缀：飞牛桌面按 app/ui/config 的 gatewayPrefix
// 把请求转到本进程的 gatewaySocket，前端资源必须挂在这个前缀下才能被 iframe 加载。
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, adminPrefix)
	rest = strings.Trim(rest, "/")

	switch {
	case rest == "" || rest == "index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(adminHTML)
	case rest == "api/config":
		s.adminConfig(w, r)
	case rest == "api/status":
		s.adminStatus(w, r)
	case rest == "api/test":
		s.adminTest(w, r)
	case rest == "api/sources":
		writeJSON(w, http.StatusOK, map[string]any{"sources": knownSources})
	default:
		http.NotFound(w, r)
	}
}

// requireWrite 拦住跨站表单提交。
//
// 管理页本身只有本机 root 能连（Unix socket 权限），但浏览器里的第三方页面
// 仍可能诱导管理员发起请求。要求 JSON content-type 是最省事的一道门：
// HTML 表单无法伪造它，跨域 fetch 又会被同源策略挡在读取之外。
func requireWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return false
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "请求需为 application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// configView 是给前端的配置视图。
// MeowToken 只回传"是否已设置"，绝不把明文送进浏览器——管理页截图、
// 浏览器扩展、剪贴板同步都可能把它带出去。
type configView struct {
	MeowURL        string   `json:"meow_url"`
	TokenSet       bool     `json:"token_set"`
	Sources        []string `json:"sources"`
	SearchLimit    int      `json:"search_limit"`
	OnlineEnabled  bool     `json:"online_enabled"`
	SearchTimeout  int      `json:"search_timeout_s"`
	StreamTimeout  int      `json:"stream_timeout_s"`
	LyricTimeout   int      `json:"lyric_timeout_s"`
	TargetSocket   string   `json:"target_socket"`
	UpstreamSocket string   `json:"upstream_socket"`
	Version        string   `json:"version"`
}

func (s *Server) adminConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := s.Config()
		writeJSON(w, http.StatusOK, configView{
			MeowURL:        cfg.MeowURL,
			TokenSet:       cfg.MeowToken != "",
			Sources:        cfg.Sources,
			SearchLimit:    cfg.SearchLimit,
			OnlineEnabled:  cfg.OnlineEnabled,
			SearchTimeout:  cfg.SearchTimeout,
			StreamTimeout:  cfg.StreamTimeout,
			LyricTimeout:   cfg.LyricTimeout,
			TargetSocket:   cfg.TargetSocket,
			UpstreamSocket: cfg.UpstreamSocket,
			Version:        Version,
		})
		return
	}
	if !requireWrite(w, r) {
		return
	}

	// 部分更新：前端只提交改过的字段，未提交的保持原值。
	// 这样 Config 结构体将来加字段也不会让老页面把它清空。
	var body struct {
		MeowURL       *string   `json:"meow_url"`
		MeowToken     *string   `json:"meow_token"`
		ClearToken    bool      `json:"clear_token"`
		Sources       *[]string `json:"sources"`
		SearchLimit   *int      `json:"search_limit"`
		OnlineEnabled *bool     `json:"online_enabled"`
		SearchTimeout *int      `json:"search_timeout_s"`
		StreamTimeout *int      `json:"stream_timeout_s"`
		LyricTimeout  *int      `json:"lyric_timeout_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON：" + err.Error()})
		return
	}

	cfg := s.Config()
	if body.MeowURL != nil {
		cfg.MeowURL = strings.TrimSpace(*body.MeowURL)
	}
	if body.ClearToken {
		cfg.MeowToken = ""
	} else if body.MeowToken != nil && strings.TrimSpace(*body.MeowToken) != "" {
		cfg.MeowToken = strings.TrimSpace(*body.MeowToken)
	}
	if body.Sources != nil {
		clean := make([]string, 0, len(*body.Sources))
		seen := map[string]bool{}
		for _, v := range *body.Sources {
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "" && !seen[v] {
				seen[v] = true
				clean = append(clean, v)
			}
		}
		sort.Strings(clean)
		cfg.Sources = clean
	}
	if body.SearchLimit != nil {
		cfg.SearchLimit = *body.SearchLimit
	}
	if body.OnlineEnabled != nil {
		cfg.OnlineEnabled = *body.OnlineEnabled
	}
	if body.SearchTimeout != nil {
		cfg.SearchTimeout = *body.SearchTimeout
	}
	if body.StreamTimeout != nil {
		cfg.StreamTimeout = *body.StreamTimeout
	}
	if body.LyricTimeout != nil {
		cfg.LyricTimeout = *body.LyricTimeout
	}

	if err := s.ApplyConfig(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()

	// 官方探活用短超时：管理页要秒开，不能因为官方卡住就一直转圈。
	upCtx, upCancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer upCancel()
	upErr := s.upstream.Probe(upCtx)

	meowErrText := ""
	if cfg.OnlineEnabled {
		mCtx, mCancel := timeoutCtx(r, 4*time.Second)
		defer mCancel()
		if err := s.meowClient().Health(mCtx); err != nil {
			meowErrText = err.Error()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":  Version,
		"uptime_s": int(time.Since(s.startedAt).Seconds()),
		"online":   cfg.OnlineEnabled,
		"guid_map": s.guids.Size(),
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
	})
}

// adminTest 测试 Meow 连通性，并顺带跑一次真实搜索。
//
// 只做 /healthz 探活是不够的：服务活着但音源全挂、或地址指错了站点，
// 健康检查都会通过。跑一次真搜索并把前几条结果回给用户，才能真正判断"能用"。
func (s *Server) adminTest(w http.ResponseWriter, r *http.Request) {
	if !requireWrite(w, r) {
		return
	}
	var body struct {
		MeowURL string `json:"meow_url"`
		Query   string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	cfg := s.Config()
	testURL := strings.TrimSpace(body.MeowURL)
	if testURL == "" {
		testURL = cfg.MeowURL
	}
	query := strings.TrimSpace(body.Query)
	if query == "" {
		query = "晴天"
	}

	probe := cfg
	probe.MeowURL = testURL
	_ = probe.Validate()
	client := NewMeowClient(probe)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	result := map[string]any{"ok": false, "url": testURL}
	if err := client.Health(ctx); err != nil {
		result["error"] = "健康检查失败：" + err.Error()
		writeJSON(w, http.StatusOK, result)
		return
	}
	tracks, err := client.Search(ctx, query, 5, nil)
	if err != nil {
		result["error"] = "搜索失败：" + err.Error()
		writeJSON(w, http.StatusOK, result)
		return
	}
	samples := make([]map[string]any, 0, len(tracks))
	for i, t := range tracks {
		if i >= 5 {
			break
		}
		samples = append(samples, map[string]any{
			"title":  t.Title,
			"artist": t.Artist,
			"source": t.Source,
		})
	}
	result["ok"] = true
	result["query"] = query
	result["count"] = len(tracks)
	result["samples"] = samples
	if len(tracks) == 0 {
		result["warning"] = "连通正常，但这个关键词没有返回结果：检查 Meow 侧音源是否启用"
	}
	writeJSON(w, http.StatusOK, result)
}
