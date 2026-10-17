package ratelimit

import (
	"testing"
	"time"
)

// ---------- 令牌桶 ----------

func TestRateLimiterDisabled(t *testing.T) {
	if New(0, 10) != nil {
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
	l := New(60, 2)

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
	l := New(60, 1)
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
	l := New(120, 0)
	if l.Limit() != 120 {
		t.Fatalf("burst 应默认等于 rate，got %d", l.Limit())
	}
}

func TestRateLimiterGC(t *testing.T) {
	l := New(60, 1)
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
