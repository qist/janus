package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// responses.go —— OpenAI Responses API（/v1/responses）。
//
// 与 Chat Completions 共用同一套会话/执行/工具内核（Store + executor + ToolBridge），
// 只是输入输出换成了 Responses 的形状：
//
//	instructions + input[]  →  Canonical messages
//	executor runResult      →  output[]（reasoning / message / function_call）
//
// 关键差异：
//   - 输入是 items 数组（message / function_call / function_call_output），不是 role 消息
//   - 工具定义是扁平的 {type:"function", name, description, parameters}
//   - previous_response_id 表示"接着上一次的会话"，此时 input 只含新增内容
//   - 流式用带类型的事件（event: response.output_text.delta）

const (
	responseIDPrefix = "resp_"
	outputMsgPrefix  = "msg_"
	reasoningPrefix  = "rs_"
	fcPrefix         = "fc_"

	defaultResponseTTL = 30 * time.Minute
)

// ---------- 请求 ----------

type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Reasoning          *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
	Store       *bool           `json:"store,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`

	// ParallelToolCalls=false 时：桥在同一轮里只回一个 function_call，
	// 等客户端回填结果后再回下一个（串行语义）。默认并行。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// ---------- 响应 ----------

type ResponsesResponse struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"`
	CreatedAt          int64              `json:"created_at"`
	Status             string             `json:"status"`
	Model              string             `json:"model"`
	Output             []ResponseItem     `json:"output"`
	OutputText         string             `json:"output_text,omitempty"`
	Usage              *ResponsesUsage    `json:"usage,omitempty"`
	PreviousResponseID *string            `json:"previous_response_id"`
	Instructions       *string            `json:"instructions"`
	Metadata           json.RawMessage    `json:"metadata,omitempty"`
	ParallelToolCalls  bool               `json:"parallel_tool_calls"`
	ToolChoice         string             `json:"tool_choice,omitempty"`
	Tools              []ResponseTool     `json:"tools"`
	Error              *openAIError       `json:"error"`
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details,omitempty"`
}

// IncompleteDetails 说明为什么 status=incomplete。
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"` // max_output_tokens
}

