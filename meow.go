package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MeowClient 是 MeowMusicServer 的 HTTP 客户端。
//
// 只封装扩展真正需要的五个端点：/api/search、/stream_live、/api/lyric、
// /healthz，以及外部源直链代理 /api/ext_audio。其余能力（收藏、歌单、统计）
// 属于 Meow 自己的 Web UI，扩展不参与。
type MeowClient struct {
	base  string
	token string
	http  *http.Client
}

func NewMeowClient(cfg Config) *MeowClient {
	return &MeowClient{
		base:  cfg.MeowURL,
		token: cfg.MeowToken,
		http: &http.Client{
			// 不设 Client.Timeout：取流要走几百秒的转码，整体超时交给
			// 每次请求自带的 ctx 控制，否则会把"还在转码"误判成"连接超时"。
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          32,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 0,
				// Meow 的转码是边下边转，把它压进 gzip 只会让首字节更晚到。
				DisableCompression: true,
			},
		},
	}
}

// MeowTrack 对应 Meow /api/search 返回数组里的一个元素。
type MeowTrack struct {
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	URL      string `json:"url"`
	Link     string `json:"link"`
	LyricURL string `json:"lyric_url"`
	CoverURL string `json:"cover_url"`
	Duration int    `json:"duration"`
	Source   string `json:"source"`
}

// MeowError 带上游状态码，让上层能区分"额度用尽（429）"和"真故障"。
type MeowError struct {
	Status int
	Msg    string
}

func (e *MeowError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("Meow 上游返回 %d：%s", e.Status, e.Msg)
	}
	return e.Msg
}

func (c *MeowClient) newRequest(ctx context.Context, method, path string, query url.Values) (*http.Request, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// Search 调 Meow 的聚合搜索。
//
// sources 为空时不传 source 参数，等于让 Meow 用自己配置的全部启用音源；
// 这比在扩展里维护一份音源清单更耐用——上游增减音源不用跟着改扩展。
func (c *MeowClient) Search(ctx context.Context, query string, limit int, sources []string) ([]MeowTrack, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("query", query)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(sources) > 0 {
		q.Set("source", strings.Join(sources, ","))
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/search", q)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &MeowError{Msg: "连接 Meow 失败：" + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &MeowError{Status: resp.StatusCode, Msg: "读取 Meow 响应失败：" + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &MeowError{Status: resp.StatusCode, Msg: snippet(body)}
	}
	var tracks []MeowTrack
	if err := json.Unmarshal(body, &tracks); err != nil {
		return nil, &MeowError{Status: resp.StatusCode, Msg: "Meow 搜索响应不是预期的 JSON 数组"}
	}
	return tracks, nil
}

// Stream 请求 /stream_live 或外部源直链，返回未读取的响应体交给上层透传。
//
// 调用方负责 Close。Range 头原样转发——Meow 命中缓存时用 http.ServeContent
// 出流，本身就支持字节区间，扩展不必自己做分片。
//
// method 必须跟着客户端走，不能一律用 GET：Meow 的游客额度只对"从头开始的那次
// 请求"计数，HEAD 不计。若把客户端的 HEAD 探长改成 GET，每探一次白白消耗一首
// 额度——用户还没听到声音，今日 20 首就被探没了。
func (c *MeowClient) Stream(ctx context.Context, method, playPath, rangeHeader string) (*http.Response, error) {
	if strings.TrimSpace(playPath) == "" {
		return nil, &MeowError{Msg: "缺少播放地址"}
	}
	if method != http.MethodHead {
		method = http.MethodGet
	}
	if !strings.HasPrefix(playPath, "/") {
		playPath = "/" + playPath
	}
	req, err := c.newRequest(ctx, method, "", nil)
	if err != nil {
		return nil, err
	}
	// 直接拼 URL：playPath 已带 query，再走 url.Values 会二次编码。
	req.URL, err = url.Parse(c.base + playPath)
	if err != nil {
		return nil, &MeowError{Msg: "播放地址非法：" + err.Error()}
	}
	req.Header.Del("Accept")
	req.Header.Set("Accept", "*/*")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &MeowError{Msg: "连接 Meow 取流失败：" + err.Error()}
	}
	if resp.StatusCode >= 500 {
		// 只有 5xx 才算"上游挂了"：读一小段错误体当错误信息（限长，万一是
		// 误配置指向了某个返回整页 HTML 的服务），交给上层统一报 502。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &MeowError{Status: resp.StatusCode, Msg: snippet(body)}
	}
	// 4xx 原样返回，不包装成错误。
	// 典型是 429"游客每日试听 20 首已用完，登录后不限"和 404"解析不到直链"——
	// 上游那句提示往往正是用户唯一能看懂的解释，而 Retry-After / X-Guest-Quota
	// 这些头也是客户端判断"该等一小时还是换个源"的依据。自己编一句错误会把这些
	// 全部丢掉，用户只能看到一个没头没尾的 502。
	return resp, nil
}

