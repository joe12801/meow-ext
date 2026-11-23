package main

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func cacheKeyFor(q string) searchCacheKey {
	return searchCacheKey{q: normalizeQuery(q), sig: cacheSignature(nil, false, 30)}
}

func TestSearchCacheExactHit(t *testing.T) {
	c := newSearchCache()
	key := cacheKeyFor("晴天")
	if _, ok := c.get(key); ok {
		t.Fatal("空缓存不该命中")
	}
	c.put(key, []MeowTrack{{Title: "晴天", Artist: "周杰伦"}})
	got, ok := c.get(key)
	if !ok || len(got) != 1 {
		t.Fatalf("写入后应精确命中，got=%v ok=%v", got, ok)
	}
	// 大小写与多余空白必须归一，否则"晴天"/" 晴天 "会各存一份。
	if _, ok := c.get(cacheKeyFor("  晴天 ")); !ok {
		t.Fatal("空白差异应被视为同一个关键词")
	}
}

func TestSearchCacheSeparatesSourceSets(t *testing.T) {
	// 同一个词在不同音源集合下结果不同，混用会让用户勾了新音源却看不到它的结果。
	c := newSearchCache()
	kuwo := searchCacheKey{q: "晴天", sig: cacheSignature([]string{"kuwo"}, false, 30)}
	bili := searchCacheKey{q: "晴天", sig: cacheSignature([]string{"bilibili"}, false, 30)}
	c.put(kuwo, []MeowTrack{{Title: "来自酷我"}})
	if _, ok := c.get(bili); ok {
		t.Fatal("不同音源集合不该互相命中")
	}
}

func TestSearchCachePrefixOnlyMatchesShorterKey(t *testing.T) {
	c := newSearchCache()
	c.put(cacheKeyFor("someone like"), []MeowTrack{{Title: "占位结果"}})

	// 用户继续输入：可以用更短关键词的结果顶上。
	if got, ok := c.prefix(cacheKeyFor("someone like you")); !ok || len(got) != 1 {
		t.Fatal("更长的关键词应复用短前缀的缓存结果")
	}
	// 反方向必须拒绝：缓存里是 "someone like you" 时，搜 "someone"
	// 不能拿它的结果冒充——那是错，不是占位。
	c.put(cacheKeyFor("someone like you"), []MeowTrack{{Title: "具体结果"}})
	if _, ok := c.prefix(cacheKeyFor("someone")); ok {
		t.Fatal("短关键词不该复用更长关键词的结果")
	}
}

func TestSearchCacheIgnoresSingleCharPrefix(t *testing.T) {
	c := newSearchCache()
	c.put(cacheKeyFor("s"), []MeowTrack{{Title: "一个字母的噪声"}})
	if _, ok := c.prefix(cacheKeyFor("someone")); ok {
		t.Fatal("单字符前缀的结果本身就是噪声，不该拿来占位")
	}
}

func TestSearchCacheStaleAfterLongerInput(t *testing.T) {
	c := newSearchCache()
	c.note("someone like")
	if c.stale("someone like") {
		t.Fatal("同一个关键词不算过时")
	}
	c.note("someone like you")
	if !c.stale("someone like") {
		t.Fatal("已被更长的输入取代，应判为过时")
	}
	c.note("晴天")
	if c.stale("someone like") {
		t.Fatal("换成完全不同的关键词不算过时（结果仍可入缓存）")
	}
}

func TestSearchCacheWaitRunning(t *testing.T) {
	c := newSearchCache()
	short, long := cacheKeyFor("some"), cacheKeyFor("someone")
	if !c.begin(short) {
		t.Fatal("begin 应成功")
	}
	done := make(chan bool, 1)
	go func() { done <- c.waitRunning(context.Background(), long, time.Second) }()

	time.Sleep(20 * time.Millisecond)
	c.end(short)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("应等到在途的短词搜索结束")
		}
	case <-time.After(time.Second):
		t.Fatal("等待没有返回")
	}

	// 超时兜底：对方迟迟不结束也不能把调用方拖死。
	if !c.begin(short) {
		t.Fatal("重新 begin 应成功")
	}
	start := time.Now()
	if c.waitRunning(context.Background(), long, 30*time.Millisecond) {
		t.Fatal("超时后应返回 false")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("等待时间失控：%s", d)
	}
	c.end(short)

	// 不同音源集合的搜索互不相干。
	other := searchCacheKey{q: "someone", sig: cacheSignature([]string{"kuwo"}, false, 30)}
	if c.begin(cacheKeyFor("some")) {
		defer c.end(cacheKeyFor("some"))
	}
	if c.waitRunning(context.Background(), other, 30*time.Millisecond) {
		t.Fatal("音源集合不同时不该互相等待")
	}
}

