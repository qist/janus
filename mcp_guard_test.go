package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// 目标会话没有在飞请求时，MCP tools/call 必须立刻被拒，而不是挂到 ToolCallWait。
func TestHandleMCPRejectsIdleConversation(t *testing.T) {
	s := NewServer(Config{APIKey: "x"}, NewLogger("error"))
	sess := s.tools.Register("f:idle", []ToolSpec{{Type: "function", Function: ToolFunction{Name: "read_file"}}})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
		sess.mcpToolName("read_file") + `","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/x", strings.NewReader(body))
	req.SetPathValue("token", sess.token)
	rec := httptest.NewRecorder()

	start := time.Now()
	s.handleMCP(rec, req)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("拒绝耗时 %s，应立刻返回", d)
	}
	if !strings.Contains(rec.Body.String(), "no active request") {
		t.Fatalf("期望被拒，实际: %s", rec.Body.String())
	}
}

func TestParseCIDRList(t *testing.T) {
	got := parseCIDRList(" 127.0.0.1/32 , 10.0.0.0/8 ,::1 , bad ,")
	want := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// 配了 BRIDGE_MCP_ALLOW 时，非白名单来源连 token 都不查，直接 403。
func TestMCPAllowListRejectsForeignSource(t *testing.T) {
	s := NewServer(Config{APIKey: "x", MCPAllow: "127.0.0.1/32, ::1"}, NewLogger("error"))
	sess := s.tools.Register("f:x", nil)

	// 白名单来源 → 走到 token 校验（未知 token 404）
	req := httptest.NewRequest(http.MethodPost, "/mcp/x", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.SetPathValue("token", "nope")
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("白名单来源应进入校验(404)，got %d", rec.Code)
	}

	// 非白名单来源 → 403
	req = httptest.NewRequest(http.MethodPost, "/mcp/x", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.RemoteAddr = "8.8.8.8:12345"
	req.SetPathValue("token", sess.token)
	rec = httptest.NewRecorder()
	s.handleMCP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非白名单来源应 403，got %d body=%s", rec.Code, rec.Body.String())
	}
}