// Lyric 取 LRC 歌词。Meow 在没有歌词时返回 404，这里统一成空串 + nil，
// 因为"这首歌没歌词"是正常状态，不该让客户端看到错误。
func (c *MeowClient) Lyric(ctx context.Context, song, singer string) (string, error) {
	if strings.TrimSpace(song) == "" {
		return "", nil
	}
	q := url.Values{}
	q.Set("song", song)
	if singer != "" {
		q.Set("singer", singer)
	}
	q.Set("format", "lrc")
	req, err := c.newRequest(ctx, http.MethodGet, "/api/lyric", q)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &MeowError{Msg: "连接 Meow 取歌词失败：" + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", &MeowError{Status: resp.StatusCode, Msg: "取歌词失败"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Health 探测 Meow 是否可用。/healthz 返回纯文本 ok。
func (c *MeowClient) Health(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &MeowError{Msg: "无法连接 " + c.base + "：" + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return &MeowError{Status: resp.StatusCode, Msg: snippet(body)}
	}
	if !strings.Contains(strings.ToLower(string(body)), "ok") {
		return &MeowError{Msg: "Meow 健康检查返回了非预期内容：" + snippet(body)}
	}
	return nil
}

// trackToRef 把一条搜索结果折算成 id 载荷。
//
// 绝大多数音源给的是 /stream_live?song=…&singer=…[&xid=…][&bv=…]，
// 这种拆成结构字段存，id 短且可读。外部源（YouTube / mp3api）给的是
// /api/ext_audio?… 之类带签名参数的地址，拆不开也不该拆——整段相对地址
// 原样存下来，播放时直接请求。
func trackToRef(t MeowTrack) MeowRef {
	ref := MeowRef{
		Song:   strings.TrimSpace(t.Title),
		Singer: strings.TrimSpace(t.Artist),
		Source: strings.TrimSpace(t.Source),
	}
	raw := strings.TrimSpace(t.URL)
	if raw == "" {
		raw = strings.TrimSpace(t.Link)
	}
	if raw == "" {
		return ref
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ref
	}
	if u.Path == "/stream_live" {
		q := u.Query()
		// song/singer 以 /api/search 的字段为准：url 里的可能是上游原始写法
		// （带书名号、全角括号），而 title/artist 已由 Meow 归一过。
		if s := strings.TrimSpace(q.Get("song")); s != "" && ref.Song == "" {
			ref.Song = s
		}
		if s := strings.TrimSpace(q.Get("singer")); s != "" && ref.Singer == "" {
			ref.Singer = s
		}
		ref.XID = strings.TrimSpace(q.Get("xid"))
		ref.BV = strings.TrimSpace(q.Get("bv"))
		return ref
	}
	// 非 /stream_live：保留 path+query（丢掉 host，避免把上游反代域名固化进 id）。
	ref.Path = u.Path
	if u.RawQuery != "" {
		ref.Path += "?" + u.RawQuery
	}
	return ref
}

// playPath 由 id 载荷还原出要向上游请求的相对地址。
func (r MeowRef) playPath() string {
	if r.Path != "" {
		return r.Path
	}
	q := url.Values{}
	q.Set("song", r.Song)
	if r.Singer != "" {
		q.Set("singer", r.Singer)
	}
	if r.XID != "" {
		q.Set("xid", r.XID)
	}
	if r.BV != "" {
		q.Set("bv", r.BV)
	}
	return "/stream_live?" + q.Encode()
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 180 {
		s = s[:180] + "…"
	}
	return s
}

// meowErrStatus 把上游错误映射成给客户端的状态码。
// 429 / 403 要原样保留，客户端据此提示"额度用完""需要会员"，
// 统一返回 500 会让用户以为是扩展坏了。
func meowErrStatus(err error) int {
	var me *MeowError
	if errors.As(err, &me) {
		switch me.Status {
		case http.StatusTooManyRequests, http.StatusForbidden, http.StatusNotFound,
			http.StatusUnauthorized, http.StatusBadRequest:
			return me.Status
		}
	}
	return http.StatusBadGateway
}
