package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// Meow 的限流冷却。
//
// 为什么需要：Meow 的游客额度是"每日 20 首"，用完之后每一次搜索、每一次
// 取词都会撞 429。日志里 85 次 429 全部发生在额度早已耗尽之后——用户每敲一个
// 字、每点一首歌，都在为同一个已知结论多付一次往返，而拿回来的还是一句
// "登录后不限"。冷却期内直接跳过 Meow：YouTube 等其它源照常工作，
// 用户至少还能搜到东西，而不是看着一个没有任何结果的转圈。
//
// 冷却到期后放行一次试探（不是把整段额度重新打开）：如果额度真恢复了，
// 一次成功就能让后续请求正常；如果仍是 429，就再冷一段时间。
type meowCooldown struct {
	mu     sync.Mutex
	until  time.Time
	status int
	reason string
}

const (
	// meowCooldownQuota 对 429。额度类问题通常要等用户去 Meow 里登录，
	// 5 分钟是一个"别太烦人、也别太频繁试探"的折中。
	meowCooldownQuota = 5 * time.Minute
	// meowCooldownAuth 对 401/403。token 失效或需要会员，短期重试没意义。
	meowCooldownAuth = 10 * time.Minute
)

// active 返回当前是否处于冷却，以及到期时间与上游状态码。
func (c *meowCooldown) active() (time.Time, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until.IsZero() || !time.Now().Before(c.until) {
		return time.Time{}, 0, false
	}
	return c.until, c.status, true
}

// block 进入（或延长）冷却，返回是否是新进入的冷却。
// 只有新进入时调用方才会打日志，否则每次搜索都会刷一行。
func (c *meowCooldown) block(d time.Duration, status int, reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasActive := !c.until.IsZero() && time.Now().Before(c.until)
	if next := time.Now().Add(d); next.After(c.until) {
		c.until = next
	}
	c.status, c.reason = status, reason
	return !wasActive
}

// clear 立即解除冷却（管理页改配置、用户刚登录完时用得上）。
func (c *meowCooldown) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until = time.Time{}
	c.status, c.reason = 0, ""
}

// remaining 返回剩余冷却时长，用于健康检查展示。
func (c *meowCooldown) remaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until.IsZero() {
		return 0
	}
	if d := time.Until(c.until); d > 0 {
		return d
	}
	return 0
}

// searchMeow 是搜索走 Meow 的唯一入口：套冷却，并在 429/403 时记下状态。
func (s *Server) searchMeow(ctx context.Context, keyword string, limit int, sources []string) ([]MeowTrack, error) {
	if _, _, cooling := s.meowCooldown.active(); cooling {
		// 返回空结果而不是错误：让其它音源（YouTube）继续出力，
		// 整次搜索不至于因为 Meow 冷却而变成"搜不到任何东西"。
		return nil, nil
	}
	tracks, err := s.meowClient().Search(ctx, keyword, limit, sources)
	if err != nil {
		s.noteMeowError(err)
	}
	return tracks, err
}

// noteMeowError 识别限流/鉴权类错误并进入冷却。
func (s *Server) noteMeowError(err error) {
	var me *MeowError
	if !errors.As(err, &me) {
		return
	}
	var d time.Duration
	switch me.Status {
	case http.StatusTooManyRequests:
		d = meowCooldownQuota
	case http.StatusUnauthorized, http.StatusForbidden:
		d = meowCooldownAuth
	default:
		return
	}
	if s.meowCooldown.block(d, me.Status, me.Msg) {
		log.Printf("[meow] 进入冷却 %s（HTTP %d：%s）；期间跳过 Meow，其它音源不受影响",
			d, me.Status, me.Msg)
	}
}
