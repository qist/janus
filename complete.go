package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ---------- 流式 ----------

func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request,
	req ChatRequest, ref OCModelRef, conv *Conversation,
	sub *subscription, promptAt int64, planText string) {

	// 本轮结束若不需要再等客户端回填工具结果，就注销本会话的 MCP server，
	// 避免空闲 server 被同 location 的其它会话看到（串会话）。
	defer s.releaseToolsIfIdle(conv)

	model := modelName(ref, req.Model)
	sw, err := newSSE(w, model)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", err.Error(), "")
		return
	}

	started := time.Now()
	s.metrics.streamStart()
	defer s.metrics.streamEnd()

	// handler 返回后绝不能再写 ResponseWriter（详见 markClosed 注释）
	defer sw.markClosed()

	// 心跳：agent 跑工具时可能几分钟没有增量，用 SSE 注释保活
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
				sw.ping()
			}
		}
	}()

	// 客户端断开 → 中断上游执行，避免继续烧 token
	clientGone := r.Context().Done()

	sid := conv.snapshotSessionID()
	ex := newExecutor(s, sid, model, s.cfg.ToolAnnotations)
	ex.toolSess = conv.toolSess
	ex.budget = newTokenBudget(effectiveMaxTokens(req.MaxTokens, req.MaxCompletionTokens))
	ex.writeText = func(t string) error { return sw.deltaText(t) }
	ex.writeReasoning = func(t string) error { return sw.deltaReasoning(t) }

	_ = sw.begin()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ex.runEvents(runCtx, sub, promptAt, s.cfg.ReconcileInterval)

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

	// ---- max_tokens 截断：中断上游并立刻收尾 ----
	if outcome == "length" {
		res := ex.snapshot()
		conv.setResponse(nil)
		s.metrics.addTokens(res.usage.PromptTokens, res.usage.CompletionTokens)
		s.log.Infof("max_tokens reached: sid=%s, interrupting upstream", sid)
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, sid)
		}()
		_ = sw.finish("length", &res.usage,
			req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}

	// ---- 工具调用：agent 要用客户端的工具 ----
	// 把挂起的调用原样回给客户端（finish_reason=tool_calls），本轮到此为止。
	// 挂起的 MCP 请求保持不答，等客户端下一次请求带结果回来。
	if outcome == "tool_calls" {
		pending := ex.toolSnapshot()
		conv.setPendingToolCalls(pending)
		conv.setResponse(nil)
		res := ex.snapshot()
		s.metrics.incToolCalls(len(pending))
		s.metrics.addTokens(res.usage.PromptTokens, res.usage.CompletionTokens)
		s.log.Infof("tool_calls: sid=%s count=%d", sid, len(pending))
		_ = sw.toolCalls(pending)
		_ = sw.finish("tool_calls", &res.usage,
			req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}

	res := ex.snapshot()
	built := buildResponse(sw.id, sw.created, model, res)

	// 空回复：不入缓存，且明确报错而不是静默返回 ""。
	// 实测上游（尤其免费模型被限流时）会拖到 ~90s 后返回完全空的结果，
	// finish=stop、usage=0；静默 200 会让客户端以为"模型就是这么答的"。
	if !responseHasContent(&built) {
		conv.setResponse(nil)
		code, typ := "empty_completion", "api_error"
		msg := "upstream returned an empty completion"
		if uf := ex.failure(); uf != nil {
			// 上游给了明确原因（余额不足 / 限流 / 鉴权…），照实透出来
			_, typ, code, msg = mapUpstreamFailure(uf)
		} else if el := time.Since(started); el > 30*time.Second {
			msg = fmt.Sprintf("upstream returned an empty completion after %s (likely rate-limited or stalled)", el.Round(time.Second))
		} else if outcome == "failed" {
			msg = "opencode session execution failed: " + msg
		}
		s.log.Warnf("empty completion: sid=%s outcome=%s code=%s elapsed=%s msg=%s",
			sid, outcome, code, time.Since(started).Round(time.Second), msg)
		_ = sw.errorEvent(typ, msg, code)
		return
	}

	if responseHasContent(&built) {
		conv.setResponse(&built)
	}
	s.metrics.addTokens(res.usage.PromptTokens, res.usage.CompletionTokens)

	_ = sw.finish(res.finish, &res.usage,
		req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
}

// ---------- 非流式 ----------

// turnOutcome 是一轮执行的汇总，供不同输出格式（Chat / Responses）复用。
type turnOutcome struct {
	res     runResult
	outcome string
	tools   []*pendingCall
	failure *upstreamFailure
	elapsed time.Duration
	model   string
	sid     string
}

// runBlocking 跑完一轮（非流式），只做汇总不做 HTTP 收尾。
// 超时/断连时返回 err；工具调用、空回复等都在返回值里体现。
func (s *Server) runBlocking(ctx context.Context, conv *Conversation, model string,
	sub *subscription, promptAt int64, maxTokens int) (*turnOutcome, error) {

	sid := conv.snapshotSessionID()
	started := time.Now()
	ex := newExecutor(s, sid, model, s.cfg.ToolAnnotations)
	ex.toolSess = conv.toolSess
	ex.budget = newTokenBudget(maxTokens)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go ex.runEvents(runCtx, sub, promptAt, s.cfg.ReconcileInterval)

	var outcome string
	select {
	case outcome = <-ex.done:
	case <-ctx.Done():
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, sid)
		}()
		return nil, ctx.Err()
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
	}
	return t, nil
}

