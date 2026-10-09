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

var errNoFlusher = errors.New("response writer does not support flushing")

// responses_stream.go —— Responses API 的 SSE 事件流。
//
// 与 Chat Completions 的差异：每个事件都带 `event:` 名，data 里也带 `type`
// 和自增的 `sequence_number`。客户端据此区分增量、工具调用、结束。

type responsesSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher

	mu     sync.Mutex
	closed bool
	seq    int
}

func newResponsesSSE(w http.ResponseWriter) (*responsesSSE, error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, errNoFlusher
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &responsesSSE{w: w, flusher: fl}, nil
}

func (s *responsesSSE) event(typ string, payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	payload["sequence_number"] = s.seq
	s.seq++

	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("event: " + typ + "\ndata: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// markClosed 让后续写变成 no-op（同 sseWriter，避免 handler 返回后仍写 ResponseWriter）。
func (s *responsesSSE) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *responsesSSE) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, err := s.w.Write([]byte(": ping\n\n")); err != nil {
		return
	}
	s.flusher.Flush()
}

// responsesStream 是 Responses 流式的事件状态机。
//
// 它维护 output[] 的项顺序与各自的生命周期，保证事件序列严格符合 OpenAI：
//
//	reasoning: output_item.added → reasoning_summary_part.added
//	           → reasoning_summary_text.delta* → reasoning_summary_text.done
//	           → reasoning_summary_part.done → output_item.done
//	message:   output_item.added → content_part.added
//	           → output_text.delta* → output_text.done
//	           → content_part.done → output_item.done
//	function_call: output_item.added → function_call_arguments.delta
//	           → function_call_arguments.done → output_item.done
//
// 事件里引用的 item id 与最终 response.output[] 里的完全一致。
type responsesStream struct {
	ss      *responsesSSE
	nextIdx int
	items   []ResponseItem

	rOpen bool
	rIdx  int
	rID   string
	rBuf  strings.Builder

	tOpen bool
	tIdx  int
	tID   string
	tBuf  strings.Builder
}

func newResponsesStream(ss *responsesSSE) *responsesStream {
	return &responsesStream{ss: ss}
}

func (st *responsesStream) event(typ string, payload map[string]any) {
	_ = st.ss.event(typ, payload)
}

func (st *responsesStream) reasoning(t string) {
	if t == "" {
		return
	}
	if !st.rOpen {
		st.closeText() // item 生命周期必须连续
		st.rIdx = st.nextIdx
		st.nextIdx++
		st.rID = reasoningPrefix + newID()[:24]
		item := ResponseItem{Type: "reasoning", ID: st.rID,
			Summary: []ResponseSummary{{Type: "summary_text"}}}
		st.items = append(st.items, item)
		st.event("response.output_item.added", map[string]any{"output_index": st.rIdx, "item": item})
		st.event("response.reasoning_summary_part.added", map[string]any{
			"output_index": st.rIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""}})
		st.rOpen = true
	}
	st.rBuf.WriteString(t)
	st.event("response.reasoning_summary_text.delta", map[string]any{
		"output_index": st.rIdx, "summary_index": 0, "delta": t})
}

