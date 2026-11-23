package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// guidQueryKeys 是飞牛客户端可能用来传曲目 id 的查询参数名。
//
// 这不是想象力过剩：Web 端用 guid，App 收藏回读用 trackGUID，封面接口用 coverId。
// 漏掉任何一个，对应场景就会静默回退成"官方找不到这个 id"。
var guidQueryKeys = []string{"guid", "trackGUID", "trackGuid", "coverId", "id", "trackId"}

// extractGUID 从路径或查询串里取出曲目 id。
func extractGUID(r *http.Request, pathGUID string) string {
	if pathGUID != "" {
		return pathGUID
	}
	q := r.URL.Query()
	for _, k := range guidQueryKeys {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// resolveMeowRef 把客户端传来的 id 解析成 Meow 取流参数。
//
// 接受两种形态：伪装后的 32-hex（客户端实际持有的），以及原始的
// online:meow:<base64>（排障时手工构造的）。两者都能用，省去排障时还要
// 反查映射表。
func (s *Server) resolveMeowRef(id string) (MeowRef, bool) {
	if id == "" {
		return MeowRef{}, false
	}
	real, ok := s.guids.Resolve(id)
	if !ok {
		return MeowRef{}, false
	}
	return DecodeGUID(real)
}

// handleStream 是 /music/api/v1/track/stream 的实现。
//
// 转码、缓存、失败重试全部交给 Meow 的 /stream_live，扩展只做三件事：
// 把 id 还原成取流参数、把 Range 头带过去、把响应头和字节流原样送回。
// 这样做的直接好处是拖动进度条可用——Meow 命中缓存后走 http.ServeContent，
// 本身就能出 206 与 Content-Range。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, pathGUID string) {
	ref, ok := s.resolveMeowRef(extractGUID(r, pathGUID))
	if !ok {
		s.upstream.ServeHTTP(w, r)
		return
	}

	// 转码可能跑几百秒，用独立的 ctx：既不受客户端先断开时的取消影响
	// （让 Meow 把这次转码跑完，下次点歌就命中缓存了），也不设整体超时
	// （超时由 Meow 自己的额度检查与失败重试兜底）。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
		time.Duration(s.cfg.StreamTimeout)*time.Second)
	defer cancel()

	rangeHeader := ""
	if s.cfg.StreamDirectRanges {
		rangeHeader = r.Header.Get("Range")
	}
	start := time.Now()

	// 按来源分流：YouTube 走自建的 mp3api（/api/music），Meow 走 /stream_live。
	// 两条链路都支持 Range，但地址和鉴权完全不同，混在一起会把视频 id
	// 当成歌名发给 Meow，拿回一句没头没尾的"解析不到直链"。
	var (
		resp *http.Response
		err  error
	)
	if ref.Source == youtubeSourceKey {
		yt := s.youtubeClient()
		if yt == nil {
			http.Error(w, "YouTube 源未配置：请在管理页填写 mp3api 地址", http.StatusServiceUnavailable)
			return
		}
		resp, err = yt.Stream(ctx, r.Method, ref.XID, rangeHeader)
	} else {
		resp, err = s.meow.Stream(ctx, r.Method, ref.playPath(), rangeHeader)
	}
	if err != nil {
		status := meowErrStatus(err)
		log.Printf("[stream] 取流失败 song=%q singer=%q xid=%q bv=%q: %v",
			ref.Song, ref.Singer, ref.XID, ref.BV, err)
		// 走到这里的只有上游 5xx 与连接失败。4xx（额度用尽、解析不到直链）
		// 已在 Stream 里原样返回，不会落到这个分支。
		http.Error(w, err.Error(), status)
		return
	}
	defer resp.Body.Close()

	okStatus := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent

	// 只搬影响播放的头。Content-Length 与 Content-Range 必须原样保留：
	// 前者决定客户端知道何时读完，后者是 206 的分段依据。
	for _, k := range []string{
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Last-Modified", "ETag", "X-Source", "X-Guest-Quota", "Retry-After",
	} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	// 默认值只在真正要出音频时补：4xx 的响应体是上游的说明文字，
	// 给它贴个 audio/mpeg 会让客户端把它当音频解出满屏噪音。
	if okStatus {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "audio/mpeg")
		}
		// 无论上游是否声明，都补上 Accept-Ranges：客户端据此判断能否拖动。
		if w.Header().Get("Accept-Ranges") == "" {
			w.Header().Set("Accept-Ranges", "bytes")
		}
	}
	w.WriteHeader(resp.StatusCode)

	if !okStatus {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("[stream] 上游拒绝 song=%q 状态=%d 等待=%s 提示=%s",
			ref.Song, resp.StatusCode, w.Header().Get("Retry-After"), snippet(body))
		w.Write(body)
		return
	}

	if r.Method == http.MethodHead {
		return
	}
	n, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		// 客户端拖动进度条会主动断连，这不是错误，不刷日志。
		if !isClientGone(copyErr) {
			log.Printf("[stream] 传输中断 song=%q 已发送 %d 字节: %v", ref.Song, n, copyErr)
		}
		return
	}
	log.Printf("[stream] 完成 song=%q singer=%q 状态=%d 大小=%d 用时=%dms",
		ref.Song, ref.Singer, resp.StatusCode, n, time.Since(start).Milliseconds())
}

