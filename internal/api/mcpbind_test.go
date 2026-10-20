package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"janus/internal/config"
	"janus/internal/logx"
)

func TestMCPBindingToken(t *testing.T) {
	if got := MCPBindingToken(""); got != "" {
		t.Fatalf("empty keyID should yield empty token, got %q", got)
	}
	if got := MCPBindingToken("   "); got != "" {
		t.Fatalf("blank keyID should yield empty token, got %q", got)
	}

	a := MCPBindingToken("key_abc")
	b := MCPBindingToken("key_abc")
	if a != b {
		t.Fatalf("token not deterministic: %q vs %q", a, b)
	}
	if !isMCPBindingToken(a) {
		t.Fatalf("token %q not recognized as binding token", a)
	}
	if c := MCPBindingToken("key_xyz"); c == a {
		t.Fatalf("different keys produced same token %q", a)
	}
	// 会话随机 token 是 32 位 hex，不应被判成绑定 token。
	if isMCPBindingToken(strings.Repeat("a", 32)) {
		t.Fatal("session token must not be treated as binding token")
	}
}

func TestToolBridgeBind(t *testing.T) {
	b := NewToolBridge(logx.NewLogger("error"), 0, 0, nil)
	sess := b.Register("f:k", nil)

	binding := MCPBindingToken("key_abc")
	b.Bind(binding, sess)
	if got := b.GetBinding(binding); got != sess {
		t.Fatal("binding did not resolve to session")
	}
	// 空 binding 是 no-op，不应 panic 或污染表。
	b.Bind("", sess)
	if got := b.GetBinding(""); got != nil {
		t.Fatal("empty binding should resolve to nil")
	}
	var nilBridge *ToolBridge
	nilBridge.Bind("x", sess) // 不应 panic
	if got := nilBridge.GetBinding("x"); got != nil {
		t.Fatal("nil bridge should resolve to nil")
	}

	// 会话注销后绑定应一并摘除（回落到空会话路径）。
	b.Unregister("f:k")
	if got := b.GetBinding(binding); got != nil {
		t.Fatal("binding should be dropped when session unregisters")
	}
}

// 用稳定 token 请求 tools/list，应拿到绑定会话声明的工具。
func TestHandleMCPResolvesBindingToken(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x"}, logx.NewLogger("error"))
	sess := s.tools.Register("f:k", []ToolSpec{
		{Type: "function", Function: ToolFunction{Name: "read_file", Description: "读文件"}},
	})
	binding := MCPBindingToken("key_abc")
	s.tools.Bind(binding, sess)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/"+binding, strings.NewReader(body))
	req.SetPathValue("token", binding)
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v\n%s", err, rec.Body.String())
	}
	if len(resp.Result.Tools) != 1 {
		t.Fatalf("want 1 tool, got %+v", resp.Result.Tools)
	}
	want := sess.mcpToolName("read_file")
	if resp.Result.Tools[0].Name != want {
		t.Fatalf("tool name = %q, want %q", resp.Result.Tools[0].Name, want)
	}
}

// 绑定 token 没有活跃会话时：可恢复的空会话响应（200），不是 404。
func TestHandleMCPBindingTokenWithoutSession(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x"}, logx.NewLogger("error"))
	binding := MCPBindingToken("key_unbound")

	req := httptest.NewRequest(http.MethodPost, "/mcp/"+binding,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.SetPathValue("token", binding)
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"tools"`) {
		t.Fatalf("want empty tools list, got %s", rec.Body.String())
	}
}