func (st *responsesStream) text(t string) {
	if t == "" {
		return
	}
	if !st.tOpen {
		st.closeReasoning() // item 生命周期必须连续，不能交错
		st.tIdx = st.nextIdx
		st.nextIdx++
		st.tID = outputMsgPrefix + newID()[:24]
		item := ResponseItem{Type: "message", ID: st.tID, Role: "assistant", Status: "in_progress",
			Content: []ResponseContent{{Type: "output_text", Annotations: []json.RawMessage{}}}}
		st.items = append(st.items, item)
		st.event("response.output_item.added", map[string]any{"output_index": st.tIdx, "item": item})
		st.event("response.content_part.added", map[string]any{
			"output_index": st.tIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		st.tOpen = true
	}
	st.tBuf.WriteString(t)
	st.event("response.output_text.delta", map[string]any{
		"output_index": st.tIdx, "content_index": 0, "delta": t})
}

func (st *responsesStream) closeReasoning() {
	if !st.rOpen {
		return
	}
	txt := st.rBuf.String()
	item := st.items[st.rIdx]
	item.Summary[0].Text = txt
	st.items[st.rIdx] = item
	st.event("response.reasoning_summary_text.done", map[string]any{
		"output_index": st.rIdx, "summary_index": 0, "text": txt})
	st.event("response.reasoning_summary_part.done", map[string]any{
		"output_index": st.rIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": txt}})
	st.event("response.output_item.done", map[string]any{"output_index": st.rIdx, "item": st.items[st.rIdx]})
	st.rOpen = false
}

func (st *responsesStream) closeText() {
	if !st.tOpen {
		return
	}
	txt := st.tBuf.String()
	item := st.items[st.tIdx]
	item.Status = "completed"
	if len(item.Content) == 0 {
		item.Content = []ResponseContent{{Type: "output_text"}}
	}
	item.Content[0].Text = txt
	item.Content[0].Annotations = []json.RawMessage{}
	st.items[st.tIdx] = item
	st.event("response.output_text.done", map[string]any{
		"output_index": st.tIdx, "content_index": 0, "text": txt})
	st.event("response.content_part.done", map[string]any{
		"output_index": st.tIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": txt, "annotations": []any{}}})
	st.event("response.output_item.done", map[string]any{"output_index": st.tIdx, "item": st.items[st.tIdx]})
	st.tOpen = false
}

func (st *responsesStream) functionCalls(pending []*pendingCall) {
	for _, p := range pending {
		idx := st.nextIdx
		st.nextIdx++
		item := ResponseItem{Type: "function_call", ID: fcPrefix + newID()[:24],
			CallID: p.CallID, Name: p.ToolName, Arguments: p.Args, Status: "completed"}
		st.items = append(st.items, item)
		st.event("response.output_item.added", map[string]any{"output_index": idx, "item": item})
		st.event("response.function_call_arguments.delta", map[string]any{
			"output_index": idx, "item_id": item.ID, "delta": p.Args})
		st.event("response.function_call_arguments.done", map[string]any{
			"output_index": idx, "item_id": item.ID, "arguments": p.Args})
		st.event("response.output_item.done", map[string]any{"output_index": idx, "item": item})
	}
}

// closeAll 在收尾时关闭仍打开的输出项。
func (st *responsesStream) closeAll() {
	st.closeReasoning()
	st.closeText()
}

// streamResponses 以 Responses SSE 格式驱动一轮执行。
func (s *Server) streamResponses(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req ResponsesRequest, conv *Conversation, sub *subscription, promptAt int64, model, dir string) {

	ss, err := newResponsesSSE(w)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", err.Error(), "")
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

	// 响应骨架（先发 created）
	skeleton := s.newResponsesResponse(req, model, conv.Key, started)
	st := newResponsesStream(ss)
	st.event("response.created", map[string]any{"response": skeleton})

	sid := conv.snapshotSessionID()
	ex := newExecutor(s, sid, model, s.cfg.ToolAnnotations)
	ex.toolSess = conv.toolSess
	ex.parallel = parallelDefault(req.ParallelToolCalls)
	ex.budget = newTokenBudget(responsesMaxTokens(req))
	ex.writeText = func(t string) error { st.text(t); return nil }
	ex.writeReasoning = func(t string) error { st.reasoning(t); return nil }

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
		res:     ex.snapshot(),
		outcome: outcome,
		failure: ex.failure(),
		elapsed: time.Since(started),
		model:   model,
		sid:     sid,
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

	// 先关闭已打开的输出项（reasoning / message）
	st.closeAll()

	if len(t.tools) > 0 {
		s.metrics.incToolCalls(len(t.tools))
		st.functionCalls(t.tools)
	} else if !responseHasContent(&ChatResponse{Choices: []Choice{{Message: AssistantMsg{
		Content: t.res.text, ReasoningContent: t.res.reasoning,
	}}}}) {
		conv.setResponse(nil)
		_, typ, code, msg := t.emptyCompletionError()
		s.log.Warnf("responses empty completion: sid=%s code=%s msg=%s", sid, code, msg)
		failed := *skeleton
		failed.Status = "failed"
		e := openAIError{Message: msg, Type: typ, Code: code}
		failed.Error = &e
		st.event("response.failed", map[string]any{"response": failed})
		return
	}

	final := s.newResponsesResponse(req, model, conv.Key, started)
	final.ID = skeleton.ID
	final.Status = responsesStatus(t)
	final.Output = st.items
	if len(t.tools) == 0 {
		final.OutputText = t.res.text
	}
	final.Usage = usageToResponses(t.res.usage)
	applyIncomplete(final, t)
	if req.Store == nil || *req.Store {
		s.responses.put(final, conv.Key, dir)
	}
	st.event("response.completed", map[string]any{"response": final})
}
