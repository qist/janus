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
