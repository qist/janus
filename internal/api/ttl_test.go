package api

import (
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/logx"
)

// 共享（scope）会话用独立的 TTL；普通会话仍用 BRIDGE_SESSION_TTL。
func TestSharedSessionTTL(t *testing.T) {
	// 普通会话 ttl=0（立即过期），共享会话 1h（保留）
	s := NewStore(logx.NewLogger("error"), 0, time.Hour, 8)

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

// config.Never（<0）表示永不按空闲回收。
func TestTTLNeverKeepsSession(t *testing.T) {
	s := NewStore(logx.NewLogger("error"), config.Never, config.Never, 8)
	c := s.Acquire("s:abc", []ChatMessage{m("user", "x")})
	c.setSessionID("ses_keep")
	c.setLast([]ChatMessage{m("user", "x")})
	s.Release(c)

	if dead := s.GC(); len(dead) != 0 {
		t.Fatalf("config.Never 不应回收, got %v", dead)
	}
}
