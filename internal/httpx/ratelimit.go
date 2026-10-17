package httpx

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"janus/internal/logx"
	"janus/internal/metricx"
	"janus/internal/ratelimit"
)

// RateLimitOptions 是限流中间件的依赖。
//
// 中间件本身不认识业务协议：被限流时的错误体通过 WriteError 交给调用方
// （OpenAI 信封 / Anthropic 信封等）。
type RateLimitOptions struct {
	PerKey     *ratelimit.RateLimiter
	Global     *ratelimit.RateLimiter
	TrustProxy bool
	Log        *logx.Logger
	Metrics    *metricx.Metrics
	WriteError func(w http.ResponseWriter, status int, typ, msg, code string)
}

// RateKey 决定按什么分桶：优先 API key，其次客户端 IP。
func (o RateLimitOptions) RateKey(r *http.Request) string {
	if k := Bearer(r); k != "" {
		return "key:" + k
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return "key:" + k
	}
	return "ip:" + ClientIP(r, o.TrustProxy)
}

// RateExempt 判断是否跳过限流。
func RateExempt(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true // CORS 预检
	}
	p := r.URL.Path
	return p == "/healthz" || strings.HasPrefix(p, "/mcp/")
}

// RateLimit 按 API key / IP 与全局桶限流。
func RateLimit(o RateLimitOptions, next http.Handler) http.Handler {
	if o.PerKey == nil && o.Global == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RateExempt(r) {
			next.ServeHTTP(w, r)
			return
		}

		// 全局桶先扣；不放行就不必再扣分桶
		if o.Global != nil {
			if ok, _, retry := o.Global.Allow("global"); !ok {
				o.writeLimited(w, o.Global, retry, "global")
				return
			}
		}

		if o.PerKey != nil {
			key := o.RateKey(r)
			ok, remaining, retry := o.PerKey.Allow(key)
			if !ok {
				o.writeLimited(w, o.PerKey, retry, key)
				return
			}
			SetRateHeaders(w, o.PerKey, remaining, retry)
		}

		next.ServeHTTP(w, r)
	})
}

// writeLimited 返回协议自己的 429。
func (o RateLimitOptions) writeLimited(w http.ResponseWriter, lim *ratelimit.RateLimiter, retry time.Duration, key string) {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	SetRateHeaders(w, lim, 0, retry)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	if o.Metrics != nil {
		o.Metrics.IncRateLimited()
	}
	if o.Log != nil {
		o.Log.Warnf("rate limited: %s (retry after %ds)", key, secs)
	}
	if o.WriteError != nil {
		o.WriteError(w, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("Rate limit reached. Please retry after %ds.", secs),
			"rate_limit_exceeded")
		return
	}
	http.Error(w, "rate limit reached", http.StatusTooManyRequests)
}

// SetRateHeaders 按 OpenAI 惯例回带限流信息，方便客户端自适应退避。
func SetRateHeaders(w http.ResponseWriter, lim *ratelimit.RateLimiter, remaining int, retry time.Duration) {
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
