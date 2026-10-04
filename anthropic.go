package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// anthropic.go —— Anthropic Messages API（/v1/messages）兼容层。
//
// 让 Claude Code / Anthropic SDK 能直接使用 Janus：与 Chat / Responses 共用同一套
// Store + executor + ToolBridge，只换输入输出形状（anthropic ↔ canonical）。
//
//	tool_use / tool_result  ↔  OpenAI tool_calls / role:"tool"
//	content blocks          ↔  content parts
//	stop_reason             ↔  finish_reason

const anthropicDefaultMaxTokens = 4096

// ---------- 请求 ----------

type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// image
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ---------- 响应 ----------

type AnthropicResponse struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"` // message
	Role         string              `json:"role"` // assistant
	Model        string              `json:"model"`
	Content      []AnthropicOutBlock `json:"content"`
	StopReason   string              `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence"`
	Usage        AnthropicUsage      `json:"usage"`
}

type AnthropicOutBlock struct {
	Type  string          `json:"type"` // text | tool_use
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---------- 输入映射 ----------

// anthropicSystemText 把 system 提取成纯文本（字符串或 []block）。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "" || blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// anthropicInputToMessages 把 Anthropic 的 messages 转成内部 ChatMessage。
// 返回需要回填给 MCP 的工具结果（tool_use_id -> output）。
func anthropicInputToMessages(systemText string, msgs []AnthropicMessage) ([]ChatMessage, map[string]ToolResult) {
	results := map[string]ToolResult{}
	var out []ChatMessage
	if strings.TrimSpace(systemText) != "" {
		out = append(out, ChatMessage{Role: "system", Content: MessageContent{Text: systemText}})
	}

	for _, m := range msgs {
		// content 可以是纯字符串
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			out = append(out, ChatMessage{
				Role:    orDefault(m.Role, "user"),
				Content: MessageContent{Text: s},
			})
			continue
		}
		var blocks []anthropicBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}

		// assistant 侧：text + tool_use 合成一条 assistant 消息
		if m.Role == "assistant" {
			var text strings.Builder
			var calls []ToolCall
			for _, blk := range blocks {
				switch blk.Type {
				case "", "text":
					text.WriteString(blk.Text)
				case "tool_use":
					args := string(blk.Input)
					if args == "" || args == "null" {
						args = "{}"
					}
					calls = append(calls, ToolCall{
						ID:       blk.ID,
						Type:     "function",
						Function: &FunctionCall{Name: blk.Name, Arguments: args},
					})
				}
			}
			out = append(out, ChatMessage{
				Role:      "assistant",
				Content:   MessageContent{Text: text.String()},
				ToolCalls: calls,
			})
			continue
		}

		// user 侧：text/image 合成 user 消息；tool_result 单独成 role:"tool"
		var text strings.Builder
		var parts []ContentPart
		for _, blk := range blocks {
			switch blk.Type {
			case "", "text":
				text.WriteString(blk.Text)
			case "image":
				if u := anthropicImageURI(blk); u != "" {
					parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: u}})
				}
			case "tool_result":
				content := anthropicToolResultText(blk.Content)
				out = append(out, ChatMessage{
					Role:       "tool",
					ToolCallID: blk.ToolUseID,
					Content:    MessageContent{Text: content},
				})
				results[blk.ToolUseID] = ToolResult{Content: content, IsError: blk.IsError}
			}
		}
		if text.Len() > 0 || len(parts) > 0 {
			out = append(out, ChatMessage{
				Role:    "user",
				Content: MessageContent{Text: text.String(), Parts: parts, IsArray: len(parts) > 0},
			})
		}
	}
	return out, results
}

// anthropicImageURI 把 image block 的 source 转成 data URI（base64）或 URL。
func anthropicImageURI(blk anthropicBlock) string {
	if blk.Source == nil {
		return ""
	}
	if blk.Source.URL != "" {
		return blk.Source.URL
	}
	if blk.Source.Data != "" {
		mt := blk.Source.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + blk.Source.Data
	}
	return ""
}

