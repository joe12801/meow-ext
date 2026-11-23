package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// YouTubeClient 对接自建的 "Music MP3 API"（yt-dlp + ffmpeg）。
//
// 为什么要单独接一个服务，而不是让 Meow 来搜：Meow 只内置了国内那 9 个音源，
// 二进制里没有 YouTube 相关实现，也没法扩展。所以 YouTube 只能由扩展自己
// 接一条"YouTube 链接 → MP3"的转换链路：
//
//	/api/search  搜视频（标题/时长/频道/缩略图）
//	/api/music   取音频，支持 Range（拖动进度条靠它）
//	/api/lyrics  取逐行歌词（lrclib），转成 LRC 给飞牛
//
// 服务端自己带缓存（首次下载转码，之后直接命中），所以重复点同一首很快；
// 它还有时长上限（默认 900 秒），超长视频会在下载阶段直接失败——因此搜索阶段
// 就按 YoutubeMaxDuration 先挡掉，别让用户点了才看到错误。
type YouTubeClient struct {
	base        string
	key         string
	bitrate     int
	maxDuration int
	http        *http.Client
}

// youtubeSourceKey 是音源键名。它不出现在 Meow 的 sources 里，
// 但会和 Meow 的音源一起出现在管理页勾选项里，用同一个 Key 空间。
const youtubeSourceKey = "youtube"

// NewYouTubeClient 在未配置地址时返回 nil——调用方据此跳过整个 YouTube 分支，
// 不必到处判断"有没有配地址"。
func NewYouTubeClient(cfg Config) *YouTubeClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.YoutubeAPIURL), "/")
	if base == "" {
		return nil
	}
	bitrate := cfg.YoutubeBitrate
	if bitrate <= 0 {
		bitrate = 192
	}
	return &YouTubeClient{
		base:        base,
		key:         strings.TrimSpace(cfg.YoutubeAPIKey),
		bitrate:     bitrate,
		maxDuration: cfg.YoutubeMaxDuration,
		http: &http.Client{
			// 不设整体超时：/api/music 首次要下载整条音轨再转码，
			// 时间由调用方传入的 ctx 控制（取流用 StreamTimeout）。
			Transport: &http.Transport{
				MaxIdleConns:        8,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

func (c *YouTubeClient) newRequest(ctx context.Context, path string, q url.Values) (*http.Request, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// ytSearchResponse 对应 /api/search 的返回。
type ytSearchResponse struct {
	Query   string   `json:"query"`
	Count   int      `json:"count"`
	Results []ytItem `json:"results"`
}

type ytItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Channel  string `json:"channel"`
	Duration int    `json:"duration"`
	Thumb    string `json:"thumbnail"`
	Webpage  string `json:"webpage_url"`
}

// Search 搜 YouTube 并把结果转成与 Meow 一致的结构（MeowTrack），
// 这样上层合并、去重、下发 id 的逻辑完全不用区分来源。
//
// 转成 MeowTrack 时把 video id 放进 URL 的 vid= 参数：
// trackToRef 会把 /stream_live 之外的东西整段存进 Ref.Path，
// 但 YouTube 的播放必须走 /api/music 而不是 Meow 的 /stream_live，
// 所以这里约定用 source=youtube + XID=videoID 来还原（见 trackToRef）。
func (c *YouTubeClient) Search(ctx context.Context, query string, limit int) ([]MeowTrack, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}
	// 服务端搜索上限 20；多要一秒也不会更快，只会白等。
	if limit > 20 {
		limit = 20
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(limit))
	req, err := c.newRequest(ctx, "/api/search", q)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &MeowError{Msg: "连接 mp3api 失败：" + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &MeowError{Status: resp.StatusCode, Msg: "读取 mp3api 响应失败：" + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &MeowError{Status: resp.StatusCode, Msg: "mp3api 搜索失败：" + snippet(body)}
	}
	var parsed ytSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &MeowError{Status: resp.StatusCode, Msg: "mp3api 搜索响应不是预期的 JSON"}
	}

	out := make([]MeowTrack, 0, len(parsed.Results))
	skipped := 0
	for _, item := range parsed.Results {
		id := strings.TrimSpace(item.ID)
		title := strings.TrimSpace(item.Title)
		if id == "" || title == "" {
			continue
		}
		// 超长视频直接丢：服务端下载会因时长上限失败，
		// 留着只会让用户点了看到一句错误。
		if c.maxDuration > 0 && item.Duration > c.maxDuration {
			skipped++
			continue
		}
		out = append(out, MeowTrack{
			Title:    title,
			Artist:   strings.TrimSpace(item.Channel),
			Source:   youtubeSourceKey,
			Duration: item.Duration,
			CoverURL: strings.TrimSpace(item.Thumb),
			Link:     strings.TrimSpace(item.Webpage),
			URL:      youtubePlayPath(id),
		})
	}
	if skipped > 0 {
		log.Printf("[youtube] %q 跳过 %d 条超长视频（>%d 秒）", query, skipped, c.maxDuration)
	}
	return out, nil
}

// youtubePlayPath 生成一个能被 trackToRef 正确解析的"播放地址"。
//
// 不用 vid 参数而是复用 Meow 的 xid 约定：trackToRef 已经把
// stream_live?…&xid=… 解析成 Ref.XID，YouTube 的 video id 放同一个字段，
// 取流时按 Source 分流即可。这样 id 编码只有一套，不必为 YouTube 另开格式。
func youtubePlayPath(videoID string) string {
	return "/stream_live?song=&singer=&xid=" + url.QueryEscape(videoID)
}

// PlayURL 把 video id 还原成 mp3api 的取流地址。
func (c *YouTubeClient) PlayURL(videoID string) string {
	q := url.Values{}
	q.Set("video_id", videoID)
	q.Set("bitrate", strconv.Itoa(c.bitrate))
	return c.base + "/api/music?" + q.Encode()
}

// Stream 请求 /api/music，返回未读取的响应体交给上层透传。
//
// 和 Meow 的 Stream 一样只把 5xx 当错误：4xx（时长超限、视频不可用、
// API Key 失效）里服务端给的是 JSON 说明，原样送给客户端比自编一句
// "取流失败"有用得多。
func (c *YouTubeClient) Stream(ctx context.Context, method, videoID, rangeHeader string) (*http.Response, error) {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return nil, &MeowError{Msg: "缺少 YouTube 视频 id"}
	}
	if method != http.MethodHead {
		method = http.MethodGet
	}
	u := c.PlayURL(videoID)
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	req.Header.Set("Accept", "*/*")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &MeowError{Msg: "连接 mp3api 取流失败：" + err.Error()}
	}
	if resp.StatusCode >= 500 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &MeowError{Status: resp.StatusCode, Msg: snippet(body)}
	}
	return resp, nil
}

