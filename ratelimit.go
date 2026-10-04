package main

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ratelimit.go —— 进程内令牌桶限流。
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

// NewRateLimiter 按"每分钟请求数"构造。perMinute<=0 表示不限流，返回 nil。
func NewRateLimiter(perMinute, burst int) *RateLimiter {
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

// ---------- 中间件 ----------

// rateKey 决定按什么分桶：优先 API key，其次客户端 IP。
func (s *Server) rateKey(r *http.Request) string {
	if k := bearer(r); k != "" {
		return "key:" + k
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return "key:" + k
	}
	return "ip:" + s.clientIP(r)
}

// clientIP 取客户端地址。默认只看 RemoteAddr；BRIDGE_TRUST_PROXY=true 时才信任
// X-Forwarded-For（否则客户端可以随便伪造来绕过限流）。
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if rip := r.Header.Get("X-Real-IP"); rip != "" {
			return strings.TrimSpace(rip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateExempt 判断是否跳过限流。
func rateExempt(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true // CORS 预检
	}
	p := r.URL.Path
	return p == "/healthz" || strings.HasPrefix(p, "/mcp/")
}

func (s *Server) withRateLimit(next http.Handler) http.Handler {
	if s.perKeyLimit == nil && s.globalLimit == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rateExempt(r) {
			next.ServeHTTP(w, r)
			return
		}

		// 全局桶先扣；不放行就不必再扣分桶
		if s.globalLimit != nil {
			if ok, _, retry := s.globalLimit.Allow("global"); !ok {
				s.writeRateLimited(w, s.globalLimit, retry, "global")
				return
			}
		}

		if s.perKeyLimit != nil {
			key := s.rateKey(r)
			ok, remaining, retry := s.perKeyLimit.Allow(key)
			if !ok {
				s.writeRateLimited(w, s.perKeyLimit, retry, key)
				return
			}
			setRateHeaders(w, s.perKeyLimit, remaining, retry)
		}

		next.ServeHTTP(w, r)
	})
}

// writeRateLimited 返回 OpenAI 风格的 429。
func (s *Server) writeRateLimited(w http.ResponseWriter, lim *RateLimiter, retry time.Duration, key string) {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	setRateHeaders(w, lim, 0, retry)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.metrics.incRateLimited()
	s.log.Warnf("rate limited: %s (retry after %ds)", key, secs)
	writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error",
		fmt.Sprintf("Rate limit reached. Please retry after %ds.", secs),
		"rate_limit_exceeded")
}

// setRateHeaders 按 OpenAI 惯例回带限流信息，方便客户端自适应退避。
func setRateHeaders(w http.ResponseWriter, lim *RateLimiter, remaining int, retry time.Duration) {
	if lim == nil {
		return
	}
	h := w.Header()
	h.Set("x-ratelimit-limit-requests", strconv.Itoa(lim.Limit()))
	h.Set("x-ratelimit-remaining-requests", strconv.Itoa(remaining))
	if retry > 0 {
		h.Set("x-ratelimit-reset-requests", retry.Round(time.Millisecond).String())
	}
}
