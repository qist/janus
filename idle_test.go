package main

import (
	"testing"
	"time"
)

func TestExecutorIdleFor(t *testing.T) {
	e := newExecutor(nil, "sid", "m", false)
	if e.idleFor() > time.Second {
		t.Fatalf("刚创建应接近 0，实际 %v", e.idleFor())
	}
	// 模拟 3 分钟没有真实数据
	e.lastData.Store(time.Now().Add(-3 * time.Minute).UnixMilli())
	if got := e.idleFor(); got < 2*time.Minute {
		t.Fatalf("idleFor = %v, want >= 2m", got)
	}
	// touchData 重置
	e.touchData()
	if e.idleFor() > time.Second {
		t.Fatalf("touchData 后应重置，实际 %v", e.idleFor())
	}
}
