package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// 在线曲目的歌词接口。
//
// 客户端真正打的是 GET /music/api/v1/lyric/list?trackGUID=<id>，
// 而不是早先猜的 /track/lyrics——这一点是从 nginx 访问日志里数出来的：
// 每次播放都会紧跟一条 lyric/list，状态 200、响应体只有 55 字节
// （官方对未知 guid 回的 {"code":99999}）。所以照片与歌词"都没有"，
// 不是取词失败，而是请求压根没进到扩展里。
//
// 响应结构对齐官方：data.list 里每条要有非空 content，
// source=2 表示 EXTERNAL_LRC（非内嵌歌词，不强制 offset），preferred 指向要用的那条。
type lyricItem struct {
	GUID      string `json:"guid"`
	Content   string `json:"content"`
	Source    int    `json:"source"`
	IsLRC     bool   `json:"isLRC"`
	Offset    int    `json:"offset"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// handleLyricList 是 /music/api/v1/lyric/list 的实现。
func (s *Server) handleLyricList(w http.ResponseWriter, r *http.Request, pathGUID string) {
	guid := extractGUID(r, pathGUID)
	ref, ok := s.resolveMeowRef(guid)
	if !ok {
		// 不是我们的 id：官方怎么答就怎么答（本地曲目的歌词走官方那条链路）。
		s.upstream.ServeHTTP(w, r)
		return
	}

	text := s.lyricFor(r.Context(), ref)
	data := map[string]any{"list": []lyricItem{}, "preferred": ""}
	if strings.TrimSpace(text) != "" {
		now := time.Now().Unix()
		lyricGUID := guid + ":lyric"
		data["list"] = []lyricItem{{
			GUID:      lyricGUID,
			Content:   text,
			Source:    2,
			IsLRC:     true,
			Offset:    0,
			CreatedAt: now,
			UpdatedAt: now,
		}}
		data["preferred"] = lyricGUID
	}
	// 没取到也回 code:0 + 空列表：客户端拿到错误码会弹提示，
	// 而"这首歌没歌词"是正常状态，不该被当成故障。
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "ok", "data": data})
}

// lyricSourceAllowed 决定要不要按"歌名+歌手"去检索歌词。
//
// 只对真正的音乐源这么做。B 站、喜马拉雅、荔枝上多半是视频教程、有声书、
// 电台节目，它们的标题会被 Meow 当成歌名去搜，结果配上一首毫不相干的歌词
// （实测《一直很安静》指弹教程配到了李永昌的情歌）——错歌词比没歌词更糟，
// 用户会以为扩展在乱码。这几类内容宁可回到空歌词。
func lyricSourceAllowed(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", "kuwo", "netease", "kugou", "qq", "qishui", "fivesing", "ext":
		return true
	}
	return false
}

// lyricFor 按来源取歌词：YouTube → mp3api，音乐源 → Meow，最后拿网易云兜底。
//
// 每一段都有独立的时间预算。这不是过度设计：Meow 的 /api/lyric 是冷启动
// 重新检索的，罕见标题实测要 3 秒以上，早先没设上限时整个歌词请求能拖到 12 秒，
// 客户端那段时间就是一个转圈的空白面板。
func (s *Server) lyricFor(ctx context.Context, ref MeowRef) string {
	cfg := s.Config()
	total := time.Duration(cfg.LyricTimeout) * time.Second
	if total <= 0 {
		total = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	if ref.Source == youtubeSourceKey {
		if yt := s.youtubeClient(); yt != nil {
			if text, err := yt.Lyric(ctx, ref.XID); err == nil && strings.TrimSpace(text) != "" {
				return text
			}
		}
	} else if lyricSourceAllowed(ref.Source) && strings.TrimSpace(ref.Song) != "" {
		// 给 Meow 一半预算：它命中缓存时是毫秒级，冷启动要给够时间。
		step := total / 2
		if step > 3*time.Second {
			step = 3 * time.Second
		}
		mctx, mcancel := context.WithTimeout(ctx, step)
		text, err := s.meowClient().Lyric(mctx, ref.Song, ref.Singer)
		mcancel()
		if err == nil && strings.TrimSpace(text) != "" {
			return text
		}
		if err != nil {
			// 超时是预期内的降级路径（Meow 冷启动常年超出 3 秒预算，
			// 后续会落到网易云并通常能拿到词），打成"失败"会让排障的人误判。
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				log.Printf("[lyrics] Meow 取词超出 %s 预算，转网易云兜底 song=%q", step, ref.Song)
			} else {
				log.Printf("[lyrics] Meow 取词失败 song=%q: %v", ref.Song, err)
			}
		}
	} else if !lyricSourceAllowed(ref.Source) {
		// 视频/有声书/电台：连网易云兜底也不做。它们的标题太容易"包含"到
		// 某首真实的歌（《…长城谣》→《长城谣》），配上就是牛头不对马嘴。
		return ""
	}

	if !cfg.MetaEnabled || strings.TrimSpace(ref.Song) == "" {
		return ""
	}
	info := s.metaLookup(ctx, ref, total)
	if info.Lyric != "" {
		return info.Lyric
	}
	if info.NeID <= 0 {
		return ""
	}
	// 命中缓存但歌词还没取过：这时才去拉，并写回缓存。
	text := s.neteaseLyric(ctx, info.NeID)
	if text != "" {
		key, _ := metaKeyFor(ref)
		s.meta.SetLyric(key, text)
	}
	return text
}

// coverStrict 决定封面检索要不要"只认完全同名"。
//
// 有声书与电台的标题常常长到把一首真歌名包在里面（"《一颗…树·长城谣》
// 席慕容朗诵"），宽松匹配会把它们配到那首歌的专辑封面上，牛头不对马嘴。
// B 站不同：上面大量是歌曲 MV / 翻唱，标题带括号备注很正常，
// 宽松匹配反而更容易配到正确的那张专辑图。
func coverStrict(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "ximalaya", "lizhi":
		return true
	}
	return false
}

// metaKeyFor 给出补全缓存的键，以及这次检索要不要"只认完全同名"。
func metaKeyFor(ref MeowRef) (string, bool) {
	strict := coverStrict(ref.Source)
	key := metaKey(ref.Song, ref.Singer)
	if strict {
		key += "\x1fstrict"
	}
	return key, strict
}

// metaLookup 按曲目取补全信息（缓存键与严格度都由来源决定）。
func (s *Server) metaLookup(ctx context.Context, ref MeowRef, wait time.Duration) metaInfo {
	key, strict := metaKeyFor(ref)
	return s.meta.Get(ctx, key, wait, func(ctx context.Context) (metaInfo, bool) {
		return s.fetchMetaInfo(ctx, ref.Song, ref.Singer, strict)
	})
}

// metaPeek 只读缓存版本，用于不阻塞的场合（比如 metadata 里的专辑名）。
func (s *Server) metaPeek(ref MeowRef) metaInfo {
	key, _ := metaKeyFor(ref)
	return s.meta.Peek(key)
}

// coverCandidates 给出这首歌可能的海报地址（按优先级）。
//
// YouTube 是确定性地址：同一个 video id 的缩略图是固定 URL，
// 不用检索也不会有歧义，maxresdefault 不存在时退 hqdefault。
// 其它源只能靠网易云按"歌名+歌手"检索，检索不到就没有候选 → 回音源 LOGO。
func (s *Server) coverCandidates(ctx context.Context, ref MeowRef, wait time.Duration) []string {
	if ref.Source == youtubeSourceKey {
		if ref.XID == "" {
			return nil
		}
		return []string{
			"https://i.ytimg.com/vi/" + ref.XID + "/maxresdefault.jpg",
			"https://i.ytimg.com/vi/" + ref.XID + "/hqdefault.jpg",
		}
	}
	if !s.Config().MetaEnabled || strings.TrimSpace(ref.Song) == "" {
		return nil
	}
	info := s.metaLookup(ctx, ref, wait)
	if info.Cover == "" {
		return nil
	}
	return []string{info.Cover}
}

// warmCovers 在搜索返回后后台预热封面。
//
// 目的是让用户滚到列表时封面已经在缓存里：否则每一格的第一次请求都得先
// 等一次外网检索，列表会先铺满 LOGO 再逐个换图，观感上就是"一直没有海报"。
// Get 的 wait=0 只触发后台解析、立即返回，所以这里不需要另开 goroutine 池，
// 真正的并发限流在 resolver 内部。
func (s *Server) warmCovers(tracks []MeowTrack) {
	if !s.Config().MetaEnabled {
		return
	}
	const maxWarm = 12
	warmed := 0
	for _, t := range tracks {
		if warmed >= maxWarm {
			break
		}
		if t.Source == youtubeSourceKey || strings.TrimSpace(t.Title) == "" {
			continue
		}
		warmed++
		ref := trackToRef(t)
		s.metaLookup(context.Background(), ref, 0)
	}
	if warmed > 0 {
		log.Printf("[cover] 预热 %d 首曲目的封面", warmed)
	}
}
