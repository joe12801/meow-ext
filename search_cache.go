package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 在线搜索结果的短期缓存。
//
// 飞牛前端是"输入即搜"，这是从 nginx 访问日志里数出来的事实：同一次输入
// 会产生 keyword="someone like" → "someone like y" → "someone like yo"
// → "someone like you" 四条请求，每条都可能打满 8 个音源加一路 YouTube。
// 没有缓存时，用户打十个字就是十轮全量外网请求，日志里 search/suggest
// 那几次 499（客户端等不及自己断开）正是这么来的。
//
// 这里做两件事：
//
//  1. 精确命中：同一关键词 45 秒内重复搜索直接回缓存，不再发任何请求。
//  2. 前缀复用：用户继续输入时，先用"上一个更短关键词"的结果顶上，
//     真正的搜索延后 250ms 再发；若这 250ms 内用户又敲了下一个字，
//     这次搜索就作废。于是那四条请求最终只产生一次外网往返，
//     而用户看到的是列表立刻有内容，不必每个字都转圈。
//
// 只缓存"在线结果"这一层，官方结果永远实时穿透：本地曲库的新增与删除
// 必须立刻反映到搜索里，那不是可以延迟的东西。

const (
	// searchCacheTTL 是命中结果的存活时间。取 45 秒：足够覆盖"反复搜同一个词"
	// 与"输入中途回退"，又短到不会让刚上架的歌被旧结果盖住。
	searchCacheTTL = 45 * time.Second
	// searchCacheEmptyTTL 是"搜不到"结论的存活时间。短一些：
	// 空结果更可能是检索侧临时抽风，不该让它挡住半分钟。
	searchCacheEmptyTTL = 15 * time.Second
	// searchCacheMax 限制条目数。一条搜索结果几十条曲目、几 KB，
	// 100 条也就几百 KB。
	searchCacheMax = 100
	// searchRefreshDelay 是前缀复用后延迟刷新真实结果的时长。
	// 250ms 比人继续打下一个字的间隔（约 100~200ms）稍长，
	// 又短到用户松手后结果几乎立刻校正。
	searchRefreshDelay = 250 * time.Millisecond
	// searchWaitMax 是"等一个更短的同类搜索跑完"的上限。
	//
	// 这个值不能小：Meow 一次多源搜索实测要 2~3 秒，窗口设成 1.5 秒的话
	// 后面的字等不到就各自又搜一遍，等于白等。上限取搜索超时本身——
	// 用户在第一个字上本来就要等这么久，后面的字等它并不算出格。
	searchWaitMax = 4 * time.Second
)

// searchCacheKey 由"归一后的关键词 + 音源组合签名"构成。
//
// 音源签名必须进 key：同一个词在不同音源集合下结果不同，
// 混用会让用户勾了新音源却看不到它的结果（缓存把旧集合的结果顶上来）。
type searchCacheKey struct {
	q   string
	sig string
}

type searchCacheEntry struct {
	tracks []MeowTrack
	at     time.Time
}

type searchCache struct {
	mu    sync.Mutex
	items map[searchCacheKey]searchCacheEntry
	order []searchCacheKey
	// running 记录正在打外网的搜索。它的值是一个关闭即表示完成的 channel，
	// 用来做两件事：同一 key 不重复请求；更长的关键词等更短的那个先跑完，
	// 然后直接复用它的结果——否则用户打前几个字时，几个搜索会同时铺开，
	// 每个都打满一遍音源（日志里同一秒出现 "so"/"som"/"some" 就是这个）。
	running map[searchCacheKey]chan struct{}
	// lastQ 是最近一次真正发起搜索的关键词，用来判断后台刷新是否已经过时。
	lastQ string
}

func newSearchCache() *searchCache {
	return &searchCache{
		items:   make(map[searchCacheKey]searchCacheEntry),
		running: make(map[searchCacheKey]chan struct{}),
	}
}

// normalizeQuery 归一关键词：大小写与多余空白不该造成两次查询。
func normalizeQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// cacheSignature 把音源组合压成一个可比较的串。
//
// 排序是必须的：用户勾选顺序变化不代表结果不同，不排序会白白多存一份。
func cacheSignature(sources []string, ytOn bool, limit int) string {
	srcs := append([]string(nil), sources...)
	sort.Strings(srcs)
	yt := "0"
	if ytOn {
		yt = "1"
	}
	return strings.Join(srcs, ",") + "|yt=" + yt + "|n=" + strconv.Itoa(limit)
}

