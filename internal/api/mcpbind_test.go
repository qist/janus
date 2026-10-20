package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if got, ok := b.ResolveBinding(binding); !ok || got != sess {
		t.Fatal("binding did not resolve to session")
	}
	// 空 binding 是 no-op，不应 panic 或污染表。
	b.Bind("", sess)
	if _, ok := b.ResolveBinding(""); ok {
		t.Fatal("empty binding should resolve to no session")
	}
	var nilBridge *ToolBridge
	nilBridge.Bind("x", sess) // 不应 panic
	if _, ok := nilBridge.ResolveBinding("x"); ok {
		t.Fatal("nil bridge should resolve to no session")
	}

	// 会话注销后绑定应一并摘除（回落到空会话路径）。
	b.Unregister("f:k")
	if _, ok := b.ResolveBinding(binding); ok {
		t.Fatal("binding should be dropped when session unregisters")
	}
}

// 同一 key 下有多个会话时，解析结果不能取决于绑定顺序；只有在恰好一个
// 会话有在飞请求时才允许命中。
func TestToolBridgeBindingIsDeterministic(t *testing.T) {
	binding := MCPBindingToken("key_multi")
	newBridge := func(order ...string) (*ToolBridge, map[string]*toolSession) {
		b := NewToolBridge(logx.NewLogger("error"), 0, 0, nil)
		sessions := map[string]*toolSession{}
		for _, key := range []string{"f:a", "f:b"} {
			sessions[key] = b.Register(key, nil)
		}
		for _, key := range order {
			b.Bind(binding, sessions[key])
		}
		return b, sessions
	}

	b1, s1 := newBridge("f:a", "f:b")
	if _, ok := b1.ResolveBinding(binding); ok {
		t.Fatal("ambiguous binding must not resolve")
	}
	b2, s2 := newBridge("f:b", "f:a")
	if _, ok := b2.ResolveBinding(binding); ok {
		t.Fatal("resolution must not depend on bind order")
	}

	// 唯一有 waiter 的会话 = 当前正在服务请求的会话，可以确定性命中。
	w := s1["f:b"].watch()
	if got, ok := b1.ResolveBinding(binding); !ok || got != s1["f:b"] {
		t.Fatalf("active session should win, got %+v ok=%v", got, ok)
	}
	w2 := s1["f:a"].watch()
	if _, ok := b1.ResolveBinding(binding); ok {
		t.Fatal("multiple active sessions must be ambiguous")
	}
	s1["f:b"].unwatch(w)
	s1["f:a"].unwatch(w2)
	if _, ok := b1.ResolveBinding(binding); ok {
		t.Fatal("multiple idle candidates must be ambiguous")
	}

	// 注销一个候选后，剩余唯一候选即可确定命中。
	b2.Unregister("f:a")
	if got, ok := b2.ResolveBinding(binding); !ok || got != s2["f:b"] {
		t.Fatalf("single remaining candidate should resolve, got %+v ok=%v", got, ok)
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

// 同一 key 下有多个并发会话时，稳定绑定不能靠「最近活跃」猜：
// tools/list 返回空（可恢复），带命名空间的 tools/call 仍按名字精确路由。
func TestHandleMCPBindingAmbiguousFailsClosed(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x", ToolOrphanWait: 10 * time.Millisecond},
		logx.NewLogger("error"))
	a := s.tools.Register("f:a", []ToolSpec{
		{Type: "function", Function: ToolFunction{Name: "read_file"}},
	})
	b := s.tools.Register("f:b", []ToolSpec{
		{Type: "function", Function: ToolFunction{Name: "write_file"}},
	})
	binding := MCPBindingToken("key_multi")
	s.tools.Bind(binding, a)
	s.tools.Bind(binding, b)

	listBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/"+binding, strings.NewReader(listBody))
	req.SetPathValue("token", binding)
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var listed struct {
		Result struct {
			Tools []any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("bad json: %v\n%s", err, rec.Body.String())
	}
	if len(listed.Result.Tools) != 0 {
		t.Fatalf("ambiguous binding must expose no tools, got %+v", listed.Result.Tools)
	}

	// tools/call 用带会话命名空间的工具名：不依赖绑定解析，直接路由到 f:a。
	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` +
		a.mcpToolName("read_file") + `","arguments":{}}}`
	req = httptest.NewRequest(http.MethodPost, "/mcp/"+binding, strings.NewReader(callBody))
	req.SetPathValue("token", binding)
	rec = httptest.NewRecorder()
	s.handleMCP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "no active client request") {
		t.Fatalf("call should route to f:a (idle guard), status=%d body=%s", rec.Code, rec.Body.String())
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

// janus 拉起 CLI harness 时，key 级稳定绑定地址要改写成会话级精确地址，
// 否则多个并发会话只能靠「最近活跃」猜，确定性无从谈起。
func TestRuntimeMCPURLPrefersExactSession(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x"}, logx.NewLogger("error"))
	sess := s.tools.Register("f:k", nil)

	bindingURL := "http://127.0.0.1:2810/mcp/" + MCPBindingToken("key_abc")
	if got, want := s.runtimeMCPURL(bindingURL, "f:k"), s.mcpEndpoint(sess.token); got != want {
		t.Fatalf("binding URL should be rewritten to exact session URL: got %q want %q", got, want)
	}
	// 找不到会话 key 时保留原地址（回退到 key 级绑定）。
	if got := s.runtimeMCPURL(bindingURL, "f:none"); got != bindingURL {
		t.Fatalf("unknown session key should keep binding URL, got %q", got)
	}
	// 精确会话地址 / 外部 MCP 地址 / 空地址都不改写。
	exact := s.mcpEndpoint(sess.token)
	for _, raw := range []string{exact, "https://mcp.example.com/sse", ""} {
		if got := s.runtimeMCPURL(raw, "f:k"); got != raw {
			t.Fatalf("runtimeMCPURL(%q) = %q, want unchanged", raw, got)
		}
	}
}

func TestMCPURLToken(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:2810/mcp/babc": "babc",
		"https://host/janus/mcp/babc/":   "babc",
		"https://mcp.example.com/sse":    "",
		"https://host/mcp":               "",
		"not a url":                      "",
		"http://127.0.0.1:2810/mcp/":     "",
	}
	for raw, want := range cases {
		if got := mcpURLToken(raw); got != want {
			t.Fatalf("mcpURLToken(%q) = %q, want %q", raw, got, want)
		}
	}
}
