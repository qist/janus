package main

import (
	"testing"
	"time"
)

func TestMCPRegName(t *testing.T) {
	cases := []struct{ base, fp, want string }{
		{"ob-f-abc", "deadbeefdeadbeef", "ob-f-abc-deadbeef"},
		{"ob-f-abc", "12345678", "ob-f-abc-12345678"},
		{"ob-f-abc", "", "ob-f-abc"},
	}
	for _, c := range cases {
		if got := mcpRegName(c.base, c.fp); got != c.want {
			t.Errorf("mcpRegName(%q,%q) = %q, want %q", c.base, c.fp, got, c.want)
		}
	}
}

func TestToolBridgeAliasAndUnregister(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), 0)
	sess := b.Register("f:abc", []ToolSpec{{Type: "function", Function: ToolFunction{Name: "read_file"}}})
	base := sess.mcpName
	alias := mcpRegName(base, "deadbeef")

	b.Alias(sess, alias)
	if b.sessionByName(alias) != sess {
		t.Fatal("alias 未登记到 byName")
	}

	b.Unregister("f:abc")

	for _, n := range []string{base, alias} {
		if b.sessionByName(n) != nil {
			t.Errorf("Unregister 后 byName[%s] 仍存在", n)
		}
	}
	if len(b.RegisteredNames()) != 0 {
		t.Errorf("Unregister 后仍有注册名: %v", b.RegisteredNames())
	}
}

func TestWaitForWaiter(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), 0)
	sess := b.Register("f:x", nil)

	if sess.waitForWaiter(150 * time.Millisecond) {
		t.Fatal("无 watch 时不应返回 true")
	}

	ch := sess.watch()
	if !sess.waitForWaiter(150 * time.Millisecond) {
		t.Fatal("有 watch 时应返回 true")
	}
	sess.unwatch(ch)
	if sess.waitForWaiter(150 * time.Millisecond) {
		t.Fatal("unwatch 后不应返回 true")
	}
}

// parallel_tool_calls=false 时按到达顺序逐个取出；takePending 也保持到达顺序。
func TestToolSessionTakeOldestOrder(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), 0)

	sess := b.Register("f:ord", nil)
	c1 := sess.park("call_1", "a", "m_a", "{}")
	c2 := sess.park("call_2", "b", "m_b", "{}")
	c3 := sess.park("call_3", "c", "m_c", "{}")
	for i, want := range []*pendingCall{c1, c2, c3} {
		if got := sess.takeOldest(); got != want {
			t.Fatalf("第 %d 次 takeOldest = %v, want %s", i, got, want.CallID)
		}
	}
	if sess.takeOldest() != nil {
		t.Fatal("取完后应为 nil")
	}

	// takePending 顺序
	sess2 := b.Register("f:ord2", nil)
	sess2.park("call_x", "x", "m_x", "{}")
	sess2.park("call_y", "y", "m_y", "{}")
	all := sess2.takePending()
	if len(all) != 2 || all[0].CallID != "call_x" || all[1].CallID != "call_y" {
		t.Fatalf("takePending 顺序不对: %+v", all)
	}
}

func TestParallelDefault(t *testing.T) {
	if !parallelDefault(nil) {
		t.Error("未设置应默认并行(true)")
	}
	f := false
	if parallelDefault(&f) {
		t.Error("false 应返回 false")
	}
	tr := true
	if !parallelDefault(&tr) {
		t.Error("true 应返回 true")
	}
}
