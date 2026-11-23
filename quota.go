package main

import (
	"context"
	"sync"
	"time"
)

// meowQuotaCache 给 healthz 与管理页用。
//
// 额度是"给人看的静态信息"，没必要每次健康检查都往 Meow 打一次；
// 但它又必须足够新——用户刚听完一首就刷新页面时，界面上的剩余数该跟着动。
// 折中：30 秒软过期，过期后同步刷新一次。
//
// 刷新失败时保留上一次的成功结果、只把错误带出去：Meow 短暂打嗝不该让
// 页面上已经算好的"剩余 3 首"变成"未知"，那反而会让人以为额度重置了。
type meowQuotaCache struct {
	mu  sync.Mutex
	at  time.Time
	q   MeowQuota
	has bool
}

const meowQuotaTTL = 30 * time.Second

func (c *meowQuotaCache) get(ctx context.Context, cli *MeowClient) (MeowQuota, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.has && time.Since(c.at) < meowQuotaTTL {
		return c.q, true, nil
	}
	q, err := cli.Quota(ctx)
	if err != nil {
		// 失败：把过期时间往后挪一点，避免 healthz 被连续调用时
		// 每个请求都去撞一次已经坏掉的上游。
		c.at = time.Now()
		return c.q, c.has, err
	}
	c.q = q
	c.has = true
	c.at = time.Now()
	return q, true, nil
}