func (e searchCacheEntry) live(now time.Time) bool {
	ttl := searchCacheTTL
	if len(e.tracks) == 0 {
		ttl = searchCacheEmptyTTL
	}
	return now.Sub(e.at) < ttl
}

func cloneTracks(in []MeowTrack) []MeowTrack {
	if len(in) == 0 {
		return nil
	}
	out := make([]MeowTrack, len(in))
	copy(out, in)
	return out
}

// get 精确命中：同一关键词、同一音源集合、仍在 TTL 内。
func (c *searchCache) get(key searchCacheKey) ([]MeowTrack, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !e.live(time.Now()) {
		return nil, false
	}
	return cloneTracks(e.tracks), true
}

// prefix 找"当前关键词是某个已缓存关键词延长"的那一条，取最长的。
//
// 方向不能搞反：只允许"缓存的短词是当前长词的前缀"。
// 反过来（缓存 "abc" 去顶 "a"）会把一条具体搜索的结果当成宽泛搜索的结果，
// 用户搜 "a" 却看到 "abc" 的内容——那不是占位，那是错。
func (c *searchCache) prefix(key searchCacheKey) ([]MeowTrack, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var best searchCacheKey
	found := false
	for k, e := range c.items {
		if k.sig != key.sig || len(k.q) >= len(key.q) {
			continue
		}
		// 单字符前缀（用户刚打第一个字母）结果本身就是噪声，
		// 拿它当占位只会让人以为搜索坏了。
		if len(k.q) < 2 || !strings.HasPrefix(key.q, k.q) || !e.live(now) {
			continue
		}
		if !found || len(k.q) > len(best.q) {
			best, found = k, true
		}
	}
	if !found {
		return nil, false
	}
	return cloneTracks(c.items[best].tracks), true
}

// note 记录最近一次搜索的关键词。
func (c *searchCache) note(q string) {
	c.mu.Lock()
	c.lastQ = q
	c.mu.Unlock()
}

// stale 判断某个关键词是否已被更长的输入取代。
//
// 逐字输入时每个前缀都会排一次延迟刷新，只有最后一个能活下来：
// 前几个醒来时 lastQ 已经是更长的词，说明用户还在打字，直接作废。
func (c *searchCache) stale(q string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastQ == q {
		return false
	}
	return len(c.lastQ) > len(q) && strings.HasPrefix(c.lastQ, q)
}

// begin 声明某个 key 开始打外网，重复调用返回 false。
func (c *searchCache) begin(key searchCacheKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.running[key]; ok {
		return false
	}
	c.running[key] = make(chan struct{})
	return true
}

func (c *searchCache) end(key searchCacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.running[key]; ok {
		close(ch)
		delete(c.running, key)
	}
}

// waitRunning 等一个"同 key 或更短前缀"的在途搜索结束。
//
// 返回 true 后调用方应重新查一次缓存：大部分情况下对方已经把结果写进去了，
// 直接复用即可，不必自己再打一遍音源。返回 false 表示没有可等的东西。
//
// 等待期间每 100ms 探一次缓存：对方可能在还没结束的时候就先写入了前缀结果
// （它自己也是复用来的），这时立刻放行，不必等它彻底收尾。
func (c *searchCache) waitRunning(ctx context.Context, key searchCacheKey, timeout time.Duration) bool {
	c.mu.Lock()
	ch, ok := c.running[key]
	if !ok {
		best := -1
		for k, v := range c.running {
			if k.sig != key.sig || len(k.q) >= len(key.q) {
				continue
			}
			if !strings.HasPrefix(key.q, k.q) {
				continue
			}
			if len(k.q) > best {
				best, ch = len(k.q), v
			}
		}
		ok = ch != nil
	}
	c.mu.Unlock()
	if !ok {
		return false
	}

	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ch:
			return true
		case <-ctx.Done():
			return false
		case <-tick.C:
			if time.Now().After(deadline) {
				return false
			}
			if _, ok := c.get(key); ok {
				return true
			}
			if _, ok := c.prefix(key); ok {
				return true
			}
		}
	}
}

// put 写入缓存，超出容量时按写入顺序丢最旧的。
func (c *searchCache) put(key searchCacheKey, tracks []MeowTrack) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		c.order = append(c.order, key)
	}
	c.items[key] = searchCacheEntry{tracks: cloneTracks(tracks), at: time.Now()}
	for len(c.order) > searchCacheMax {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.items, old)
	}
}

// size 返回当前条目数（健康检查/排障用）。
func (c *searchCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
