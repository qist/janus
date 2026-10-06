package main

import (
	"context"
	"testing"
	"time"
)

func TestShouldLogReject(t *testing.T) {
	s := &toolSession{}

	ok, n := s.shouldLogReject(time.Minute)
	if !ok || n != 0 {
		t.Fatalf("首次应打印: ok=%v n=%d", ok, n)
	}
	for i := 0; i < 3; i++ {
		if ok, _ := s.shouldLogReject(time.Minute); ok {
			t.Fatal("限流窗口内不应再打印")
		}
	}
	// 把上次打印时间推回 2 分钟前 → 应打印，并带上被抑制的 3 条
	s.lastRejectLog.Store(time.Now().Add(-2 * time.Minute).UnixMilli())
	ok, n = s.shouldLogReject(time.Minute)
	if !ok || n != 3 {
		t.Fatalf("应打印并报 3 条抑制: ok=%v n=%d", ok, n)
	}
}

func TestWaitResultTimeoutFlag(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), 30*time.Millisecond, 0, nil)
	p := &pendingCall{CallID: "c", ToolName: "Grep", result: make(chan ToolResult, 1)}
	r := b.waitResult(context.Background(), p)
	if !r.TimedOut {
		t.Fatalf("超时应带 TimedOut: %+v", r)
	}
}

func TestToolSessionID(t *testing.T) {
	s := &toolSession{}
	if s.getSessionID() != "" {
		t.Fatalf("初始应为空: %q", s.getSessionID())
	}
	s.setSessionID("ses_x")
	if got := s.getSessionID(); got != "ses_x" {
		t.Fatalf("got %q", got)
	}
}
