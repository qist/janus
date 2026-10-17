package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/logx"
)

// 目标会话没有在飞请求时，MCP tools/call 必须在 BRIDGE_TOOL_ORPHAN_WAIT 后被拒，
// 而不是挂到 ToolCallWait（5 分钟）。
func TestHandleMCPRejectsIdleConversation(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x", ToolOrphanWait: 50 * time.Millisecond}, logx.NewLogger("error"))
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
	if !strings.Contains(rec.Body.String(), "no active client request") {
		t.Fatalf("期望被拒，实际: %s", rec.Body.String())
	}
}

// 配了 BRIDGE_MCP_ALLOW 时，非白名单来源连 token 都不查，直接 403。
func TestMCPAllowListRejectsForeignSource(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x", MCPAllow: "127.0.0.1/32, ::1"}, logx.NewLogger("error"))
	sess := s.tools.Register("f:x", nil)

	// 白名单来源 → 进入处理；未知 token 现在给"空会话"可恢复响应（200），不再 404
	req := httptest.NewRequest(http.MethodPost, "/mcp/x", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.SetPathValue("token", "nope")
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("白名单来源应进入处理(200)，got %d body=%s", rec.Code, rec.Body.String())
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

// 未知 token 不再 404：initialize/ping/tools-list 正常、tools-call 明确报错，
// 让上游平滑收敛而不是判成"服务挂了"。
func TestMCPUnknownSessionRecoverable(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x"}, logx.NewLogger("error"))
	cases := []struct{ method, params, want string }{
		{"initialize", `{"protocolVersion":"2025-06-18"}`, `"serverInfo"`},
		{"tools/list", `{}`, `"tools":[]`},
		{"tools/call", `{"name":"x","arguments":{}}`, `"isError":true`},
	}
	for _, c := range cases {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + c.method + `","params":` + c.params + `}`
		req := httptest.NewRequest(http.MethodPost, "/mcp/stale", strings.NewReader(body))
		req.SetPathValue("token", "stale")
		rec := httptest.NewRecorder()
		s.handleMCP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%s", c.method, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("%s: body=%s, want contains %s", c.method, rec.Body.String(), c.want)
		}
	}
}

// stale token（会话重置/换指纹后的残留配置）的 tools/call：工具名内嵌的会话
// 命名空间仍指向活会话 → 按名重路由、正常挂起，而不是报 "stale (session was reset)"。
// 实测：全量重放型客户端的历史带着旧纪元工具名，agent 依样调用，全部失败后
// 认定"工具通道断了"并放弃任务。
func TestMCPUnknownTokenReroutesByToolName(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x", ToolOrphanWait: time.Second}, logx.NewLogger("error"))
	sess := s.tools.Register("f:live", []ToolSpec{{Type: "function", Function: ToolFunction{Name: "read_file"}}})
	watch := sess.watch()
	defer sess.unwatch(watch) // 模拟在飞请求的执行器已注册 watch

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
		sess.mcpToolName("read_file") + `","arguments":{"path":"/tmp/a"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/deadtoken", strings.NewReader(body))
	req.SetPathValue("token", "deadtoken")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleMCP(rec, req)
	}()

	// 等待重路由挂起（守卫通过 → park → watch 通知），然后以客户端身份回填结果
	deadline := time.After(2 * time.Second)
	for {
		if pend := sess.takeOldest(); pend != nil {
			if pend.ToolName != "read_file" {
				t.Fatalf("重路由后工具名 = %q", pend.ToolName)
			}
			pend.complete(ToolResult{Content: "file content here"})
			break
		}
		select {
		case <-watch:
		case <-deadline:
			t.Fatal("stale-token 调用未被重路由挂起")
		}
	}
	<-done
	if strings.Contains(rec.Body.String(), "stale") {
		t.Fatalf("不应报 stale: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "file content here") {
		t.Fatalf("结果未回给 agent: %s", rec.Body.String())
	}
}

// 工具名不属于任何活会话时，unknown token 的 tools/call 仍报 stale（兜底不变）。
func TestMCPUnknownTokenStaleFallback(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x"}, logx.NewLogger("error"))
	s.tools.Register("f:live", []ToolSpec{{Type: "function", Function: ToolFunction{Name: "read_file"}}})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ob_otherworld_read_file","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/deadtoken", strings.NewReader(body))
	req.SetPathValue("token", "deadtoken")
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)
	if !strings.Contains(rec.Body.String(), "stale (session was reset)") {
		t.Fatalf("期望 stale 兜底: %s", rec.Body.String())
	}
}

// 孤儿守卫的拒绝消息按状态分流：有挂起调用时说明"已在等客户端回填"，
// 没有 waiter 时说明"没有在飞请求"；两种都明确劝停，避免模型反复重试
// （实测旧文案被当成重试暗示，agent 连续调了 5 轮工具、每轮白等 30s）。
func TestMCPIdleRejectMessageGuidesModelToStop(t *testing.T) {
	s := NewServer(config.Config{APIKey: "x", ToolOrphanWait: 50 * time.Millisecond}, logx.NewLogger("error"))
	sess := s.tools.Register("f:idle2", []ToolSpec{{Type: "function", Function: ToolFunction{Name: "read_file"}}})

	call := func() string {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
			sess.mcpToolName("read_file") + `","arguments":{}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp/"+sess.token, strings.NewReader(body))
		req.SetPathValue("token", sess.token)
		rec := httptest.NewRecorder()
		s.handleMCP(rec, req)
		return rec.Body.String()
	}
	if out := call(); !strings.Contains(out, "no active client request") {
		t.Fatalf("无 pending 时的消息不对: %s", out)
	}
	sess.park("call_x", "read_file", sess.mcpToolName("read_file"), "{}")
	if out := call(); !strings.Contains(out, "already delivered") {
		t.Fatalf("有 pending 时的消息不对: %s", out)
	}
}
