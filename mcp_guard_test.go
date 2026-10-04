package main

import (
	"net/http"
	"net/http/httptest"
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