func TestSearchCacheEvictsOldest(t *testing.T) {
	c := newSearchCache()
	for i := 0; i < searchCacheMax+10; i++ {
		c.put(cacheKeyFor("kw"+strconv.Itoa(i)), []MeowTrack{{Title: "t"}})
	}
	if got := c.size(); got > searchCacheMax {
		t.Fatalf("条目数应被限制在 %d 以内，实际 %d", searchCacheMax, got)
	}
}

func TestTrackVOHasLyricMatchesSource(t *testing.T) {
	// HasLyric 必须和"实际能不能出词"一致：视频/有声书/电台不检索歌词，
	// 声明 true 只会让客户端点进一个永远空白的歌词页。
	cases := []struct {
		source string
		want   bool
	}{
		{"kuwo", true},
		{"netease", true},
		{"kugou", true},
		{"qq", true},
		{"fivesing", true},
		{"ext", true},
		{"", true},
		{youtubeSourceKey, true},
		{"bilibili", false},
		{"ximalaya", false},
		{"lizhi", false},
	}
	for _, tc := range cases {
		vo := buildTrackVO("real", "0123456789abcdef0123456789abcdef",
			MeowTrack{Title: "测试", Source: tc.source}, "")
		if vo.HasLyric != tc.want {
			t.Fatalf("source=%q 的 hasLyric 应为 %v，实际 %v", tc.source, tc.want, vo.HasLyric)
		}
	}
}

func TestTrackVOCarriesAlbumName(t *testing.T) {
	// 列表页的"专辑"列以前恒为空，而播放页其实已经从补全层拿到了专辑名。
	vo := buildTrackVO("real", "fake", MeowTrack{Title: "晴天"}, "叶惠美")
	if vo.AlbumName != "叶惠美" || vo.Album.Name != "叶惠美" {
		t.Fatalf("专辑名未下发：albumName=%q album.name=%q", vo.AlbumName, vo.Album.Name)
	}
}

func TestMeowCooldownBlocksThenExpires(t *testing.T) {
	c := &meowCooldown{}
	if _, _, ok := c.active(); ok {
		t.Fatal("初始状态不该处于冷却")
	}
	if !c.block(40*time.Millisecond, 429, "额度用尽") {
		t.Fatal("首次进入冷却应返回 true（用于只打一次日志）")
	}
	if _, status, ok := c.active(); !ok || status != 429 {
		t.Fatalf("应处于冷却且带上游状态码，ok=%v status=%d", ok, status)
	}
	// 冷却期内再次撞 429 不算"新进入"，否则每次搜索都会刷一行日志。
	if c.block(time.Millisecond, 429, "again") {
		t.Fatal("延长冷却不该被算成新进入")
	}
	if c.remaining() <= 0 {
		t.Fatal("剩余冷却时间应大于 0")
	}
	time.Sleep(60 * time.Millisecond)
	if _, _, ok := c.active(); ok {
		t.Fatal("到期后应放行一次试探")
	}
	c.clear()
	if c.remaining() != 0 {
		t.Fatal("clear 之后不该还有剩余时间")
	}
}

func TestSearchRepeatedKeywordHitsCache(t *testing.T) {
	env := newIntegrationEnv(t)
	target := "/music/api/v1/search/track?keyword=晴天&page=1"

	env.do(t, http.MethodGet, target)
	first := len(env.meowCalls())
	if first == 0 {
		t.Fatal("首次搜索应该请求 Meow")
	}
	env.do(t, http.MethodGet, target)
	if got := len(env.meowCalls()); got != first {
		t.Fatalf("同一关键词重复搜索不该再打 Meow：首轮 %d 次，累计 %d 次", first, got)
	}
}

func TestSearchPrefixReuseIsImmediate(t *testing.T) {
	env := newIntegrationEnv(t)
	env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天&page=1")
	before := len(env.meowCalls())

	// 用户又敲了一个字：应当立即返回上一个短词的结果，而不是同步等外网。
	start := time.Now()
	rec := env.do(t, http.MethodGet, "/music/api/v1/search/track?keyword=晴天呀&page=1")
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("前缀复用应立即返回，实际耗时 %s", d)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if got := len(env.meowCalls()); got != before {
		t.Fatalf("前缀复用不应同步请求 Meow，新增 %d 次", got-before)
	}

	// 输入停下之后，延后的那次真实搜索应补上。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(env.meowCalls()) == before {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(env.meowCalls()); got != before+1 {
		t.Fatalf("应恰有一次延后刷新，实际新增 %d 次", got-before)
	}
}

func TestSearchCooldownStillLetsYouTubeWork(t *testing.T) {
	// 冷却的核心价值：Meow 额度用尽时不要连累其它源。
	s := newTestServer(t)
	s.meowCooldown.block(time.Minute, http.StatusTooManyRequests, "额度用尽")

	tracks, err := s.searchMeow(context.Background(), "晴天", 10, []string{"kuwo"})
	if err != nil {
		t.Fatalf("冷却期不该返回错误（那会让整次搜索判失败）：%v", err)
	}
	if len(tracks) != 0 {
		t.Fatalf("冷却期不该有结果，实际 %d 条", len(tracks))
	}
}
