package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"janus/internal/agent"
	"janus/internal/channel/claudecode"
	"janus/internal/channel/codex"
	"janus/internal/httpx"
)

// agent_cli.go —— CLI 型 harness（codex / claude-code）的接入与驱动。
//
// 与 opencode 型（常驻 server、事件走 EventBus）不同，CLI 型是「一 Prompt 一子
// 进程」：AgentRuntime 由 agent.CLIRuntime 通用实现，harness 特有的 argv / env /
// 事件解析在各自的 channel 包里（codex.RuntimeSpec / claudecode.RuntimeSpec）。
//
// 这里做两件事：
//  1. 把两个 CLI runtime 注册进 runtime 注册表（GET /api/v1/runtimes 可见）。
//  2. 提供 POST /api/v1/runtimes/{name}/prompt 直接驱动一次，事件按 SSE 回放。
//     这条链路让「起子进程 + 解析事件流」真正可达 —— IDE 请求路径（/v1/*）
//     的接入是后续一步（见 docs/REFACTOR.md）。

// registerCLIRuntimes 把 CLI 型 harness 注册进 runtime 注册表。
func (s *Server) registerCLIRuntimes(reg *agent.Registry) {
	reg.Register(agent.NewCLIRuntime(codex.RuntimeSpec(s.cfg.CodexBin)))
	reg.Register(agent.NewCLIRuntime(claudecode.RuntimeSpec(s.cfg.ClaudeBin)))
}

// runtimePromptRequest 是 POST /api/v1/runtimes/{name}/prompt 的入参。
type runtimePromptRequest struct {
	Prompt     string `json:"prompt"`      // 必填
	SessionKey string `json:"session_key"` // 复用同一 key = 续接同一 harness 会话
	Directory  string `json:"directory"`
	Model      string `json:"model"`
	APIKey     string `json:"api_key"`  // 注入给 CLI 当 janus 的 key（空=用 CLI 自带认证）
	MCPURL     string `json:"mcp_url"`  // 注册给 CLI 的 MCP 端点（空=不注册工具）
	MCPName    string `json:"mcp_name"` // 缺省 janus
	Stream     *bool  `json:"stream"`   // 缺省 true → SSE
	TimeoutMS  int    `json:"timeout_ms"`
}

// handleRuntimePrompt 驱动一次 CLI 型 harness：起子进程、把事件流回放给调用方。
func (s *Server) handleRuntimePrompt(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	rt, ok := s.agentRegistry().Get(name)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "unknown_runtime", "unknown runtime "+name, nil)
		return
	}
	var in runtimePromptRequest
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "prompt is required", nil)
		return
	}

	timeout := s.cfg.RequestTimeout
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err := rt.EnsureReady(ctx); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", err.Error(), nil)
		return
	}

	sessionKey := strings.TrimSpace(in.SessionKey)
	sid, err := rt.CreateSession(ctx, agent.SessionSpec{
		Key:            sessionKey,
		Directory:      strings.TrimSpace(in.Directory),
		Model:          strings.TrimSpace(in.Model),
		GatewayBaseURL: s.publicBaseURL(r),
		GatewayAPIKey:  strings.TrimSpace(in.APIKey),
		MCPURL:         s.runtimeMCPURL(in.MCPURL, sessionKey),
		MCPName:        strings.TrimSpace(in.MCPName),
	})
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "runtime_error", err.Error(), nil)
		return
	}

	events, err := rt.Prompt(ctx, sid, agent.PromptReq{
		Model:    strings.TrimSpace(in.Model),
		Stream:   true,
		Messages: []agent.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "runtime_error", err.Error(), nil)
		return
	}

	stream := true
	if in.Stream != nil {
		stream = *in.Stream
	}
	if !stream {
		s.collectRuntimeEvents(w, name, sid, events)
		return
	}
	s.streamRuntimeEvents(w, name, sid, events)
}

// collectRuntimeEvents 收齐事件后一次性返回（stream=false）。
func (s *Server) collectRuntimeEvents(w http.ResponseWriter, name, sid string, events <-chan agent.Event) {
	collected := make([]map[string]any, 0, 16)
	finish := "stop"
	for e := range events {
		if e.Type == agent.EventDone {
			finish = e.Finish
			break
		}
		collected = append(collected, eventView(e))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"runtime":    name,
		"session_id": sid,
		"finish":     finish,
		"events":     collected,
	})
}

// streamRuntimeEvents 以 SSE 回放事件流。
func (s *Server) streamRuntimeEvents(w http.ResponseWriter, name, sid string, events <-chan agent.Event) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Janus-Runtime", name)
	w.Header().Set("X-Janus-Session", sid)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := io.WriteString(w, "data: "+string(b)+"\n\n"); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	for e := range events {
		if !send(eventView(e)) {
			// 客户端断开：排空事件流，让子进程驱动协程能退出
			go func() {
				for range events {
				}
			}()
			return
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// eventView 把 agent.Event 展成给调用方的 JSON 形态。
func eventView(e agent.Event) map[string]any {
	out := map[string]any{"type": string(e.Type)}
	if e.Text != "" {
		out["text"] = e.Text
	}
	if e.Error != "" {
		out["error"] = e.Error
	}
	if e.Finish != "" {
		out["finish"] = e.Finish
	}
	if e.ToolCall != nil {
		tc := map[string]any{"id": e.ToolCall.ID, "name": e.ToolCall.Name}
		if len(e.ToolCall.Arguments) > 0 {
			tc["arguments"] = json.RawMessage(e.ToolCall.Arguments)
		}
		out["tool_call"] = tc
	}
	if e.Usage != nil {
		out["usage"] = map[string]any{
			"input_tokens":      e.Usage.InputTokens,
			"output_tokens":     e.Usage.OutputTokens,
			"cache_read_tokens": e.Usage.CacheReadTokens,
			"reasoning_tokens":  e.Usage.ReasoningTokens,
			"cost_usd":          e.Usage.CostUSD,
		}
	}
	return out
}