// ResponseItem 是 output[] 里的一项。
type ResponseItem struct {
	Type string `json:"type"` // message | reasoning | function_call
	ID   string `json:"id,omitempty"`

	// message
	Role    string            `json:"role,omitempty"`
	Status  string            `json:"status,omitempty"`
	Content []ResponseContent `json:"content,omitempty"`

	// reasoning
	Summary []ResponseSummary `json:"summary,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ResponseContent struct {
	Type        string            `json:"type"` // output_text
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
}

type ResponseSummary struct {
	Type string `json:"type"` // summary_text
	Text string `json:"text"`
}

type ResponseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int                     `json:"input_tokens"`
	OutputTokens        int                     `json:"output_tokens"`
	TotalTokens         int                     `json:"total_tokens"`
	InputTokensDetails  *ResponsesInputDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputDetails `json:"output_tokens_details,omitempty"`
}

type ResponsesInputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---------- 响应存储（previous_response_id 用）----------

type storedResponse struct {
	resp    *ResponsesResponse
	convKey string
	dir     string // 该响应所在链锁定的会话工作目录
	at      time.Time
}

type responseStore struct {
	mu    sync.Mutex
	items map[string]*storedResponse
	ttl   time.Duration
	db    *dbStore // 非空时写穿到 SQLite
}

func NewResponseStore(ttl time.Duration) *responseStore {
	if ttl <= 0 {
		ttl = defaultResponseTTL
	}
	return &responseStore{items: map[string]*storedResponse{}, ttl: ttl}
}

// attachDB 注入持久化层：此后 put/get/delete/gc 都会同步落库/读库，
// 使 previous_response_id 能跨进程重启续链。
func (r *responseStore) attachDB(db *dbStore) {
	r.mu.Lock()
	r.db = db
	r.mu.Unlock()
}

func (r *responseStore) put(resp *ResponsesResponse, convKey, dir string) {
	if resp == nil {
		return
	}
	at := time.Now()
	r.mu.Lock()
	r.items[resp.ID] = &storedResponse{resp: resp, convKey: convKey, dir: dir, at: at}
	db := r.db
	ttl := r.ttl
	r.mu.Unlock()
	if db != nil {
		if b, err := json.Marshal(resp); err == nil {
			db.putResponse(resp.ID, convKey, dir, at.Add(ttl).Unix(), b)
		}
	}
}

func (r *responseStore) get(id string) *storedResponse {
	r.mu.Lock()
	sr := r.items[id]
	db := r.db
	ttl := r.ttl
	r.mu.Unlock()
	if sr != nil {
		return sr
	}
	if db == nil {
		return nil
	}
	payload, convKey, dir, expiresAt, ok := db.getResponse(id)
	if !ok {
		return nil
	}
	if expiresAt > 0 && time.Now().Unix() > expiresAt {
		db.deleteResponse(id)
		return nil
	}
	var resp ResponsesResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil
	}
	sr = &storedResponse{resp: &resp, convKey: convKey, dir: dir, at: time.Unix(expiresAt, 0).Add(-ttl)}
	r.mu.Lock()
	r.items[id] = sr
	r.mu.Unlock()
	return sr
}

func (r *responseStore) delete(id string) bool {
	r.mu.Lock()
	_, ok := r.items[id]
	delete(r.items, id)
	db := r.db
	r.mu.Unlock()
	if db != nil && db.deleteResponse(id) {
		ok = true
	}
	return ok
}

func (r *responseStore) gc() {
	r.mu.Lock()
	now := time.Now()
	for k, v := range r.items {
		if now.Sub(v.at) > r.ttl {
			delete(r.items, k)
		}
	}
	db := r.db
	r.mu.Unlock()
	if db != nil {
		db.gcResponses(now.Unix())
	}
}

// ---------- 输入转换 ----------

// responsesToolsToSpecs 把扁平的工具定义转成内部 ToolSpec。
func responsesToolsToSpecs(raw json.RawMessage) []ToolSpec {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Function    *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]ToolSpec, 0, len(items))
	for _, it := range items {
		// OpenAI Responses 的服务端工具（web_search / web_search_preview）是扁平定义、
		// 没有 name 字段。这里把它暴露成名为 web_search 的工具，由桥内部执行
		// （与 Anthropic 的 web_search 服务端工具同一套，见 anthropic.go）。
		if strings.HasPrefix(it.Type, "web_search") {
			out = append(out, ToolSpec{
				Type: "function",
				Function: ToolFunction{
					Name:        webSearchToolName,
					Description: "搜索网页，返回标题/链接/摘要",
					Parameters:  webSearchSchema,
				},
			})
			continue
		}
		name, desc, params := it.Name, it.Description, it.Parameters
		// 兼容把 chat 风格（嵌 function）也塞进来的客户端
		if name == "" && it.Function != nil {
			name, desc, params = it.Function.Name, it.Function.Description, it.Function.Parameters
		}
		if name == "" {
			continue
		}
		out = append(out, ToolSpec{
			Type:     "function",
			Function: ToolFunction{Name: name, Description: desc, Parameters: params},
		})
	}
	return out
}

// responsesHasWebSearch 判断 Responses 请求是否声明了 web_search 服务端工具。
func responsesHasWebSearch(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var items []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return false
	}
	for _, it := range items {
		if strings.HasPrefix(it.Type, "web_search") {
			return true
		}
	}
	return false
}

// responsesInputToMessages 把 input 转成内部消息。
// 返回需要回填给 MCP 的工具结果（call_id -> output）。
func responsesInputToMessages(input json.RawMessage) ([]ChatMessage, map[string]ToolResult) {
	results := map[string]ToolResult{}
	if len(input) == 0 || string(input) == "null" {
		return nil, results
	}

	// input 可以是纯字符串
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, results
		}
		return []ChatMessage{{Role: "user", Content: MessageContent{Text: s}}}, results
	}

	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		// function_call
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		// function_call_output
		Output string `json:"output"`
	}
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, results
	}

	var msgs []ChatMessage
	for _, it := range items {
		switch it.Type {
		case "", "message":
			text, parts := responsesContentParts(it.Content)
			msgs = append(msgs, ChatMessage{
				Role:    orDefault(it.Role, "user"),
				Content: MessageContent{Text: text, Parts: parts, IsArray: len(parts) > 0},
			})
		case "function_call":
			msgs = append(msgs, ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:       it.CallID,
					Type:     "function",
					Function: &FunctionCall{Name: it.Name, Arguments: it.Arguments},
				}},
			})
		case "function_call_output":
			msgs = append(msgs, ChatMessage{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    MessageContent{Text: it.Output},
			})
			results[it.CallID] = ToolResult{Content: it.Output}
		}
	}
	return msgs, results
}

// responsesContentText 提取 message item 的文本。
func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "", "input_text", "output_text", "text":
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// responsesContentParts 解析 message 的 content：文本 + 图片（input_image / image_url）。
// 图片转成内部 ContentPart（与 Chat 的 content 数组同形），供 ExtractAttachments 处理。
func responsesContentParts(raw json.RawMessage) (string, []ContentPart) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil
	}
	var b strings.Builder
	var out []ContentPart
	for _, p := range parts {
		switch p.Type {
		case "", "input_text", "output_text", "text":
			b.WriteString(p.Text)
		case "input_image", "image_url":
			if u := imageURLString(p.ImageURL); u != "" {
				out = append(out, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: u}})
			}
		}
	}
	return b.String(), out
}

// imageURLString 兼容 image_url 是字符串或 {"url": "..."} 两种形态。
func imageURLString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.URL
	}
	return ""
}

// ---------- 输出转换 ----------

func usageToResponses(u Usage) *ResponsesUsage {
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return nil
	}
	out := &ResponsesUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.PromptTokens + u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil {
		out.InputTokensDetails = &ResponsesInputDetails{CachedTokens: u.PromptTokensDetails.CachedTokens}
	}
	if u.CompletionTokensDetails != nil {
		out.OutputTokensDetails = &ResponsesOutputDetails{ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens}
	}
	return out
}

// buildResponsesOutput 把一轮结果转成 output[]。
func buildResponsesOutput(t *turnOutcome) ([]ResponseItem, string) {
	var out []ResponseItem

	if strings.TrimSpace(t.res.reasoning) != "" {
		out = append(out, ResponseItem{
			Type:    "reasoning",
			ID:      reasoningPrefix + newID()[:24],
			Summary: []ResponseSummary{{Type: "summary_text", Text: t.res.reasoning}},
		})
	}

	if len(t.tools) > 0 {
		for _, p := range t.tools {
			out = append(out, ResponseItem{
				Type:      "function_call",
				ID:        fcPrefix + newID()[:24],
				CallID:    p.CallID,
				Name:      p.ToolName,
				Arguments: p.Args,
				Status:    "completed",
			})
		}
		return out, ""
	}

	text := t.res.text
	if text != "" {
		out = append(out, ResponseItem{
			Type:   "message",
			ID:     outputMsgPrefix + newID()[:24],
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContent{{
				Type:        "output_text",
				Text:        text,
				Annotations: []json.RawMessage{},
			}},
		})
	}
	return out, text
}

func responsesStatus(t *turnOutcome) string {
	if len(t.tools) > 0 {
		return "completed" // 工具调用也是 completed（output 里带 function_call）
	}
	switch t.outcome {
	case "failed":
		return "failed"
	case "timeout", "length":
		return "incomplete"
	default:
		return "completed"
	}
}

// applyIncomplete 补上 incomplete_details。
func applyIncomplete(resp *ResponsesResponse, t *turnOutcome) {
	if resp.Status != "incomplete" {
		return
	}
	reason := "max_output_tokens"
	if t.outcome == "timeout" {
		reason = "timeout"
	}
	resp.IncompleteDetails = &IncompleteDetails{Reason: reason}
}

// ---------- handlers ----------

func (s *Server) handleCreateResponse(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	if !s.cfg.ResponsesEnabled {
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
			"Responses API disabled (BRIDGE_RESPONSES_ENABLED=false)", "disabled")
		return
	}

	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error(), "")
		return
	}

	inputMsgs, toolResults := responsesInputToMessages(req.Input)
	if len(inputMsgs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"input must be a non-empty string or array", "invalid_input")
		return
	}
	tools := responsesToolsToSpecs(req.Tools)

	// ---- 模型 ----
	ocModels, err := s.models.Get(r.Context(), s.up, s.cfg.Directory, false)
	if err != nil {
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}
	ref, err := s.resolveModel(r.Context(), req.Model, ocModels)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error(), "model_not_found")
		return
	}
	if effort := responsesEffort(req); effort != "" && ref.Variant == "" {
		if m := FindModel(ocModels, ref); m != nil {
			if v, ok := PickVariant(*m, effort); ok {
				ref.Variant = v
			}
		}
	}

	// 会话目录：与 Chat / Anthropic 一致——客户端项目路径在 janus 主机上不存在时
	// 用 per-scope 中性工作目录（远程 + mode B，见 scope.go），而不是回落到部署目录。
	scopeK, _ := s.scopeOf(r, r.Header.Get("X-OpenCode-Directory"), inputMsgs)
	dir := s.sessionDir(r.Header.Get("X-OpenCode-Directory"), inputMsgs, scopeK)
	agent := firstNonEmpty(r.Header.Get("X-OpenCode-Agent"), s.cfg.Agent)
	explicit := firstNonEmpty(r.Header.Get("X-Session-ID"), r.Header.Get("X-OpenCode-Session"))

	// ---- 会话：previous_response_id > X-Session-ID > 独立链 ----
	//
	// X-Session-ID 与 Chat 共用命名空间（x:<id>），使同一客户端锚点下
	// Chat Completions 与 Responses 落在同一个 Janus 会话 / OpenCode session。
	convKey := ""
	var prev *storedResponse
	if req.PreviousResponseID != "" {
		if prev = s.responses.get(req.PreviousResponseID); prev == nil {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("previous_response_id %q not found (it may have expired)", req.PreviousResponseID),
				"previous_response_not_found")
			return
		}
		convKey = prev.convKey
	} else if explicit != "" {
		convKey = "x:" + explicit
	} else {
		convKey = "resp:" + responseIDPrefix + newID()
	}

	// 续链时锁定原链的工作目录：增量 input 里通常没有项目路径，重算会漂移。
	if prev != nil && prev.dir != "" {
		dir = prev.dir
	}

	conv := s.store.AcquireKey(convKey)
	s.logClientInfo(r, "", dir, convKey, inputMsgs)
	defer s.store.Release(conv)

	// 出向路径改写：远程 mode B 下把回答里的工作区路径改写回客户端项目路径。
	s.setPathRewrite(conv, dir, r.Header.Get("X-OpenCode-Directory"), inputMsgs)

	// 上一轮被中止过：重开干净会话。断线后客户端的「同内容重发」不消费标记、
	// 也不重跑被终止的旧内容（否则模型会接着答中断的半截话）；新内容才重置。
	if terminatedReplay(conv, inputMsgs) {
		s.log.Infof("terminated conversation replayed identical turn; keep flag, not re-running (key=%s)", conv.Key)
		writeOpenAIError(w, 499, "api_error", "generation was interrupted by the client; the previous turn will not be re-run automatically", "canceled")
		return
	}
	if conv.snapshotTerminated() {
		s.consumeTerminated(conv)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	// ---- 工具 ----
	if s.cfg.ToolCalling && len(tools) > 0 {
		if err := s.ensureTools(ctx, conv, dir, tools); err != nil {
			if s.cfg.ToolSoftFail {
				s.log.Warnf("tool bridge registration failed (soft-fail): %v", err)
			} else {
				writeOpenAIError(w, http.StatusBadGateway, "api_error",
					fmt.Sprintf("cannot expose your tools to the upstream agent: %v (directory=%s)", err, dir),
					"tool_registration_failed")
				return
			}
		}
	}

	// Responses 的 web_search 是服务端工具：声明后由桥内部执行，不甩回客户端。
	if s.cfg.WebSearchEnabled && responsesHasWebSearch(req.Tools) {
		if sess := s.tools.ByKey(conv.Key); sess != nil {
			sess.setServerTools(map[string]serverToolFunc{
				webSearchToolName: s.webSearchServerTool(dir),
			})
		}
	}

	// ---- 续链：先回填上一轮的工具结果，再放行 ----
	var sub *subscription
	if prev != nil && len(conv.pendingToolCalls()) > 0 && len(toolResults) > 0 {
		pending := livePending(conv.pendingToolCalls())
		conv.setPendingToolCalls(nil)
		if len(pending) == 0 {
			// 回填来得太晚：上轮工具调用已全部超时，agent 已被释放/中断。
			// 续跑只会空转后报错，按新的一轮继续。客户端可能一次回填整批积压
			// 旧结果（含早已消费过的），必须剥掉，否则被 Flatten 进新 prompt、
			// agent 把旧数据当新上下文用（数据回放）。
			s.log.Warnf("responses tool results arrived after all pending calls timed out (conv=%s); treating as new turn",
				conv.Key)
			inputMsgs = stripStaleToolResults(inputMsgs)
		} else {
			// 先确保会话存在
			if _, err := s.ensureSession(ctx, conv, ref, agent, dir, inputMsgs); err != nil {
				st, typ, msg := s.mapUpstreamError(err)
				writeOpenAIError(w, st, typ, msg, "")
				return
			}
			sub = s.bus.Subscribe(conv.snapshotSessionID(), 512)
			defer sub.cancel()
			promptAt := time.Now().UnixMilli()
			s.deliverToolResults(conv, pending, toolResults, inputMsgs)
			setSessionHeaders(w, conv)
			s.finishResponses(ctx, w, r, req, ref, conv, sub, promptAt, dir)
			return
		}
	}

	// ---- 新的一轮：把 input 作为 prompt 发出 ----
	newSession, err := s.ensureSession(ctx, conv, ref, agent, dir, inputMsgs)
	if err != nil {
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}
	planText := Flatten(inputMsgs)
	planText += s.firstTurnNote(conv, newSession)
	// 附件：data URI 直传 / http(s) 下载；模型不支持该模态就丢弃并说明。
	files, failed := ExtractAttachments(ctx, inputMsgs, s.httpc)
	if m := FindModel(ocModels, ref); len(files) > 0 {
		var dropped []OCFileAttach
		files, dropped = filterAttachmentsByModel(files, m)
		for _, f := range dropped {
			failed = append(failed, f.Name)
		}
	}
	if len(failed) > 0 {
		s.log.Warnf("responses attachments dropped: %d item(s)", len(failed))
		planText += attachFailureNote(failed)
	}
	sub = s.bus.Subscribe(conv.snapshotSessionID(), 512)
	defer sub.cancel()

	promptAt := time.Now().UnixMilli()
	resp, err := s.up.Prompt(ctx, conv.snapshotSessionID(), OCPromptReq{Text: planText, Files: files})
	if err != nil && len(files) > 0 {
		// 附件让上游失败时降级重发纯文本，保住对话本身（与 chat 路径一致）
		s.log.Warnf("responses prompt with %d attachment(s) failed, retrying as plain text: %v", len(files), err)
		resp, err = s.up.Prompt(ctx, conv.snapshotSessionID(), OCPromptReq{Text: planText})
	}
	if err != nil {
		if newSession {
			s.resetSession(conv)
		}
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}
	if resp != nil && resp.Data.Time.Created > 0 {
		promptAt = resp.Data.Time.Created
	}
	s.log.Debugf("responses prompt sid=%s key=%s model=%s", conv.snapshotSessionID(), convKey, ref.String())

	setSessionHeaders(w, conv)
	s.finishResponses(ctx, w, r, req, ref, conv, sub, promptAt, dir)
}

// setSessionHeaders 把本轮的 Janus 会话键与上游 sessionID 回吐给客户端。
// 客户端可用它作为 X-Session-ID 在 Chat / Responses 间锚定同一底层会话。
func setSessionHeaders(w http.ResponseWriter, conv *Conversation) {
	if conv == nil {
		return
	}
	w.Header().Set("X-Janus-Conversation", conv.Key)
	if sid := conv.snapshotSessionID(); sid != "" {
		w.Header().Set("X-Janus-Session", sid)
	}
}

// finishResponses 跑完一轮并按 Responses 格式输出。
func (s *Server) finishResponses(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req ResponsesRequest, ref OCModelRef, conv *Conversation,
	sub *subscription, promptAt int64, dir string) {

	model := s.echoModel(ref, req.Model)

	if req.Stream {
		s.streamResponses(ctx, w, r, req, conv, sub, promptAt, model, dir)
		return
	}

	t, err := s.runBlocking(ctx, conv, model, sub, promptAt, responsesMaxTokens(req), parallelDefault(req.ParallelToolCalls))
	if t != nil {
		s.metrics.addTokens(t.res.usage.PromptTokens, t.res.usage.CompletionTokens)
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			writeOpenAIError(w, http.StatusGatewayTimeout, "api_error", "upstream generation timed out", "timeout")
		} else {
			writeOpenAIError(w, 499, "api_error", "client disconnected", "canceled")
		}
		return
	}

	if len(t.tools) > 0 {
		conv.setPendingToolCalls(t.tools)
		s.metrics.incToolCalls(len(t.tools))
	}
	if t.outcome == "length" {
		s.log.Infof("max_tokens reached: sid=%s (responses), interrupting upstream", t.sid)
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, t.sid)
		}()
	}

	out, text := buildResponsesOutput(t)
	if len(out) == 0 {
		conv.setResponse(nil)
		status, typ, code, msg := t.emptyCompletionError()
		s.log.Warnf("responses empty completion: sid=%s code=%s msg=%s", t.sid, code, msg)
		writeOpenAIError(w, status, typ, msg, code)
		return
	}

	now := time.Now()
	respObj := s.newResponsesResponse(req, model, conv.Key, now)
	respObj.Status = responsesStatus(t)
	respObj.Output = out
	respObj.OutputText = text
	respObj.Usage = usageToResponses(t.res.usage)
	applyIncomplete(respObj, t)
	if respObj.Status == "failed" && t.failure != nil {
		e := openAIError{Message: t.failure.String(), Type: "api_error"}
		respObj.Error = &e
	}
	if req.Store == nil || *req.Store {
		s.responses.put(respObj, conv.Key, dir)
	}
	writeJSON(w, http.StatusOK, respObj)
}

// newResponsesResponse 组装响应骨架。
func (s *Server) newResponsesResponse(req ResponsesRequest, model, convKey string, now time.Time) *ResponsesResponse {
	id := responseIDPrefix + newID()
	obj := &ResponsesResponse{
		ID:                id,
		Object:            "response",
		CreatedAt:         now.Unix(),
		Status:            "in_progress",
		Model:             model,
		ParallelToolCalls: parallelDefault(req.ParallelToolCalls),
	}
	if req.Instructions != "" {
		instr := req.Instructions
		obj.Instructions = &instr
	}
	if req.PreviousResponseID != "" {
		p := req.PreviousResponseID
		obj.PreviousResponseID = &p
	}
	if len(req.Metadata) > 0 && string(req.Metadata) != "null" {
		obj.Metadata = req.Metadata
	}
	for _, t := range responsesToolsToSpecs(req.Tools) {
		obj.Tools = append(obj.Tools, ResponseTool{
			Type: "function", Name: t.Function.Name,
			Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	return obj
}

// deliverToolResults 把客户端的工具结果回填给挂起的 MCP 调用。
// 对齐方式与 Chat / Anthropic 一致（精确 id → 工具名 → 顺序），
// 客户端换用自己生成的 function_call id 也能对上。
func (s *Server) deliverToolResults(conv *Conversation, pending []*pendingCall, results map[string]ToolResult, msgs []ChatMessage) {
	assigned := assignToolResults(pending, results, orderedToolResults(msgs), s.log)
	answered := 0
	for _, p := range pending {
		res, ok := assigned[p.CallID]
		if !ok {
			res = ToolResult{
				Content: "bridge: client did not supply a result for tool call " + p.CallID,
				IsError: true,
			}
		} else {
			answered++
		}
		p.complete(res)
	}
	s.log.Infof("responses tool results delivered: conv=%s answered=%d/%d", conv.Key, answered, len(pending))
}

// responsesMaxTokens 取 Responses 的输出上限。
func responsesMaxTokens(req ResponsesRequest) int {
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		return *req.MaxOutputTokens
	}
	return 0
}

func responsesEffort(req ResponsesRequest) string {
	if req.Reasoning != nil {
		return req.Reasoning.Effort
	}
	return ""
}

// ---------- GET / DELETE ----------

func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	id := r.PathValue("id")
	st := s.responses.get(id)
	if st == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("response %q not found", id), "not_found")
		return
	}
	writeJSON(w, http.StatusOK, st.resp)
}

func (s *Server) handleDeleteResponse(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	id := r.PathValue("id")
	if !s.responses.delete(id) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("response %q not found", id), "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "response.deleted", "deleted": true,
	})
}
