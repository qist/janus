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

// streamResponses 以 Responses SSE 格式驱动一轮执行。
func (s *Server) streamResponses(ctx context.Context, w http.ResponseWriter, r *http.Request,
	req ResponsesRequest, conv *Conversation, sub *subscription, promptAt int64, model string) {

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
	_ = ss.event("response.created", map[string]any{"response": skeleton})

	sid := conv.snapshotSessionID()
	ex := newExecutor(s, sid, model, s.cfg.ToolAnnotations)
	ex.toolSess = conv.toolSess
	ex.parallel = parallelDefault(req.ParallelToolCalls)
	ex.budget = newTokenBudget(responsesMaxTokens(req))
	ex.writeText = func(t string) error {
		if t == "" {
			return nil
		}
		return ss.event("response.output_text.delta", map[string]any{
			"output_index": 0, "content_index": 0, "delta": t,
		})
	}
	ex.writeReasoning = func(t string) error {
		if t == "" {
			return nil
		}
		return ss.event("response.reasoning_summary_text.delta", map[string]any{
			"output_index": 0, "summary_index": 0, "delta": t,
		})
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ex.runEvents(runCtx, sub, promptAt, s.cfg.ReconcileInterval)

	clientGone := r.Context().Done()
	var outcome string
	select {
	case outcome = <-ex.done:
	case <-clientGone:
		cancel()
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

	// 工具调用：发 function_call 项
	s.metrics.addTokens(t.res.usage.PromptTokens, t.res.usage.CompletionTokens)
	if t.outcome == "length" {
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, sid)
		}()
	}
	if len(t.tools) > 0 {
		s.metrics.incToolCalls(len(t.tools))
		// function_call 在最终 output[] 里的起始下标：reasoning 项占据 0。
		base := 0
		if strings.TrimSpace(t.res.reasoning) != "" {
			base = 1
		}
		for i, p := range t.tools {
			idx := base + i
			item := ResponseItem{
				Type: "function_call", ID: fcPrefix + newID()[:24],
				CallID: p.CallID, Name: p.ToolName, Arguments: p.Args, Status: "completed",
			}
			_ = ss.event("response.output_item.added", map[string]any{"output_index": idx, "item": item})
			_ = ss.event("response.function_call_arguments.delta", map[string]any{
				"output_index": idx, "item_id": item.ID, "delta": p.Args,
			})
			_ = ss.event("response.function_call_arguments.done", map[string]any{
				"output_index": idx, "item_id": item.ID, "arguments": p.Args,
			})
			_ = ss.event("response.output_item.done", map[string]any{"output_index": idx, "item": item})
		}
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
		_ = ss.event("response.failed", map[string]any{"response": failed})
		return
	}

	out, text := buildResponsesOutput(t)
	final := s.newResponsesResponse(req, model, conv.Key, started)
	final.ID = skeleton.ID
	final.Status = responsesStatus(t)
	final.Output = out
	final.OutputText = text
	final.Usage = usageToResponses(t.res.usage)
	applyIncomplete(final, t)
	if req.Store == nil || *req.Store {
		s.responses.put(final, conv.Key)
	}
	_ = ss.event("response.completed", map[string]any{"response": final})
}