// emptyCompletionError 把"空回复"归一成状态码与错误信息。
func (t *turnOutcome) emptyCompletionError() (status int, typ, code, msg string) {
	status, typ, code = http.StatusBadGateway, "api_error", "empty_completion"
	msg = "upstream returned an empty completion"
	if t.failure != nil {
		status, typ, code, msg = mapUpstreamFailure(t.failure)
	} else if t.elapsed > 30*time.Second {
		msg = fmt.Sprintf("upstream returned an empty completion after %s (likely rate-limited or stalled)", t.elapsed.Round(time.Second))
	} else if t.outcome == "failed" {
		msg = "opencode session execution failed: " + msg
	}
	return status, typ, code, msg
}

func (s *Server) blockingCompletion(ctx context.Context, w http.ResponseWriter,
	req ChatRequest, ref OCModelRef, conv *Conversation,
	sub *subscription, promptAt int64) {

	// 同 streamCompletion：本轮不需要回填工具结果时注销 MCP server。
	defer s.releaseToolsIfIdle(conv)

	model := modelName(ref, req.Model)
	t, err := s.runBlocking(ctx, conv, model, sub, promptAt,
		effectiveMaxTokens(req.MaxTokens, req.MaxCompletionTokens))
	if t != nil {
		s.metrics.addTokens(t.res.usage.PromptTokens, t.res.usage.CompletionTokens)
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			writeOpenAIError(w, http.StatusGatewayTimeout, "api_error",
				"upstream generation timed out", "timeout")
		} else {
			writeOpenAIError(w, 499, "api_error", "client disconnected", "canceled")
		}
		return
	}

	// ---- max_tokens 截断 ----
	if t.outcome == "length" {
		conv.setResponse(nil)
		s.log.Infof("max_tokens reached: sid=%s (non-stream), interrupting upstream", t.sid)
		go func() {
			iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = s.up.Interrupt(iCtx, t.sid)
		}()
		resp := buildResponse("chatcmpl-"+newID(), time.Now().Unix(), model, t.res)
		resp.Choices[0].FinishReason = "length"
		writeJSON(w, http.StatusOK, &resp)
		return
	}

	// ---- 工具调用 ----
	if t.outcome == "tool_calls" {
		conv.setPendingToolCalls(t.tools)
		conv.setResponse(nil)
		s.metrics.incToolCalls(len(t.tools))
		s.log.Infof("tool_calls: sid=%s count=%d (non-stream)", t.sid, len(t.tools))
		writeJSON(w, http.StatusOK, buildToolCallResponse("chatcmpl-"+newID(), model, t.tools))
		return
	}

	resp := buildResponse("chatcmpl-"+newID(), time.Now().Unix(), model, t.res)

	// 空回复：不入缓存，并明确报错。见 streamCompletion 的同款说明。
	if !responseHasContent(&resp) {
		conv.setResponse(nil)
		status, typ, code, msg := t.emptyCompletionError()
		s.log.Warnf("empty completion: sid=%s outcome=%s code=%s elapsed=%s msg=%s",
			t.sid, t.outcome, code, t.elapsed.Round(time.Second), msg)
		writeOpenAIError(w, status, typ, msg, code)
		return
	}

	conv.setResponse(&resp)
	writeJSON(w, http.StatusOK, &resp)
}
