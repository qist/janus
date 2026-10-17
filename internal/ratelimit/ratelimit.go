// Package ratelimit 实现进程内令牌桶限流。
//
// 目的：桥绑定 0.0.0.0 时，任何拿到 key 的人都能无限打 —— 既烧上游额度，
// 也会把上游限流（429/quota）引到自己身上。这是审计里唯一的安全硬缺口。
//
// 设计取舍：
//   - 令牌桶而不是固定窗口：允许突发（客户端一次发几个请求是正常的），
//     同时限制长期平均速率。
//   - 分桶键：优先 API key；没有 key 时用客户端 IP。这样多客户端互不影响。
//   - 另有一个可选的全局桶，防止有人用一堆 key/IP 绕过。
//   - 只做"请求准入"限流，不碰流式响应中途 —— SSE 一旦建立就不该被掐。
//   - /healthz 与 /mcp/* 豁免：前者是探活，后者是上游调我们（工具闭环的一部分）。
package ratelimit

import (
	"math"
	"sync"
	"time"
)

type tokenBucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

// RateLimiter 是一组令牌桶。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket

	rate  float64 // 每秒补充的令牌数
	burst float64 // 桶容量（允许的瞬时突发）
	ttl   time.Duration
}

// New 按"每分钟请求数"构造。perMinute<=0 表示不限流，返回 nil。
func New(perMinute, burst int) *RateLimiter {
	if perMinute <= 0 {
		return nil
	}
	if burst <= 0 {
		// 默认突发 = 1 分钟的额度，至少 1
		burst = perMinute
	}
	return &RateLimiter{
		buckets: map[string]*tokenBucket{},
		rate:    float64(perMinute) / 60.0,
		burst:   float64(burst),
		ttl:     10 * time.Minute,
	}
}

// Allow 消耗一个令牌。返回是否放行、剩余令牌数、以及需要等多久。
func (l *RateLimiter) Allow(key string) (ok bool, remaining int, retryAfter time.Duration) {
	if l == nil {
		return true, 0, 0
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[key]
	if b == nil {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		// 按流逝时间补充
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
			b.last = now
		}
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, int(b.tokens), 0
	}
	// 需要等多久才能攒够 1 个令牌
	need := (1 - b.tokens) / l.rate
	return false, 0, time.Duration(need * float64(time.Second))
}

// Limit 返回桶容量（用于响应头）。
func (l *RateLimiter) Limit() int {
	if l == nil {
		return 0
	}
	return int(l.burst)
}

// GC 回收长期不活跃的桶，避免 key/IP 很多时 map 无界增长。
func (l *RateLimiter) GC() {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.ttl {
			delete(l.buckets, k)
		}
	}
}
