package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 令牌桶 ----------

func TestRateLimiterDisabled(t *testing.T) {
	if NewRateLimiter(0, 10) != nil {
		t.Fatal("perMinute=0 应返回 nil（不限流）")
	}
	var nilLim *RateLimiter
	if ok, _, _ := nilLim.Allow("x"); !ok {
		t.Fatal("nil limiter 应永远放行")
	}
	nilLim.GC() // 不应 panic
	if nilLim.Limit() != 0 {
		t.Fatal("nil limiter 容量应为 0")
	}
}

func TestRateLimiterBurstThenRefill(t *testing.T) {
	// 60/min → 1 token/s，突发 2
	l := NewRateLimiter(60, 2)

	if ok, rem, _ := l.Allow("k"); !ok || rem != 1 {
		t.Fatalf("第 1 次应放行且剩 1，got ok=%v rem=%d", ok, rem)
	}
	if ok, rem, _ := l.Allow("k"); !ok || rem != 0 {
		t.Fatalf("第 2 次应放行且剩 0，got ok=%v rem=%d", ok, rem)
	}
	ok, _, retry := l.Allow("k")
	if ok {
		t.Fatal("第 3 次应被限流")
	}
	if retry <= 0 || retry > 2*time.Second {
		t.Fatalf("retryAfter 应接近 1s，got %v", retry)
	}

	// 等令牌补回来
	time.Sleep(1100 * time.Millisecond)
	if ok, _, _ := l.Allow("k"); !ok {
		t.Fatal("补足后应放行")
	}
}

func TestRateLimiterPerKeyIsolation(t *testing.T) {
	l := NewRateLimiter(60, 1)
	if ok, _, _ := l.Allow("a"); !ok {
		t.Fatal("a 首次应放行")
	}
	if ok, _, _ := l.Allow("a"); ok {
		t.Fatal("a 第二次应被限流")
	}
	if ok, _, _ := l.Allow("b"); !ok {
		t.Fatal("b 不应受 a 影响")
	}
}

func TestRateLimiterBurstDefaultsToRate(t *testing.T) {
	l := NewRateLimiter(120, 0)
	if l.Limit() != 120 {
		t.Fatalf("burst 应默认等于 rate，got %d", l.Limit())
	}
}

func TestRateLimiterGC(t *testing.T) {
	l := NewRateLimiter(60, 1)
	l.Allow("stale")
	l.mu.Lock()
	l.buckets["stale"].lastSeen = time.Now().Add(-time.Hour)
	l.mu.Unlock()

	l.GC()
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("过期桶应被回收，剩 %d", n)
	}
}

// ---------- 中间件 ----------

func newRateLimitServer(t *testing.T, perMin, burst, globalRPM int) *Server {
	t.Helper()
	stub := stubUpstream(t)
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel:    "p/m1",
		RateLimitPerMin: perMin, RateLimitBurst: burst, RateLimitGlobalRPM: globalRPM,
		UsageEnabled: false, ResponsesEnabled: false,
	}
	return NewServer(cfg, NewLogger("error"))
}

func doModels(srv *Server, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRateLimitMiddlewareBlocksAfterBurst(t *testing.T) {
	srv := newRateLimitServer(t, 60, 1, 0)

	if rec := doModels(srv, "sk-test"); rec.Code == http.StatusTooManyRequests {
		t.Fatalf("首次不该被限流: %d", rec.Code)
	}
	rec := doModels(srv, "sk-test")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第二次应 429，got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("缺少 Retry-After")
	}
	if rec.Header().Get("x-ratelimit-limit-requests") != "1" {
		t.Errorf("x-ratelimit-limit-requests = %q", rec.Header().Get("x-ratelimit-limit-requests"))
	}
	if rec.Header().Get("x-ratelimit-remaining-requests") != "0" {
		t.Errorf("x-ratelimit-remaining-requests = %q", rec.Header().Get("x-ratelimit-remaining-requests"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "rate_limit_exceeded") || !strings.Contains(body, "rate_limit_error") {
		t.Errorf("错误体应含 rate_limit 语义:\n%s", body)
	}
}

func TestRateLimitPerKeyIsolationOverHTTP(t *testing.T) {
	srv := newRateLimitServer(t, 60, 1, 0)
	if rec := doModels(srv, "sk-a"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("a 首次不该被限流")
	}
	if rec := doModels(srv, "sk-b"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("b 不该受 a 影响")
	}
	if rec := doModels(srv, "sk-a"); rec.Code != http.StatusTooManyRequests {
		t.Fatal("a 第二次应被限流")
	}
}

func TestRateLimitExemptsHealthAndMCP(t *testing.T) {
	srv := newRateLimitServer(t, 60, 1, 0)

	// /healthz 豁免
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("healthz 不该被限流（第 %d 次）", i)
		}
	}
	// MCP 豁免（上游调我们，限流会打断工具闭环）
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/mcp/token", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("MCP 不该被限流（第 %d 次）", i)
		}
	}
	// OPTIONS 预检豁免
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://x")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("OPTIONS 不该被限流")
	}
}

func TestRateLimitDisabledByDefault(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0)
	for i := 0; i < 10; i++ {
		if rec := doModels(srv, "sk-test"); rec.Code == http.StatusTooManyRequests {
			t.Fatal("未配置限流时不该 429")
		}
	}
}

func TestRateLimitGlobalCap(t *testing.T) {
	// 每 key 宽松，全局 1/min：不同 key 也会被全局桶挡住
	srv := newRateLimitServer(t, 600, 600, 1)
	if rec := doModels(srv, "sk-a"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("全局首次不该被限流")
	}
	if rec := doModels(srv, "sk-b"); rec.Code != http.StatusTooManyRequests {
		t.Fatal("全局桶应挡住第二个 key")
	}
}

// ---------- 分桶键 ----------

func TestRateKeyPrefersAPIKey(t *testing.T) {
	srv := newRateLimitServer(t, 60, 1, 0)

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer sk-abc")
	if got := srv.rateKey(r); got != "key:sk-abc" {
		t.Fatalf("got %q", got)
	}

	// 没有 key → 用 IP
	r2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r2.RemoteAddr = "10.0.0.2:9999"
	if got := srv.rateKey(r2); got != "ip:10.0.0.2" {
		t.Fatalf("got %q", got)
	}
}

func TestClientIPRespectsTrustProxy(t *testing.T) {
	srv := newRateLimitServer(t, 60, 1, 0)

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	// 默认不信任 XFF（否则客户端可以伪造来绕过限流）
	if got := srv.clientIP(r); got != "10.0.0.1" {
		t.Fatalf("默认应忽略 XFF，got %q", got)
	}
	srv.cfg.TrustProxy = true
	if got := srv.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("信任代理时应取 XFF 第一段，got %q", got)
	}
}