// ytLyrics 对应 /api/lyrics 的返回。lines 里的 text 已含演唱者标注
// （如 "[周]半夜睡不著覺"），直接当歌词正文用。
type ytLyrics struct {
	Available bool     `json:"available"`
	Source    string   `json:"source"`
	Lines     []ytLine `json:"lines"`
}

type ytLine struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Lyric 取歌词并转成 LRC。
//
// 飞牛客户端吃的是 LRC 文本（[mm:ss.xx]歌词），而 mp3api 给的是结构化行，
// 转换只能在这里做。没有歌词（纯音乐、无人上传）时返回空串而不是错误——
// 客户端收到错误码会弹提示，而"这首歌没歌词"是正常状态。
func (c *YouTubeClient) Lyric(ctx context.Context, videoID string) (string, error) {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return "", nil
	}
	q := url.Values{}
	q.Set("video_id", videoID)
	q.Set("lang", "auto")
	req, err := c.newRequest(ctx, "/api/lyrics", q)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &MeowError{Msg: "连接 mp3api 取歌词失败：" + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &MeowError{Status: resp.StatusCode, Msg: "取歌词失败：" + snippet(body)}
	}
	var parsed ytLyrics
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", &MeowError{Status: resp.StatusCode, Msg: "歌词响应不是预期的 JSON"}
	}
	if !parsed.Available || len(parsed.Lines) == 0 {
		return "", nil
	}
	return linesToLRC(parsed.Lines), nil
}

// linesToLRC 把 {start,end,text} 行转成 LRC 文本。
//
// 先按 start 排序：接口在不同来源（字幕/自动字幕/lrclib）下顺序不保证，
// 而 LRC 是"按时间顺序高亮"的格式，乱序会让歌词面板跳来跳去。
func linesToLRC(lines []ytLine) string {
	sorted := make([]ytLine, len(lines))
	copy(sorted, lines)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	var b strings.Builder
	for _, ln := range sorted {
		text := strings.TrimSpace(ln.Text)
		if text == "" {
			continue
		}
		if ln.Start < 0 {
			ln.Start = 0
		}
		total := ln.Start
		mm := int(total) / 60
		ss := total - float64(mm*60)
		b.WriteString(fmt.Sprintf("[%02d:%05.2f]%s\n", mm, ss, text))
	}
	return b.String()
}

// Health 探测 mp3api 是否可用。
func (c *YouTubeClient) Health(ctx context.Context) error {
	req, err := c.newRequest(ctx, "/api/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &MeowError{Msg: "连接 mp3api 失败：" + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return &MeowError{Status: resp.StatusCode, Msg: "mp3api 健康检查失败：" + snippet(body)}
	}
	// 服务端把依赖状态（ffmpeg 是否可用、yt-dlp 版本）放在 /api/health 里，
	// 顺手校验一下：ffmpeg 缺失时搜索能过但下载必然失败，属于"看起来能用"的假象。
	var parsed struct {
		Status   string `json:"status"`
		Download struct {
			FFmpeg bool `json:"ffmpeg_available"`
		} `json:"download"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Status != "" && parsed.Status != "ok" {
			return &MeowError{Msg: "mp3api 状态异常：" + parsed.Status}
		}
		if !parsed.Download.FFmpeg {
			return &MeowError{Msg: "mp3api 未检测到 ffmpeg，无法转码"}
		}
	}
	return nil
}