// anthropicToolResultText 提取 tool_result 的文本（字符串或 []block）。
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "" || blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func anthropicToolsToSpecs(tools []AnthropicTool) []ToolSpec {
	out := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, ToolSpec{
			Type:     "function",
			Function: ToolFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	return out
}

// ---------- 输出映射 ----------

func anthropicStopReason(t *turnOutcome) string {
	if len(t.tools) > 0 {
		return "tool_use"
	}
	switch t.outcome {
	case "length":
		return "max_tokens"
	case "timeout":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func buildAnthropicResponse(t *turnOutcome, model string) *AnthropicResponse {
	resp := &AnthropicResponse{
		ID:         "msg_" + newID()[:24],
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		StopReason: anthropicStopReason(t),
		Usage: AnthropicUsage{
			InputTokens:  t.res.usage.PromptTokens,
			OutputTokens: t.res.usage.CompletionTokens,
		},
	}
	if txt := t.res.text; txt != "" {
		resp.Content = append(resp.Content, AnthropicOutBlock{Type: "text", Text: txt})
	}
	for _, p := range t.tools {
		input := json.RawMessage(p.Args)
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage("{}")
		}
		resp.Content = append(resp.Content, AnthropicOutBlock{
			Type: "tool_use", ID: p.CallID, Name: p.ToolName, Input: input,
		})
	}
	return resp
}

// anthropicFromCached 把 Chat 缓存结果转成 Anthropic 响应（DiffNone 复用）。
func anthropicFromCached(cached *ChatResponse, model string) *AnthropicResponse {
	resp := &AnthropicResponse{
		ID: "msg_" + newID()[:24], Type: "message", Role: "assistant", Model: model,
		StopReason: "end_turn",
	}
	if cached == nil || len(cached.Choices) == 0 {
		return resp
	}
	msg := cached.Choices[0].Message
	if msg.Content != "" {
		resp.Content = append(resp.Content, AnthropicOutBlock{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		name := ""
		args := "{}"
		if tc.Function != nil {
			name, args = tc.Function.Name, tc.Function.Arguments
		}
		resp.Content = append(resp.Content, AnthropicOutBlock{
			Type: "tool_use", ID: tc.ID, Name: name, Input: json.RawMessage(args),
		})
		resp.StopReason = "tool_use"
	}
	return resp
}

// ---------- 错误 ----------

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

// anthropicErrorFromUpstream 把上游错误映射成 Anthropic 错误类型。
func (s *Server) anthropicErrorFromUpstream(err error) (int, string, string) {
	status, _, msg := s.mapUpstreamError(err)
	typ := "api_error"
	switch status {
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusBadRequest:
		typ = "invalid_request_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		typ = "authentication_error"
	}
	return status, typ, msg
}

// ---------- handler ----------

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	if !s.cfg.AnthropicEnabled {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error",
			"Anthropic Messages API disabled (BRIDGE_ANTHROPIC_ENABLED=false)")
		return
	}
	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	systemText := anthropicSystemText(req.System)
	inputMsgs, toolResults := anthropicInputToMessages(systemText, req.Messages)
	if len(inputMsgs) == 0 || (len(inputMsgs) == 1 && inputMsgs[0].Role == "system") {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages must be a non-empty array")
		return
	}
	tools := anthropicToolsToSpecs(req.Tools)

	ocModels, err := s.models.Get(r.Context(), s.up, s.cfg.Directory, false)
	if err != nil {
		st, typ, msg := s.anthropicErrorFromUpstream(err)
		writeAnthropicError(w, st, typ, msg)
		return
	}
	ref, err := ResolveModel(req.Model, ocModels)
	if err != nil {
		// Anthropic 的模型名（claude-*）通常不在 OpenCode 里：回退默认模型，别 404
		d, derr := s.resolveDefaultModel(r.Context(), ocModels)
		if derr != nil {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
			return
		}
		s.log.Debugf("anthropic model %q not found, falling back to %s", req.Model, d.String())
		ref = d
	}

	dir := firstNonEmpty(r.Header.Get("X-OpenCode-Directory"), s.cfg.Directory)
	agent := firstNonEmpty(r.Header.Get("X-OpenCode-Agent"), s.cfg.Agent)
	explicit := firstNonEmpty(r.Header.Get("X-Session-ID"), r.Header.Get("X-OpenCode-Session"))

	key := ConversationKey(explicit, systemText, "", dir)
	conv := s.store.Acquire(key, inputMsgs)
	defer s.store.Release(conv)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	if s.cfg.ToolCalling && len(tools) > 0 {
		if err := s.ensureTools(ctx, conv, dir, tools); err != nil {
			if !s.cfg.ToolSoftFail {
				writeAnthropicError(w, http.StatusBadGateway, "api_error",
					fmt.Sprintf("cannot expose your tools to the upstream agent: %v (directory=%s)", err, dir))
				return
			}
			s.log.Warnf("tool bridge registration failed (soft-fail): %v", err)
		}
	}

	// 客户端回填了工具结果 → 唤醒挂起的 agent，不重发 prompt
	if s.cfg.ToolCalling && conv.mcpName != "" && conv.snapshotSessionID() != "" &&
		hasToolResults(inputMsgs) && len(conv.pendingToolCalls()) > 0 {
		pending := conv.pendingToolCalls()
		conv.setPendingToolCalls(nil)
		conv.setLast(cloneMessages(inputMsgs))
		sid := conv.snapshotSessionID()
		sub := s.bus.Subscribe(sid, 512)
		defer sub.cancel()
		s.resumeAnthropic(ctx, w, r, req, ref, conv, pending,
			toolResults, dir, sub, time.Now().UnixMilli())
		return
	}

	stored := conv.snapshotLast()
	mode, delta := Diff(stored, inputMsgs)
	if mode == DiffNone {
		if cached := conv.snapshotResponse(); responseHasContent(cached) {
			resp := anthropicFromCached(cached, ref.String())
			setSessionHeaders(w, conv)
			if req.Stream {
				s.streamAnthropicCached(w, resp)
			} else {
				writeJSON(w, http.StatusOK, resp)
			}
			return
		}
		if retry := lastUserTurn(inputMsgs); len(retry) > 0 {
			delta = retry
			mode = DiffAppend
		}
	}
	if mode == DiffReset && conv.snapshotSessionID() != "" {
		s.resetSession(conv)
	}

	if _, err := s.ensureSession(ctx, conv, ref, agent, dir, inputMsgs); err != nil {
		st, typ, msg := s.anthropicErrorFromUpstream(err)
		writeAnthropicError(w, st, typ, msg)
		return
	}

	planText := FlattenDelta(delta)
	files, failed := ExtractAttachments(ctx, delta, s.httpc)
	if m := FindModel(ocModels, ref); len(files) > 0 {
		var dropped []OCFileAttach
		files, dropped = filterAttachmentsByModel(files, m)
		for _, f := range dropped {
			failed = append(failed, f.Name)
		}
	}
	if len(failed) > 0 {
		planText += attachFailureNote(failed)
	}

	sid := conv.snapshotSessionID()
	setSessionHeaders(w, conv)
	sub := s.bus.Subscribe(sid, 512)
	defer sub.cancel()

	promptAt := time.Now().UnixMilli()
	resp, err := s.up.Prompt(ctx, sid, OCPromptReq{Text: planText, Files: files})
	if err != nil && len(files) > 0 {
		s.log.Warnf("anthropic prompt with %d attachment(s) failed, retrying plain: %v", len(files), err)
		resp, err = s.up.Prompt(ctx, sid, OCPromptReq{Text: planText})
	}
	if err != nil {
		s.resetSession(conv)
		st, typ, msg := s.anthropicErrorFromUpstream(err)
		writeAnthropicError(w, st, typ, msg)
		return
	}
	if resp != nil && resp.Data.Time.Created > 0 {
		promptAt = resp.Data.Time.Created
	}
	conv.setLast(cloneMessages(inputMsgs))
	s.persistConv(conv)

	s.finishAnthropic(ctx, w, r, req, ref, conv, sub, promptAt)
}

// resumeAnthropic 回填挂起的工具结果，然后按 Anthropic 形状收尾。
func (s *Server) resumeAnthropic(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req AnthropicRequest, ref OCModelRef, conv *Conversation,
	pending []*pendingCall, results map[string]ToolResult, dir string,
	sub *subscription, promptAt int64) {

	answered := 0
	for _, p := range pending {
		res, ok := results[p.CallID]
		if !ok {
			res = ToolResult{Content: "bridge: client did not supply a result for tool_use " + p.CallID, IsError: true}
		} else {
			answered++
		}
		p.complete(res)
	}
	s.log.Infof("anthropic tool results delivered: conv=%s answered=%d/%d", conv.Key, answered, len(pending))
	s.finishAnthropic(ctx, w, r, req, ref, conv, sub, promptAt)
}

// finishAnthropic 跑完一轮并按 Anthropic 形状输出。
func (s *Server) finishAnthropic(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req AnthropicRequest, ref OCModelRef, conv *Conversation, sub *subscription, promptAt int64) {

	model := modelName(ref, req.Model)
	if req.Stream {
		s.streamAnthropic(ctx, w, r, req, ref, conv, sub, promptAt, model)
		return
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	t, err := s.runBlocking(ctx, conv, model, sub, promptAt, maxTokens, true)
	if t != nil {
		s.metrics.addTokens(t.res.usage.PromptTokens, t.res.usage.CompletionTokens)
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream generation timed out")
		} else {
			writeAnthropicError(w, 499, "api_error", "client disconnected")
		}
		return
	}

	if len(t.tools) > 0 {
		conv.setPendingToolCalls(t.tools)
		s.metrics.incToolCalls(len(t.tools))
	}
	respObj := buildAnthropicResponse(t, model)
	if len(respObj.Content) == 0 {
		conv.setResponse(nil)
		st, typ, _, msg := t.emptyCompletionError()
		writeAnthropicError(w, st, typ, msg)
		return
	}
	conv.setResponse(chatResponseFromAnthropic(respObj))
	writeJSON(w, http.StatusOK, respObj)
}

// chatResponseFromAnthropic 把 Anthropic 结果回写成 Chat 形状，供下一轮 DiffNone 复用。
func chatResponseFromAnthropic(a *AnthropicResponse) *ChatResponse {
	msg := AssistantMsg{Role: "assistant"}
	for _, c := range a.Content {
		switch c.Type {
		case "text":
			msg.Content += c.Text
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:       c.ID,
				Type:     "function",
				Function: &FunctionCall{Name: c.Name, Arguments: string(c.Input)},
			})
		}
	}
	finish := a.StopReason
	if finish == "tool_use" {
		finish = "tool_calls"
	}
	return &ChatResponse{
		ID: a.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: a.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: finish}},
	}
}

// ---------- count_tokens ----------

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 粗略估算：~4 字符 / token。Claude Code 用它做上下文控制，不要求精确。
	approx := len(body)/4 + 1
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": approx})
}
