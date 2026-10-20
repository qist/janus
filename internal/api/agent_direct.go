package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"janus/internal/agent"
	"janus/internal/gateway"
)

// agent_direct.go —— direct harness 的 AgentRuntime 适配器。
//
// direct 不建会话：一轮 Prompt 就是一次模型面透传（走 gateway 路由 + 凭据注入），
// 结果被解析成 agent.Event 流。它是「纯 LLM 客户端」以及未来非会话 harness 的落点。
//
// 注意：模型面透传本身是流式的，但这里为把一次请求归一成事件流，先把响应收进
// 内存再解析（对纯 LLM 客户端足够；真正的逐字流式仍走既有 /v1 handler）。

type directRuntime struct {
	s *Server
}

func (d *directRuntime) Name() string { return agent.NameDirect }

func (d *directRuntime) EnsureReady(context.Context) error {
	if d.s.router == nil || d.s.router.Store() == nil {
		return errors.New("model plane is not ready (no providers/credentials store)")
	}
	return nil
}

// CreateSession：direct 无状态，返回调用方给的逻辑键（或临时生成一个）。
func (d *directRuntime) CreateSession(_ context.Context, spec agent.SessionSpec) (string, error) {
	if strings.TrimSpace(spec.Key) != "" {
		return spec.Key, nil
	}
	return "direct-" + strconv.FormatInt(time.Now().UnixNano(), 36), nil
}

func (d *directRuntime) Prompt(ctx context.Context, sid string, req agent.PromptReq) (<-chan agent.Event, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("model is required")
	}
	p := principalFrom(ctx)
	body := buildOpenAIChatBody(req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Janus-Credential", principalHint(ctx))

	sink := &bufWriter{h: http.Header{}}
	if !d.s.proxyModel(sink, httpReq, p, gateway.ProtocolOpenAIChat, body, openAIFail(sink)) {
		return nil, errors.New("model plane is not available (routing disabled)")
	}

	events := parseOpenAIEvents(sink.h.Get("Content-Type"), sink.buf.Bytes(), sink.code)
	out := make(chan agent.Event, len(events)+1)
	for _, e := range events {
		out <- e
	}
	close(out)
	return out, nil
}

func (d *directRuntime) SetModel(context.Context, string, string) error {
	// direct 无会话状态：模型由每轮 PromptReq.Model 决定。
	return nil
}

// Interrupt 是 no-op：direct 的一轮 Prompt 是同步单次透传（模型面透传本身是
// 流式的，但这里为归一成事件流会先把响应收进内存），没有可打断的进行中会话。
// 客户端中断请直接取消 ctx（它会透传到上游请求）。
func (d *directRuntime) Interrupt(context.Context, string) error { return nil }

func (d *directRuntime) Close(context.Context) error { return nil }

// ---------- 请求体构造 ----------

func buildOpenAIChatBody(req agent.PromptReq) []byte {
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.System) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		if m.Role == "" {
			continue
		}
		mm := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			mm["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			mm["name"] = m.Name
		}
		msgs = append(msgs, mm)
	}
	out := map[string]any{"model": req.Model, "messages": msgs, "stream": req.Stream}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			var params any = map[string]any{"type": "object"}
			if len(t.Schema) > 0 {
				_ = json.Unmarshal(t.Schema, &params)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": params,
				},
			})
		}
		out["tools"] = tools
	}
	b, _ := json.Marshal(out)
	return b
}

// ---------- 响应解析 ----------

// openAIVerbose 兼容流式与非流式两套信封字段。
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		PromptDetails    *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// parseOpenAIEvents 把一次 OpenAI 响应（SSE 或整包 JSON）解析成事件流。
func parseOpenAIEvents(contentType string, body []byte, status int) []agent.Event {
	if status >= 400 || len(bytes.TrimSpace(body)) == 0 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = "upstream returned no content"
		}
		return []agent.Event{{Type: agent.EventError, Error: msg},
			{Type: agent.EventDone, Finish: "error"}}
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:")) {
		return parseOpenAIStream(body)
	}
	return parseOpenAIJSON(body)
}