// handleLyrics 是 /music/api/v1/track/lyrics 的实现。
//
// 飞牛 1.1.x 的播放页实际打的是 /lyric/list（见 lyric.go），
// 这个路径是给旧版客户端与手工排障留的，两边共用同一份取词逻辑。
func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request, pathGUID string) {
	ref, ok := s.resolveMeowRef(extractGUID(r, pathGUID))
	if !ok {
		s.upstream.ServeHTTP(w, r)
		return
	}
	text := s.lyricFor(r.Context(), ref)
	// 即便没取到也返回 code:0 + 空串：客户端收到错误码会弹提示，
	// 而"这首纯音乐没有歌词"不该被当成故障。
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "ok",
		"data": map[string]any{
			"guid":  extractGUID(r, pathGUID),
			"lyric": text,
			"lrc":   text,
		},
	})
}

// 封面相关的两种缓存策略。
//
// 海报一旦出图就不会变，可以长缓存；LOGO 是"海报还没解析好"的占位，
// 必须短缓存——给它 immutable 一周，客户端会把占位图钉死在那里，
// 之后就算海报已经就绪也永远看不到，正好是用户反馈的"只有音源 LOGO"。
const (
	posterCacheControl = "public, max-age=86400"
	logoCacheControl   = "public, max-age=60"
)

// coverWait 决定"为一首歌等海报多久"。
//
// 播放页的大图是用户真正会盯着看的地方，值得多等一会儿；
// 列表缩略图也别给太短：客户端按 URL 做内存缓存，一旦先拿到 LOGO，
// 同一个格子在这次会话里就不会再请求第二次，海报要重开页面才出现。
func coverWait(size int) time.Duration {
	if size >= 300 {
		return 2000 * time.Millisecond
	}
	return 1200 * time.Millisecond
}

// handleCover 把在线曲目的封面转给客户端。
//
// 直接把客户端的封面请求重定向到源站也可以，但会踩两个坑：
// 源站常校验 Referer/UA、且不少 CDN 不允许跨域。走本进程代理一次最省心。
//
// 三级回退：真实海报（网易云专辑图 / YouTube 缩略图）→ 音源 LOGO → 官方。
// LOGO 只是占位，配短缓存，海报就绪后下一次请求就会替换掉。
func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	coverURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if coverURL == "" {
		for _, k := range []string{"coverUrl", "coverURL", "cover_url", "src"} {
			if v := strings.TrimSpace(r.URL.Query().Get(k)); v != "" {
				coverURL = v
				break
			}
		}
	}
	// 有真实封面地址就代理它。
	if coverURL != "" && (strings.HasPrefix(coverURL, "http://") || strings.HasPrefix(coverURL, "https://")) {
		if s.proxyImage(w, r, coverURL, posterCacheControl) {
			return
		}
	}

	id := coverIDParam(r)
	ref, known := s.coverRef(id)
	if !known {
		// 官方 /static/cover 只认自己库里的 coverId，认不出就一定不是我们的。
		s.upstream.ServeHTTP(w, r)
		return
	}

	size := coverSize(r)
	ctx, cancel := context.WithTimeout(r.Context(), coverWait(size)+time.Second)
	defer cancel()
	for _, candidate := range s.coverCandidates(ctx, ref, coverWait(size)) {
		if s.proxyImage(w, r, withCoverSize(candidate, size), posterCacheControl) {
			return
		}
	}
	serveSourceLogo(w, r, ref.Source)
}

// coverRef 从封面 id 还原出曲目信息。
func (s *Server) coverRef(id string) (MeowRef, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return MeowRef{}, false
	}
	for _, c := range []string{id, stripCoverIDPrefixes(id)} {
		real, ok := s.guids.Resolve(c)
		if !ok {
			continue
		}
		ref, ok := DecodeGUID(real)
		if !ok {
			return MeowRef{}, true
		}
		return ref, true
	}
	return MeowRef{}, false
}

// coverSize 取客户端要的缩略图边长。飞牛列表用 120/160，播放页用 400~1600。
func coverSize(r *http.Request) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("size")))
	if err != nil || n <= 0 {
		return 0
	}
	if n > 1600 {
		n = 1600
	}
	return n
}

// withCoverSize 给网易云的封面地址拼尺寸参数。
//
// 不拼的话拿到的是原图（实测单张 7MB），一首歌的封面就能吃掉几秒带宽；
// 拼上 ?param=600y600 后只有 300KB。别的源不认这个参数，别乱加。
func withCoverSize(rawURL string, size int) string {
	if size <= 0 || !strings.Contains(rawURL, "music.126.net") {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "param=" + strconv.Itoa(size) + "y" + strconv.Itoa(size)
}

// proxyImage 把远端图片转发给客户端，返回是否真的出图了。
func (s *Server) proxyImage(w http.ResponseWriter, r *http.Request, imageURL, cacheControl string) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return false
	}
	// 部分音源站会拦空 UA。
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; meow-ext/1.0)")
	req.Header.Set("Referer", originOf(imageURL))
	resp, err := s.images.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// 只认图片：源站风控时常返回 200 的 HTML 页面，转给客户端就是一张裂图。
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return false
	}
	w.Header().Set("Content-Type", ct)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n <= 8<<20 {
			w.Header().Set("Content-Length", cl)
		}
	}
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.Copy(w, io.LimitReader(resp.Body, 8<<20))
	}
	return true
}

func originOf(raw string) string {
	if i := strings.Index(raw, "://"); i > 0 {
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return raw[:i+3+j]
		}
	}
	return raw
}

// isClientGone 判断写失败是否只是客户端主动断开。
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "client disconnected") ||
		strings.Contains(s, "use of closed network connection")
}
