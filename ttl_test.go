package main

import (
	"testing"
	"time"
)

// 共享（scope）会话用独立的 TTL；普通会话仍用 BRIDGE_SESSION_TTL。
func TestSharedSessionTTL(t *testing.T) {
	// 普通会话 ttl=0（立即过期），共享会话 1h（保留）
	s := NewStore(NewLogger("error"), 0, time.Hour, 8)

	shared := s.Acquire("s:abc", []ChatMessage{m("user", "x")})
	shared.setSessionID("ses_shared")
	shared.setLast([]ChatMessage{m("user", "x")})
	s.Release(shared)

	plain := s.Acquire("f:abc", []ChatMessage{m("user", "x")})
	plain.setSessionID("ses_plain")
	plain.setLast([]ChatMessage{m("user", "x")})
	s.Release(plain)

	dead := s.GC()
	if len(dead) != 1 || dead[0] != "ses_plain" {
		t.Fatalf("dead = %v, want [ses_plain]（共享会话应保留）", dead)
	}
}

// ttlNever（<0）表示永不按空闲回收。
func TestTTLNeverKeepsSession(t *testing.T) {
	s := NewStore(NewLogger("error"), ttlNever, ttlNever, 8)
	c := s.Acquire("s:abc", []ChatMessage{m("user", "x")})
	c.setSessionID("ses_keep")
	c.setLast([]ChatMessage{m("user", "x")})
	s.Release(c)

	if dead := s.GC(); len(dead) != 0 {
		t.Fatalf("ttlNever 不应回收, got %v", dead)
	}
}

// durTTL 支持 never/off/none/0 = ttlNever，普通值照常解析。
func TestDurTTL(t *testing.T) {
	l := &cfgLoader{file: map[string]string{
		"T_NEVER": "never",
		"T_OFF":   "off",
		"T_NONE":  "none",
		"T_ZERO":  "0",
		"T_DUR":   "30m",
		"T_SEC":   "120",
	}}
	for _, k := range []string{"T_NEVER", "T_OFF", "T_NONE", "T_ZERO"} {
		if got := l.durTTL(k, time.Hour); got != ttlNever {
			t.Fatalf("durTTL(%s) = %v, want ttlNever", k, got)
		}
	}
	if got := l.durTTL("T_DUR", time.Hour); got != 30*time.Minute {
		t.Fatalf("durTTL(T_DUR) = %v, want 30m", got)
	}
	if got := l.durTTL("T_SEC", time.Hour); got != 120*time.Second {
		t.Fatalf("durTTL(T_SEC) = %v, want 120s", got)
	}
	if got := l.durTTL("T_UNSET", time.Hour); got != time.Hour {
		t.Fatalf("未设置应返回默认, got %v", got)
	}
}
