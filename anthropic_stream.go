package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// anthropic_stream.go —— Anthropic Messages 的 SSE 事件流。
//
// 事件序列：message_start → (content_block_start → content_block_delta* →
// content_block_stop) → message_delta → message_stop。工具块用
// content_block(type=tool_use) + input_json_delta。

var errNoFlusherAnthropic = errors.New("response writer does not support flushing")

type anthropicSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher

	mu      sync.Mutex
	closed  bool
	msgID   string
	model   string
	nextIdx int

	textOpen bool
	textIdx  int
	textBuf  strings.Builder
}

func newAnthropicSSE(w http.ResponseWriter, model string) (*anthropicSSE, error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, errNoFlusherAnthropic
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &anthropicSSE{w: w, flusher: fl, msgID: "msg_" + newID()[:24], model: model}, nil
}

// event 写一个 Anthropic SSE 事件（带 `event:` 名，data 里也带 `type`）。
func (s *anthropicSSE) event(typ string, payload map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventLocked(typ, payload)
}

func (s *anthropicSSE) eventLocked(typ string, payload map[string]any) {
	if s.closed {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := s.w.Write([]byte("event: " + typ + "\ndata: ")); err != nil {
		return
	}
	if _, err := s.w.Write(b); err != nil {
		return
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return
	}
	s.flusher.Flush()
}

func (s *anthropicSSE) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *anthropicSSE) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_, _ = s.w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	s.flusher.Flush()
}

func (s *anthropicSSE) messageStart() {
	s.event("message_start", map[string]any{
		"message": map[string]any{
			"id": s.msgID, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// textDelta 打开（如需要）文本块并写入增量。
func (s *anthropicSSE) textDelta(t string) {
	if t == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !s.textOpen {
		s.textIdx = s.nextIdx
		s.nextIdx++
		s.eventLocked("content_block_start", map[string]any{
			"index": s.textIdx, "content_block": map[string]any{"type": "text", "text": ""}})
		s.textOpen = true
	}
	s.textBuf.WriteString(t)
	s.eventLocked("content_block_delta", map[string]any{
		"index": s.textIdx, "delta": map[string]any{"type": "text_delta", "text": t}})
}

func (s *anthropicSSE) closeText() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.textOpen {
		return
	}
	s.eventLocked("content_block_stop", map[string]any{"index": s.textIdx})
	s.textOpen = false
}

// toolUse 关闭文本块后发一个 tool_use 块（start + input_json_delta + stop）。
func (s *anthropicSSE) toolUse(id, name, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.textOpen {
		s.eventLocked("content_block_stop", map[string]any{"index": s.textIdx})
		s.textOpen = false
	}
	idx := s.nextIdx
	s.nextIdx++
	input := json.RawMessage(args)
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	s.eventLocked("content_block_start", map[string]any{
		"index": idx, "content_block": map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
	s.eventLocked("content_block_delta", map[string]any{
		"index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	s.eventLocked("content_block_stop", map[string]any{"index": idx})
}

func (s *anthropicSSE) messageDelta(stopReason string, in, out int) {
	s.event("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": in, "output_tokens": out},
	})
}

func (s *anthropicSSE) messageStop() {
	s.event("message_stop", nil)
}

func (s *anthropicSSE) errorEvent(typ, msg string) {
	s.event("error", map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}

// streamAnthropic 以 Anthropic SSE 格式驱动一轮执行。
func (s *Server) streamAnthropic(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req AnthropicRequest, ref OCModelRef, conv *Conversation, sub *subscription,
	promptAt int64, model string) {

	ss, err := newAnthropicSSE(w, model)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	defer ss.markClosed()

	started := time.Now()
	s.metrics.streamStart()
	defer s.metrics.streamEnd()

	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(s.cfg.StreamHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				ss.ping()
			}
		}
	}()

	ss.messageStart()

	sid := conv.snapshotSessionID()
	ex := newExecutor(s, sid, model, s.cfg.ToolAnnotations)
	ex.toolSess = conv.toolSess
	ex.setRewrite(conv.rewriteFrom, conv.rewriteTo)
	ex.parallel = true
	ex.budget = newTokenBudget(req.MaxTokens)
	ex.writeText = func(t string) error { ss.textDelta(t); return nil }
	ex.writeReasoning = func(string) error { return nil } // Anthropic 侧暂不下发 thinking

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ex.runEvents(runCtx, sub, promptAt, s.cfg.ReconcileInterval)

	clientGone := r.Context().Done()
	var outcome string
	select {
	case outcome = <-ex.done:
	case <-clientGone:
		cancel()
		// 被终止的这轮绝不能作为缓存回放：客户端若重试同一请求（DiffNone），
		// 不能把被终止的旧答案再发一遍（表现成"终止后继续回答前面的内容"）。
		conv.setResponse(nil)
		conv.markTerminated()
		s.log.Infof("client disconnected, interrupting %s", sid)
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, sid)
		}()
		return
	}

	t := &turnOutcome{
		res: ex.snapshot(), outcome: outcome, failure: ex.failure(),
		elapsed: time.Since(started), model: model, sid: sid,
	}
	if outcome == "tool_calls" {
		t.tools = ex.toolSnapshot()
		conv.setPendingToolCalls(t.tools)
	}
	s.metrics.addTokens(t.res.usage.PromptTokens, t.res.usage.CompletionTokens)
	if t.outcome == "length" {
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, sid)
		}()
	}

	ss.closeText()
	if len(t.tools) > 0 {
		s.metrics.incToolCalls(len(t.tools))
		for _, p := range t.tools {
			ss.toolUse(p.CallID, p.ToolName, p.Args)
		}
	} else if !responseHasContent(&ChatResponse{Choices: []Choice{{Message: AssistantMsg{
		Content: t.res.text, ReasoningContent: t.res.reasoning,
	}}}}) {
		conv.setResponse(nil)
		_, typ, _, msg := t.emptyCompletionError()
		ss.errorEvent(typ, msg)
		return
	}

	ss.messageDelta(anthropicStopReason(t), t.res.usage.PromptTokens, t.res.usage.CompletionTokens)
	ss.messageStop()
}

// streamAnthropicCached 用缓存结果快速回放一个 Anthropic SSE 流。
func (s *Server) streamAnthropicCached(w http.ResponseWriter, resp *AnthropicResponse) {
	ss, err := newAnthropicSSE(w, resp.Model)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	defer ss.markClosed()
	ss.messageStart()
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			ss.textDelta(c.Text)
			ss.closeText()
		case "tool_use":
			ss.toolUse(c.ID, c.Name, string(c.Input))
		}
	}
	ss.closeText()
	ss.messageDelta(resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	ss.messageStop()
}