func parseOpenAIStream(body []byte) []agent.Event {
	var out []agent.Event
	var finish string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ch openAIChunk
		if json.Unmarshal([]byte(payload), &ch) != nil {
			continue
		}
		out = append(out, chunkEvents(ch, true)...)
		if len(ch.Choices) > 0 && ch.Choices[0].FinishReason != "" {
			finish = ch.Choices[0].FinishReason
		}
		if ch.Usage != nil {
			out = append(out, agent.Event{Type: agent.EventUsage, Usage: chunkUsage(ch)})
		}
	}
	out = append(out, agent.Event{Type: agent.EventDone, Finish: orStop(finish)})
	return out
}

func parseOpenAIJSON(body []byte) []agent.Event {
	var ch openAIChunk
	if err := json.Unmarshal(body, &ch); err != nil {
		return []agent.Event{{Type: agent.EventError, Error: "cannot decode upstream response: " + err.Error()},
			{Type: agent.EventDone, Finish: "error"}}
	}
	out := chunkEvents(ch, false)
	finish := "stop"
	if len(ch.Choices) > 0 && ch.Choices[0].FinishReason != "" {
		finish = ch.Choices[0].FinishReason
	}
	if ch.Usage != nil {
		out = append(out, agent.Event{Type: agent.EventUsage, Usage: chunkUsage(ch)})
	}
	out = append(out, agent.Event{Type: agent.EventDone, Finish: finish})
	return out
}

// chunkEvents 抽出一条 chunk 里的正文 / 思考 / 工具调用；stream 决定取 delta 还是 message。
func chunkEvents(ch openAIChunk, stream bool) []agent.Event {
	var out []agent.Event
	for _, c := range ch.Choices {
		content, reasoning := c.Message.Content, c.Message.ReasoningContent
		type tc struct{ id, name, args string }
		var calls []tc
		if stream {
			content, reasoning = c.Delta.Content, c.Delta.ReasoningContent
			for _, t := range c.Delta.ToolCalls {
				calls = append(calls, tc{t.ID, t.Function.Name, t.Function.Arguments})
			}
		} else {
			for _, t := range c.Message.ToolCalls {
				calls = append(calls, tc{t.ID, t.Function.Name, t.Function.Arguments})
			}
		}
		if reasoning != "" {
			out = append(out, agent.Event{Type: agent.EventReasoning, Text: reasoning})
		}
		if content != "" {
			out = append(out, agent.Event{Type: agent.EventText, Text: content})
		}
		for _, t := range calls {
			var args json.RawMessage
			if strings.TrimSpace(t.args) != "" {
				args = json.RawMessage(t.args)
			}
			out = append(out, agent.Event{Type: agent.EventToolCall,
				ToolCall: &agent.ToolCall{ID: t.id, Name: t.name, Arguments: args}})
		}
	}
	return out
}

func chunkUsage(ch openAIChunk) *agent.Usage {
	u := &agent.Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
	if ch.Usage.PromptDetails != nil {
		u.CacheReadTokens = ch.Usage.PromptDetails.CachedTokens
	}
	if ch.Usage.CompletionDetails != nil {
		u.ReasoningTokens = ch.Usage.CompletionDetails.ReasoningTokens
	}
	return u
}

func orStop(finish string) string {
	if strings.TrimSpace(finish) == "" {
		return "stop"
	}
	return finish
}

// ---------- 无会话 runtime 的 ctx 传参 ----------

type principalCtxKey struct{}
type credentialHintKey struct{}

// withDirectContext 注入 direct runtime 需要的请求上下文（主体 + 凭据提示）。
func withDirectContext(ctx context.Context, p Principal, credentialHint string) context.Context {
	ctx = context.WithValue(ctx, principalCtxKey{}, p)
	if credentialHint != "" {
		ctx = context.WithValue(ctx, credentialHintKey{}, credentialHint)
	}
	return ctx
}

func principalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalCtxKey{}).(Principal); ok {
		return p
	}
	return Principal{}
}

func principalHint(ctx context.Context) string {
	if h, ok := ctx.Value(credentialHintKey{}).(string); ok {
		return h
	}
	return ""
}

// ---------- 内存 ResponseWriter（把一次透传收进内存） ----------

type bufWriter struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (b *bufWriter) Header() http.Header { return b.h }

func (b *bufWriter) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}

func (b *bufWriter) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.buf.Write(p)
}

// Flush 让 copyStream 的 Flusher 断言成立（内存 writer 无需真正 flush）。
func (b *bufWriter) Flush() {}
