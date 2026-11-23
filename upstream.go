package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream 是飞牛音乐官方后端的反向代理。
//
// 官方后端只监听一个 Unix socket（接管后位于 cfg.UpstreamSocket）。除了扩展
// 明确要适配的几个接口，其余一律原样转发——本地曲库、扫描、设置、播放列表
// 这些业务扩展一概不碰，字节级透传，避免任何"顺手改一下"带来的兼容问题。
type Upstream struct {
	sock  string
	proxy *httputil.ReverseProxy

	// healthy 只用于管理页与健康检查展示，不参与转发决策：
	// 官方短暂不可用时，转发失败会如实报错，而不是提前拒绝。
	healthy atomic.Bool
	lastErr atomic.Value // string

	mu sync.Mutex
}

func NewUpstream(sock string) *Upstream {
	u := &Upstream{sock: sock}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	u.proxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Host 固定成占位值：官方后端按路径路由，不校验 Host。
			// 保留客户端的原始 Host 反而可能让后端拼出错误的绝对地址。
			req.URL.Scheme = "http"
			req.URL.Host = "unix"
			req.Host = "unix"
		},
		Transport:     transport,
		FlushInterval: -1, // 立刻刷新：歌词/详情这类小响应不必等待攒批
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			u.setError(err)
			status, msg := classifyUpstreamError(err)
			http.Error(w, msg, status)
		},
		ErrorLog: nil,
	}
	return u
}

func (u *Upstream) setError(err error) {
	if err == nil {
		u.healthy.Store(true)
		u.lastErr.Store("")
		return
	}
	u.healthy.Store(false)
	u.lastErr.Store(err.Error())
}

// Healthy 返回最近一次探测/转发的结论。
func (u *Upstream) Healthy() bool { return u.healthy.Load() }

// LastError 返回最近一次失败原因（管理页展示）。
func (u *Upstream) LastError() string {
	if v, ok := u.lastErr.Load().(string); ok {
		return v
	}
	return ""
}

// ServeHTTP 透传。
func (u *Upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.proxy.ServeHTTP(w, r)
}

// Do 直接向官方发一次请求，供扩展需要读官方响应体的接口使用
// （搜索合并、曲目详情）。调用方负责关闭 resp.Body。
func (u *Upstream) Do(ctx context.Context, method, pathAndQuery string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	for k, vals := range header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Host = "unix"
	transport := u.proxy.Transport
	resp, err := transport.RoundTrip(req)
	if err != nil {
		u.setError(err)
		return nil, err
	}
	u.setError(nil)
	return resp, nil
}

// Probe 主动探活：向官方发一个极轻的请求。
//
// 不用 /healthz 之类专用端点——官方版本间不保证存在。搜索接口用一个不可能
// 有结果的词，既验证 socket 通、又验证后端真的在处理请求（而不是只accept不响应）。
func (u *Upstream) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := u.Do(ctx, http.MethodGet,
		"/music/api/v1/search/track?keyword=__meow_ext_probe__&page=1&size=1", nil)
	if err != nil {
		var netErr *net.OpError
		if errors.As(err, &netErr) {
			return fmt.Errorf("官方音乐后端不可达（%s）：请确认飞牛音乐已启动", u.sock)
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("官方音乐后端返回 %d", resp.StatusCode)
	}
	return nil
}

// classifyUpstreamError 区分"官方没起来"和"转发出错"。
// 前者是安装/启停顺序问题，要明确告诉用户去应用中心启动飞牛音乐；
// 后者才需要看日志。
func classifyUpstreamError(err error) (int, string) {
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return http.StatusGatewayTimeout, "官方音乐后端响应超时"
		}
		return http.StatusServiceUnavailable, "官方音乐后端不可达，请在应用中心确认飞牛音乐正在运行"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "官方音乐后端响应超时"
	}
	return http.StatusBadGateway, "转发到官方音乐后端失败"
}
