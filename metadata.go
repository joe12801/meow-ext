package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// handleTrackMetadata 给在线曲目补一份"官方格式"的元数据。
//
// 为什么必须自己实现：飞牛客户端点播放前会先 GET /music/api/v1/track/metadata?guid=，
// 拿不到就把这次播放判成 source-not-found 直接中止，根本走不到 /track/stream。
// 官方的 metadata 只认自己库里的 guid，我们下发的伪 id 在那边查不到，
// 于是表现就是"搜得到、点了没声音"——连一个请求都没发到取流接口。
//
// 响应结构照官方来：data 里是 {track, audioSpec} 两层，不是扁平曲目。
// 这与搜索列表的条目结构不同，前端有两套映射，混用会静默丢掉字段。
func (s *Server) handleTrackMetadata(w http.ResponseWriter, r *http.Request) {
	guid := strings.TrimSpace(r.URL.Query().Get("guid"))
	if guid == "" {
		guid = tailSegment(r.URL.Path, "/music/api/v1/track/metadata/")
	}
	real, ok := s.guids.Resolve(guid)
	if !ok || r.Method != http.MethodGet {
		// 不是我们的 id，或者是在改元数据（POST）：官方怎么处理就怎么处理。
		s.upstream.ServeHTTP(w, r)
		return
	}
	ref, ok := DecodeGUID(real)
	if !ok {
		s.upstream.ServeHTTP(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "",
		"data": buildTrackMetadata(guid, real, ref),
	})
}

// buildTrackMetadata 组元数据。
//
// guid 必须是客户端拿到的那个伪 id：前端会把 metadata 里的 track.guid 当作
// 播放用的 track.id，一旦换成真 id，后续取流就查不到映射了。
func buildTrackMetadata(guid, realID string, ref MeowRef) map[string]any {
	title := strings.TrimSpace(ref.Song)
	if title == "" {
		title = "未知曲目"
	}
	artist := strings.TrimSpace(ref.Singer)

	artists := make([]map[string]any, 0, 1)
	if artist != "" {
		artists = append(artists, map[string]any{"name": artist, "guid": guid + ":artist"})
	}

	// duration 单位是毫秒（前端会再除以 1000）。0 表示未知，
	// 播放器加载文件后会用真实时长覆盖，不会卡住播放。
	durationMs := 0
	if ref.Duration > 0 {
		durationMs = ref.Duration * 1000
	}

	// audioSpec.path 与搜索结果里的一致：前端拿它取扩展名、拼缓存路径，
	// 两边算法不同会导致同一个 guid 出现两个缓存文件。
	sum := sha256.Sum256([]byte(realID))
	specPath := hex.EncodeToString(sum[:]) + ".mp3"

	now := time.Now().Unix()
	return map[string]any{
		"track": map[string]any{
			"guid":       guid,
			"title":      title,
			"album":      "",
			"artists":    artists,
			"genres":     []string{},
			"coverId":    guid,
			"duration":   durationMs,
			"trackNo":    0,
			"discNo":     0,
			"year":       "",
			"isFavorite": false,
			"createdAt":  now,
			"updatedAt":  now,
		},
		// 在线源一律按 320k 的 mp3 报：只影响播放器选择解码策略与信息展示，
		// 真实码率以文件为准。缺这几个字段时前端会用搜索条目里的值兜底，
		// 但有的版本直接读 audioSpec，所以给全更稳。
		"audioSpec": map[string]any{
			"path":       specPath,
			"container":  "mp3",
			"codec":      "mp3",
			"bitrate":    320000,
			"bitDepth":   16,
			"channel":    2,
			"sampleRate": 44100,
			"duration":   durationMs,
			"size":       0,
		},
	}
}
