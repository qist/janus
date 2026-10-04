package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	MaxUses     int             `json:"max_uses,omitempty"`
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

// web_search 是 Claude Code 的服务端工具，由桥内部执行。
const webSearchToolName = "web_search"

var webSearchSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"搜索关键词"}},"required":["query"]}`)

func anthropicToolsToSpecs(tools []AnthropicTool) []ToolSpec {
	out := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search") || t.Name == webSearchToolName {
			out = append(out, ToolSpec{Type: "function", Function: ToolFunction{
				Name: webSearchToolName, Description: "搜索网页，返回标题/链接/摘要", Parameters: webSearchSchema}})
			continue
		}
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

// anthropicHasWebSearch 判断请求里是否声明了 web_search 服务端工具。
func anthropicHasWebSearch(tools []AnthropicTool) bool {
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search") || t.Name == webSearchToolName {
			return true
		}
	}
	return false
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
	ref, err := s.resolveAnthropicModel(r.Context(), req.Model, ocModels)
	if err != nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}
	s.log.Debugf("anthropic request: client model=%q → resolved=%s (anthropic-version=%q)",
		req.Model, ref.String(), r.Header.Get("anthropic-version"))

	dir := firstNonEmpty(r.Header.Get("X-OpenCode-Directory"), s.cfg.Directory)
	agent := firstNonEmpty(r.Header.Get("X-OpenCode-Agent"), s.cfg.Agent)
	// Claude Code 会带 x-claude-code-session-id：直接当会话锚点，天然隔离不同 CC 会话
	explicit := firstNonEmpty(r.Header.Get("X-Session-ID"), r.Header.Get("X-OpenCode-Session"),
		r.Header.Get("x-claude-code-session-id"), r.Header.Get("X-Claude-Code-Session-Id"))

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

	// Claude Code 的 web_search 是"服务端工具"：声明它后由桥内部执行，
	// 不把它作为 tool_use 甩回给 CC。这里把它登记到会话上。
	if s.cfg.WebSearchEnabled && anthropicHasWebSearch(req.Tools) {
		if sess := s.tools.ByKey(conv.Key); sess != nil {
			sess.setServerTools(map[string]serverToolFunc{
				webSearchToolName: s.webSearchServerTool(dir),
			})
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

// ---------- 档位模型映射（Claude Code）----------

// anthropicTierAliases 是 Claude Code 常用的档位别名（供 /anthropic/v1/models 展示与默认映射）。
var anthropicTierAliases = []struct{ Alias, Tier string }{
	{"claude-opus-4-1", "opus"},
	{"claude-sonnet-4-5", "sonnet"},
	{"claude-3-5-haiku", "haiku"},
}

// anthropicTier 判断模型名属于哪个 Anthropic 档位（opus/sonnet/haiku），否则 ""。
func anthropicTier(raw string) string {
	l := strings.ToLower(raw)
	switch {
	case strings.Contains(l, "opus"):
		return "opus"
	case strings.Contains(l, "sonnet"):
		return "sonnet"
	case strings.Contains(l, "haiku"):
		return "haiku"
	}
	return ""
}

// pickTierModel 按档位自动挑一个可用模型：
//   - 排除经 API 会 403 的 opencode 免费额度
//   - 名字命中档位关键词（flash/mini、pro/max）的优先；
//     在此前提下 haiku 取最便宜、opus 取最贵、sonnet 取中间
func pickTierModel(list []OCModel, tier string) (OCModelRef, bool) {
	type cand struct {
		m    OCModel
		cost float64
		rank int
	}
	rankName := func(name string) int {
		l := strings.ToLower(name)
		switch tier {
		case "haiku":
			if strings.Contains(l, "flash") || strings.Contains(l, "mini") || strings.Contains(l, "lite") {
				return 0
			}
		case "opus":
			if strings.Contains(l, "pro") || strings.Contains(l, "luna") || strings.Contains(l, "max") || strings.Contains(l, "opus") {
				return 0
			}
		}
		return 1
	}
	var cs []cand
	for _, m := range list {
		if !m.Enabled || !m.Capabilities.Tools {
			continue
		}
		if m.ProviderID == "opencode" && isFreeModel(m) {
			continue // 免费额度经 API 会 403
		}
		c := 0.0
		if len(m.Cost) > 0 {
			c = m.Cost[0].Input + m.Cost[0].Output
		}
		cs = append(cs, cand{m: m, cost: c, rank: rankName(m.ID)})
	}
	if len(cs) == 0 {
		return OCModelRef{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].rank != cs[j].rank {
			return cs[i].rank < cs[j].rank
		}
		if cs[i].cost != cs[j].cost {
			return cs[i].cost < cs[j].cost
		}
		return cs[i].m.ID < cs[j].m.ID
	})
	// 只在最优 rank 的一组里，按档位挑（组内已按价格升序）
	bestRank := cs[0].rank
	group := cs[:0:0]
	for _, c := range cs {
		if c.rank == bestRank {
			group = append(group, c)
		}
	}
	var pick OCModel
	switch tier {
	case "haiku":
		pick = group[0].m
	case "opus":
		pick = group[len(group)-1].m
	default:
		pick = group[len(group)/2].m
	}
	return OCModelRef{ProviderID: pick.ProviderID, ID: pick.ID}, true
}

// resolveAnthropicModel 解析 Anthropic 请求的 model：
//  1. 显式 BRIDGE_MODEL_MAP（精确/前缀）
//  2. 直接可解析（客户端填了 opencode-go/xxx 这类）
//  3. claude-* 档位名 → BRIDGE_DEFAULT_MODEL（配了就用它，一个开关搞定）
//  4. 否则按档位自动挑；再不行才用上游默认
func (s *Server) resolveAnthropicModel(ctx context.Context, raw string, list []OCModel) (OCModelRef, error) {
	if ref, err := s.resolveModel(ctx, raw, list); err == nil {
		return ref, nil
	}
	if tier := anthropicTier(raw); tier != "" {
		if s.cfg.DefaultModel != "" {
			if ref, err := ResolveModel(s.cfg.DefaultModel, list); err == nil {
				return ref, nil
			}
		}
		if ref, ok := pickTierModel(list, tier); ok {
			return ref, nil
		}
	}
	if ref, err := s.resolveDefaultModel(ctx, list); err == nil {
		return ref, nil
	}
	return OCModelRef{}, fmt.Errorf("model %q not found and no usable default", raw)
}

// webSearchServerTool 返回一个"桥自己执行"的 web_search（调用 OpenCode 的 /api/websearch）。
func (s *Server) webSearchServerTool(dir string) serverToolFunc {
	return func(ctx context.Context, args string) (string, bool) {
		var a struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal([]byte(args), &a)
		q := strings.TrimSpace(a.Query)
		if q == "" {
			return "web_search: missing query", true
		}
		results, provider, err := s.up.WebSearch(ctx, dir, q)
		if err != nil {
			return "web_search failed: " + err.Error(), true
		}
		if len(results) == 0 {
			return "web_search: no results for " + q, false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "web_search(%q) provider=%s results=%d\n", q, provider, len(results))
		for i, r := range results {
			fmt.Fprintf(&b, "\n%d. %s\n   %s\n   %s\n", i+1,
				strings.TrimSpace(r.Title), r.URL, truncate(strings.TrimSpace(r.Content), 1200))
		}
		return b.String(), false
	}
}

// handleAnthropicModels 返回 Anthropic 格式的模型列表（Claude Code 的模型选择器用），
// id 用 claude 别名，display_name 里带上"实际会映射到哪个模型"，方便用户确认。
func (s *Server) handleAnthropicModels(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	list, _ := s.models.Get(r.Context(), s.up, s.cfg.Directory, false)

	type item struct{ id, display string }
	var items []item
	seen := map[string]bool{}
	add := func(alias string, ref OCModelRef, ok bool) {
		if alias == "" || seen[alias] {
			return
		}
		seen[alias] = true
		display := alias
		if ok {
			display = alias + " → " + ref.String()
		}
		items = append(items, item{alias, display})
	}
	// 显式映射里的 claude/anthropic 别名
	for k, v := range s.cfg.ModelMap {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "claude") || strings.HasPrefix(lk, "anthropic") {
			ref, err := ResolveModel(v, list)
			add(strings.TrimSuffix(k, "*"), ref, err == nil)
		}
	}
	// 内置档位别名（不管有没有显式映射都列，display 显示实际映射到谁）
	for _, t := range anthropicTierAliases {
		ref, err := s.resolveAnthropicModel(r.Context(), t.Alias, list)
		add(t.Alias, ref, err == nil)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })

	data := make([]map[string]any, 0, len(items))
	for _, it := range items {
		data = append(data, map[string]any{
			"type": "model", "id": it.id, "display_name": it.display,
			"created_at": "2025-01-01T00:00:00Z",
		})
	}
	first, last := "", ""
	if len(data) > 0 {
		first = data[0]["id"].(string)
		last = data[len(data)-1]["id"].(string)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": false, "first_id": first, "last_id": last,
	})
}
