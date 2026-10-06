package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// upstreamFailure 是上游 assistant 消息里带的失败原因。
//
// 实测形状（余额不足时）：
//
//	{"type":"provider.quota","message":"Upstream request failed: Insufficient account funds","status":402}
type upstreamFailure struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (f *upstreamFailure) String() string {
	if f == nil {
		return ""
	}
	msg := f.Message
	if msg == "" {
		msg = f.Type
	}
	if f.Status > 0 {
		return fmt.Sprintf("%s (status %d)", msg, f.Status)
	}
	return msg
}

// mapUpstreamFailure 把上游失败翻译成 OpenAI 语义的 HTTP 状态与错误码。
//
// 余额不足按 OpenAI 惯例走 429 + insufficient_quota：SDK 对这个组合有成熟的
// 处理（提示充值而不是无脑重试），比 402 兼容性好。
func mapUpstreamFailure(f *upstreamFailure) (status int, typ, code, msg string) {
	if f == nil {
		return http.StatusBadGateway, "api_error", "upstream_error", "upstream request failed"
	}
	m := strings.ToLower(f.Message)
	t := strings.ToLower(f.Type)
	switch {
	case strings.Contains(m, "free tier") || strings.Contains(t, "freetier") ||
		strings.Contains(m, "only be used from within opencode"):
		// OpenCode 免费档被网关限定为"仅官方客户端可用"，第三方/API 一律 403。
		// 这是上游策略，不是桥的问题；给出可操作的提示而不是笼统的鉴权失败。
		return http.StatusForbidden, "api_error", "free_tier_restricted",
			f.String() + "；该免费模型仅限 OpenCode 官方客户端使用，请改用 opencode-go/* 订阅模型，或在 OpenCode 里配置自己的 provider"
	case strings.Contains(t, "quota"), f.Status == http.StatusPaymentRequired,
		strings.Contains(m, "insufficient"):
		return http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota", f.String()
	case strings.Contains(t, "rate"), f.Status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded", f.String()
	case f.Status == http.StatusUnauthorized, strings.Contains(t, "auth"):
		return http.StatusBadGateway, "api_error", "upstream_auth_error", f.String()
	default:
		return http.StatusBadGateway, "api_error", "upstream_error", f.String()
	}
}

// ---------- 一次补全的执行上下文 ----------

type runResult struct {
	text      string
	reasoning string
	finish    string
	usage     Usage
	toolCalls int
	truncated bool // 因 max_tokens 近似上限被截断
}

// executor 驱动一个 OpenCode session 完成一次 prompt，把事件汇聚成 OpenAI 语义。
type executor struct {
	srv   *Server
	sid   string
	model string // OpenAI 形式的 model 名，用于 chunk

	// 下游写出（流式时为 SSE，非流式为 nil）
	writeText      func(string) error
	writeReasoning func(string) error
	toolAnn        bool
	parallel       bool // 是否允许一轮并行返回多个 tool_calls（OpenAI 默认 true）

	// 汇总
	mu sync.Mutex
	// text = 最终输出（上游正文 + 工具注释），是回给客户端的内容
	text strings.Builder
	// serverText = 上游纯文本，只用来和 /message 对账拿精确偏移
	serverText strings.Builder
	reasoning  strings.Builder
	finish     string
	usage      Usage
	toolCalls  int

	seenStarted bool
	upFail      *upstreamFailure // 上游给出的失败原因（若有）
	done        chan string      // 终态："succeeded"/"failed"/"interrupted"/"timeout"/"tool_calls"/"stalled"
	lastData    atomic.Int64     // 最近一次"真实数据"（delta/事件）的时间（毫秒）

	// 工具调用：agent 通过内置 MCP server 调用了客户端声明的工具
	toolSess *toolSession
	toolsOut []*pendingCall

	// toolNames 记录 callID → 工具名（session.tool.input.started 给出，
	// 结果事件里只有 callID，用它把结果注释回上具名）。
	toolNames map[string]string

	// 输出预算（max_tokens 的近似实现）
	budget *tokenBudget
}

func newExecutor(srv *Server, sid, model string, toolAnn bool) *executor {
	e := &executor{srv: srv, sid: sid, model: model, toolAnn: toolAnn, parallel: true,
		toolNames: map[string]string{}, done: make(chan string, 4)}
	e.touchData()
	return e
}

// touchData 记录一次"真实数据"（模型 delta / 上游 session 事件）。
// 流式空闲超时据此判断是否卡死；心跳（keepAlive）不算，否则永远不触发。
func (e *executor) touchData() { e.lastData.Store(time.Now().UnixMilli()) }

// idleFor 返回距上次真实数据过了多久。
func (e *executor) idleFor() time.Duration {
	return time.Since(time.UnixMilli(e.lastData.Load()))
}

func (e *executor) addText(s string) error {
	if s == "" {
		return nil
	}
	// max_tokens 近似上限：超出后丢弃后续正文（finish_reason 会标 length）
	if !e.budget.allow(s) {
		return nil
	}
	e.mu.Lock()
	e.text.WriteString(s)
	e.serverText.WriteString(s)
	e.mu.Unlock()
	if e.writeText != nil {
		return e.writeText(s)
	}
	return nil
}

// addToolText 工具活动注释：进最终展示，但不写 serverText（对账基准）。
func (e *executor) addToolText(s string) error {
	if s == "" {
		return nil
	}
	e.mu.Lock()
	e.text.WriteString(s)
	e.mu.Unlock()
	if e.writeText != nil {
		return e.writeText(s)
	}
	return nil
}

func (e *executor) addReasoning(s string) error {
	if s == "" {
		return nil
	}
	e.mu.Lock()
	e.reasoning.WriteString(s)
	e.mu.Unlock()
	if e.writeReasoning != nil {
		return e.writeReasoning(s)
	}
	return nil
}

func (e *executor) addTokens(t *OCTokens) {
	if t == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// 上游 t.Input 是「未命中缓存」的新增输入，t.Cache.Read 是复用缓存的部分；
	// OpenAI 语义里 prompt_tokens 是两者之和，cached_tokens 才是命中的子集。
	// t.Output 是可见正文、t.Reason 是思考，两者独立计数；completion_tokens 含
	// reasoning（后者是明细子集），故相加，保证「输出 = 思考 + 回复」自洽。
	e.usage.PromptTokens += t.Input + t.Cache.Read
	e.usage.CompletionTokens += t.Output + t.Reason
	if e.usage.PromptTokensDetails == nil {
		e.usage.PromptTokensDetails = &TokenDetails{}
	}
	if e.usage.CompletionTokensDetails == nil {
		e.usage.CompletionTokensDetails = &CompletionDetails{}
	}
	e.usage.PromptTokensDetails.CachedTokens += t.Cache.Read
	e.usage.PromptTokensDetails.CacheCreationTokens += t.Cache.Write
	e.usage.CompletionTokensDetails.ReasoningTokens += t.Reason
}

func (e *executor) setFinish(f string) {
	if f == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch f {
	case "tool-calls":
		e.finish = "tool_calls"
	case "content-filter":
		e.finish = "content_filter"
	case "length":
		e.finish = "length"
	case "stop", "error", "unknown", "":
		if e.finish == "" {
			e.finish = "stop"
		}
	default:
		if e.finish == "" {
			e.finish = "stop"
		}
	}
}

func (e *executor) setFailure(f *upstreamFailure) {
	if f == nil {
		return
	}
	e.mu.Lock()
	if e.upFail == nil {
		e.upFail = f
	}
	e.mu.Unlock()
}

func (e *executor) failure() *upstreamFailure {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.upFail
}

func (e *executor) signal(t string) {
	select {
	case e.done <- t:
	default:
	}
}

func (e *executor) snapshot() runResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.usage
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	// DeepSeek 风格缓存明细：hit=命中、miss=未命中，二者之和=prompt_tokens。
	if u.PromptTokensDetails != nil {
		u.PromptCacheHitTokens = u.PromptTokensDetails.CachedTokens
		u.PromptCacheMissTokens = u.PromptTokens - u.PromptTokensDetails.CachedTokens
	}
	fin := e.finish
	if fin == "" {
		fin = "stop"
	}
	trunc := e.budget != nil && e.budget.truncated
	// 被 max_tokens 截断 → finish_reason=length（OpenAI 语义）
	if trunc && (fin == "stop" || fin == "") {
		fin = "length"
	}
	return runResult{
		text:      e.text.String(),
		reasoning: e.reasoning.String(),
		finish:    fin,
		usage:     u,
		toolCalls: e.toolCalls,
		truncated: trunc,
	}
}

// budgetExhausted 报告 max_tokens 近似预算是否已用完。
func (e *executor) budgetExhausted() bool {
	return e.budget != nil && e.budget.truncated
}

// toolSnapshot 返回本轮收集到的、等待客户端执行的工具调用。
func (e *executor) toolSnapshot() []*pendingCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*pendingCall, len(e.toolsOut))
	copy(out, e.toolsOut)
	return out
}

// reconcile 用消息接口补齐事件流丢失的尾巴，返回是否看到了 assistant 消息。
//
// 单次对账：周期性 reconcile（tick）调用它；收尾时若还没看到消息，用
// reconcileTerminal 做短退避重试（终态事件可能早于消息落库）。
func (e *executor) reconcile(ctx context.Context, promptAt int64) bool {
	msgs, err := e.srv.up.ListMessages(ctx, e.sid, "asc", 200)
	if err != nil {
		e.srv.log.Debugf("reconcile list messages failed: %v", err)
		return false
	}

	var full strings.Builder
	var reasoning strings.Builder
	var finish string
	var sawAssistant bool

	for i := range msgs {
		m := &msgs[i]
		if m.Type != "assistant" {
			continue
		}
		if m.Time.Created > 0 && m.Time.Created < promptAt {
			continue
		}
		sawAssistant = true
		if len(m.Error) > 0 && string(m.Error) != "null" {
			var f upstreamFailure
			if json.Unmarshal(m.Error, &f) == nil && (f.Type != "" || f.Message != "") {
				e.setFailure(&f)
			}
		}
		full.WriteString(m.PlainText())
		reasoning.WriteString(m.Reasoning())
		if m.Finish != "" {
			finish = m.Finish
		}
		if m.Tokens != nil {
			// 对账时 tokens 可能与事件重复，用 max 语义：
			// 这里只在从未收到 step.ended 时补一次。
			e.mu.Lock()
			neverGot := e.usage.CompletionTokens == 0
			e.mu.Unlock()
			if neverGot {
				e.addTokens(m.Tokens)
			}
		}
	}
	if !sawAssistant {
		return false
	}

	// 推理：整体补齐（推理通常在正文前，重复发送风险低）
	if got := reasoning.String(); got != "" {
		e.mu.Lock()
		have := e.reasoning.String()
		e.mu.Unlock()
		if len(got) > len(have) {
			_ = e.addReasoning(got[len(have):])
		}
	}

	// 正文：只和 serverText（上游纯文本，不含工具注释）比，才能拿到精确偏移。
	want := full.String()
	e.mu.Lock()
	have := e.serverText.String()
	e.mu.Unlock()

	switch {
	case want == have:
		// 完全一致，什么都不做
	case strings.HasPrefix(want, have):
		_ = e.addText(want[len(have):])
	case strings.HasPrefix(have, want):
		// 上游文本反而比我们收到的短：说明部分 delta 来自被截断/重写的场景，
		// 无害，不动已发出的内容。
	default:
		// 分叉了（事件流丢了一段，或上游改写了）。已经发出去的内容撤不回来，
		// 只补未覆盖的尾部，并明确告警，方便排查。
		if len(want) > len(have) {
			e.srv.log.Warnf("reconcile diverged for %s: upstream=%dB streamed=%dB, appending tail",
				e.sid, len(want), len(have))
			_ = e.addText(want[len(have):])
		} else {
			e.srv.log.Warnf("reconcile diverged for %s: upstream=%dB streamed=%dB, keeping streamed",
				e.sid, len(want), len(have))
		}
	}

	if finish != "" {
		e.setFinish(finish)
	}
	return true
}

// reconcileTerminal 在收尾时对账：终态事件（succeeded / idle）有时早于
// assistant 消息落库，若首次没看到消息就短退避重试，避免把"有输出"误判成空回复。
func (e *executor) reconcileTerminal(ctx context.Context, promptAt int64) {
	if e.reconcile(ctx, promptAt) {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		if e.reconcile(ctx, promptAt) {
			return
		}
	}
	e.srv.log.Warnf("reconcile gave up for %s (no assistant message after 5s)", e.sid)
}

// ---------- 事件循环 ----------

// runEvents 阻塞直到终态、超时或 ctx 取消。
// promptAt 用于过滤上一轮遗留事件。
func (e *executor) runEvents(ctx context.Context, sub *subscription, promptAt int64, reconcileEvery time.Duration) {
	srv := e.srv

	reconcileTick := time.NewTicker(reconcileEvery)
	defer reconcileTick.Stop()

	// 工具调用：agent 通过 MCP 调用了客户端声明的工具时，立刻收尾本轮，
	// 把 tool_calls 交给客户端去执行。挂起的 MCP 请求保持不答。
	var toolWatch chan struct{}
	if e.toolSess != nil {
		toolWatch = e.toolSess.watch()
		defer e.toolSess.unwatch(toolWatch)
	}

	// 兜底：事件流彻底失效时用 wait() 收尾
	waitDone := make(chan struct{}, 1)
	go func() {
		// 派生自 ctx：请求一旦结束（客户端断开/超时）这个 wait 立刻被取消，
		// 不会残留到 RequestTimeout。用 Background() 会随每次断连泄漏一个协程。
		wctx, cancel := context.WithTimeout(ctx, srv.cfg.RequestTimeout)
		defer cancel()
		if err := srv.up.WaitUntilIdle(wctx, e.sid); err == nil {
			select {
			case waitDone <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			e.signal("timeout")
			return

		case <-toolWatch:
			// agent 调用了客户端的工具：把这一批挂起调用收上来，结束本轮。
			// parallel=false 时只取最早的一个，其余留在会话里，等客户端回填后再逐个给。
			var pend []*pendingCall
			if e.parallel {
				pend = e.toolSess.takePending()
			} else if p := e.toolSess.takeOldest(); p != nil {
				pend = []*pendingCall{p}
			}
			if len(pend) > 0 {
				e.mu.Lock()
				e.toolsOut = append(e.toolsOut, pend...)
				e.mu.Unlock()
				e.signal("tool_calls")
				return
			}

		case <-waitDone:
			// wait() 返回说明 agent loop 已空闲。再确认一次 idle 时间戳，
			// 避免"prompt 尚未开始执行"时的误判，并取回上游 outcome。
			if idle, outcome := e.sessionState(ctx, promptAt); idle {
				e.reconcileTerminal(ctx, promptAt)
				e.signal(e.terminalSignal(outcome))
				return
			}
			// 还没真正开始，隔一会儿再挂一个 wait。
			// 必须退避：wait 在"会话本来就空闲"时会立刻返回，直接重挂会变成
			// 每秒上千次打上游的忙等。
			waitDone = make(chan struct{}, 1)
			go func(ch chan struct{}) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
				wctx, cancel := context.WithTimeout(ctx, srv.cfg.RequestTimeout)
				defer cancel()
				if err := srv.up.WaitUntilIdle(wctx, e.sid); err == nil {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}(waitDone)

		case <-reconcileTick.C:
			// 流式空闲超时：上游卡住（长时间没有任何真实数据，心跳不算）→ 中断并收尾。
			// 一直有真实输出的长回答不受影响（每次事件都会 touchData）。
			if idle := srv.cfg.StreamIdleTimeout; idle > 0 && e.idleFor() > idle {
				srv.log.Warnf("stream idle timeout: no data for %s (sid=%s); interrupting upstream",
					e.idleFor().Round(time.Second), e.sid)
				go func() {
					iCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
					defer c()
					_ = srv.up.Interrupt(iCtx, e.sid)
				}()
				e.signal("stalled")
				return
			}
			// max_tokens 预算耗尽：立刻收尾（调用方会 interrupt 上游），
			// 否则客户端还得等上游把整段生成完 —— 那不是 OpenAI 的语义。
			if e.budgetExhausted() {
				e.signal("length")
				return
			}
			e.reconcile(ctx, promptAt)
			if idle, outcome := e.sessionState(ctx, promptAt); idle {
				// 事件流可能丢了 succeeded 事件，这里才判到空闲；同样要对账
				// （消息可能还没落库，reconcileTerminal 会退避重试）。
				e.reconcileTerminal(ctx, promptAt)
				e.signal(e.terminalSignal(outcome))
				return
			}

		case ev, ok := <-sub.ch:
			if !ok {
				e.signal("timeout")
				return
			}
			if e.budgetExhausted() {
				e.signal("length")
				return
			}
			if ev.Created > 0 && ev.Created < promptAt-2000 {
				continue
			}
			if e.handleEvent(ctx, ev, promptAt) {
				return
			}
		}
	}
}

// terminalSignal 把上游 outcome 归一成内部终态信号。
// 上游 outcome 为 failed 时必须报 failed —— 否则余额不足/被拒这类错误会被
// 当成"成功但内容为空"，客户端拿不到真正原因。
func (e *executor) terminalSignal(outcome string) string {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "failed":
		return "failed"
	default:
		return "succeeded"
	}
}

// sessionState 判断「本轮是否已结束」，并带回上游给出的 outcome。
//
// 只看 idle 时间戳不够：上游可能以 failed 收尾（例如 provider.quota 余额不足），
// 而 execution.failed 事件没被我们收到。此时若只报 succeeded，客户端会以为
// "模型就是这么答的"，拿不到真正的原因。
func (e *executor) sessionState(ctx context.Context, promptAt int64) (idle bool, outcome string) {
	sess, err := e.srv.up.GetSession(ctx, e.sid)
	if err != nil {
		return false, ""
	}
	e.mu.Lock()
	started := e.seenStarted
	e.mu.Unlock()

	outcome = sess.Outcome
	if !started {
		// 还没观测到执行开始时，仅凭 idle 判断容易误判（上一轮遗留的 idle）。
		// 此时要求 idle 晚于 prompt 至少一个 grace。
		return sess.Time.Idle >= promptAt && time.Now().UnixMilli()-promptAt > 3000, outcome
	}
	return sess.Time.Idle >= promptAt, outcome
}

// handleEvent 处理单条事件，返回 true 表示已到终态。
func (e *executor) handleEvent(ctx context.Context, ev OCEvent, promptAt int64) bool {
	var sess evtSession
	if json.Unmarshal(ev.Data, &sess) != nil || sess.SessionID != e.sid {
		return false
	}
	// 本会话有事件 = 上游还活着（delta / step / tool / usage 都算真实数据）。
	e.touchData()

	switch ev.Type {
	case "session.execution.started":
		e.mu.Lock()
		e.seenStarted = true
		e.mu.Unlock()

	case "session.step.started":
		e.mu.Lock()
		e.seenStarted = true
		e.mu.Unlock()

	case "session.text.delta":
		var d evtTextDelta
		if json.Unmarshal(ev.Data, &d) == nil && d.Delta != "" {
			_ = e.addText(d.Delta)
		}

	case "session.reasoning.delta":
		var d evtTextDelta
		if json.Unmarshal(ev.Data, &d) == nil && d.Delta != "" {
			_ = e.addReasoning(d.Delta)
		}

	case "session.tool.input.started":
		var d evtToolInput
		if json.Unmarshal(ev.Data, &d) == nil && d.Name != "" {
			e.srv.log.Debugf("tool event started: name=%q text=%q data=%s", d.Name, d.Text, truncate(string(ev.Data), 400))
			e.mu.Lock()
			e.toolCalls++
			if d.ID != "" {
				e.toolNames[d.ID] = d.Name
			}
			e.mu.Unlock()
			// 这里不注空名（那正是满屏 "glob:" / "subagent:" 噪声的来源）；
			// 入参由 input.ended 注、结果由 success/failed 注。
		}

	case "session.tool.success", "session.tool.failed":
		// 工具结果：把产出（文件内容 / 命令输出 / 子代理结论）注释进正文，
		// 否则工具型任务（如整项目审计）期间正文几乎为空，客户端看不到任何进展。
		if !e.toolAnn {
			return false
		}
		var d evtToolResult
		if json.Unmarshal(ev.Data, &d) == nil {
			e.mu.Lock()
			name := e.toolNames[d.ID]
			delete(e.toolNames, d.ID)
			e.mu.Unlock()
			// MCP 工具（"execute"）的真实调用已由 tool bridge 下发，注解属噪声。
			if name != "" && !isMCPPlaceholderTool(name) {
				if out := toolResultText(d); out != "" {
					_ = e.addToolText(toolResultAnnotation(name, out))
				}
			}
		}

	case "session.tool.input.ended":
		// 带完整入参：补一条更详细的注释
		if !e.toolAnn {
			return false
		}
		var d evtToolInput
		if json.Unmarshal(ev.Data, &d) == nil && d.Name != "" && d.Text != "" {
			e.srv.log.Debugf("tool event ended: name=%q text=%q", d.Name, truncate(d.Text, 200))
			if !isMCPPlaceholderTool(d.Name) {
				_ = e.addToolText(ToolAnnotation(d.Name, d.Text))
			}
		}

	case "session.step.ended":
		var d evtStepEnded
		if json.Unmarshal(ev.Data, &d) == nil {
			e.setFinish(d.Finish)
			e.addTokens(d.Tokens)
		}

	case "session.usage.updated":
		// usage 是累计值，step.ended 已经逐段累加，这里忽略避免重复计数

	case "session.execution.succeeded":
		e.reconcileTerminal(ctx, promptAt)
		e.signal("succeeded")
		return true

	case "session.execution.failed":
		e.reconcileTerminal(ctx, promptAt)
		e.signal("failed")
		return true

	case "session.execution.interrupted":
		e.signal("interrupted")
		return true
	}
	return false
}

// ---------- 组装 OpenAI 响应 ----------

func buildResponse(id string, created int64, model string, r runResult) ChatResponse {
	msg := AssistantMsg{Role: "assistant", Content: r.text}
	if r.reasoning != "" {
		msg.ReasoningContent = r.reasoning
	}
	return ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: r.finish}},
		Usage:   &r.usage,
	}
}

// ---------- 主流程 ----------

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}

	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"invalid JSON body: "+err.Error(), "")
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"messages must be a non-empty array", "invalid_messages")
		return
	}

	// ---- 模型解析 ----
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

	// 思考强度：标准字段 reasoning_effort → 上游 model variant。
	// 若客户端已经在 model 串里写了 ":variant"，以它为准（更具体）。
	if ref.Variant == "" && strings.TrimSpace(req.ReasoningEffort) != "" {
		if m := FindModel(ocModels, ref); m != nil {
			if v, ok := PickVariant(*m, req.ReasoningEffort); ok {
				ref.Variant = v
			} else {
				// 模型没有可选思考档位时忽略 effort 是预期行为，降为 DEBUG 免刷屏
				s.log.Debugf("model %s has no reasoning variant for effort %q (available: %v); ignoring",
					ref.ProviderID+"/"+ref.ID, req.ReasoningEffort, VariantIDs(*m))
			}
		}
	}

	// ---- 会话定位 ----
	explicit := firstNonEmpty(r.Header.Get("X-Session-ID"), r.Header.Get("X-OpenCode-Session"))
	// 会话目录：客户端项目路径在 janus 主机上不存在时用 per-scope 中性目录（见 scope.go）。
	scopeK, _ := s.scopeOf(r, r.Header.Get("X-OpenCode-Directory"), req.Messages)
	dir := s.sessionDir(r.Header.Get("X-OpenCode-Directory"), req.Messages, scopeK)
	agent := firstNonEmpty(r.Header.Get("X-OpenCode-Agent"), s.cfg.Agent)

	key := s.conversationKey(r, explicit, firstSystem(req.Messages), req.User, dir, req.Messages)
	s.logClientInfo(r, req.User, dir, key, req.Messages)
	// scope 模式（无会话 id 的兜底）：一个 scope 一个会话，不做历史前缀分桶。
	var conv *Conversation
	if s.cfg.ScopeKey && explicit == "" {
		conv = s.store.AcquireKey(key)
	} else {
		conv = s.store.Acquire(key, req.Messages)
	}
	defer s.store.Release(conv)

	// 上一轮被客户端中止过（用户点了终止 / 断连）：新内容不能再续接到那个
	// 残缺会话上，否则新任务会看到旧任务的上下文和中断残影。重开干净会话。
	if conv.takeTerminated() {
		s.log.Infof("previous turn was terminated; starting a fresh session (key=%s)", conv.Key)
		s.resetSession(conv)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	// ---- 工具：客户端回填了上轮的工具结果 → 唤醒 agent，不重发 prompt ----
	//
	// 上游 agent 此刻正停在 MCP tools/call 上等我们回结果。这条路径必须走完整：
	// 先把 agent 放行，再按普通一轮收尾（它可能给最终答案，也可能再次调工具）。
	if s.cfg.ToolCalling && conv.mcpName != "" && conv.snapshotSessionID() != "" &&
		hasToolResults(req.Messages) && len(conv.pendingToolCalls()) > 0 {

		pending := conv.pendingToolCalls()
		conv.setPendingToolCalls(nil)
		conv.setLast(cloneMessages(req.Messages))
		s.persistConv(conv)

		sid := conv.snapshotSessionID()
		sub := s.bus.Subscribe(sid, 512)
		defer sub.cancel()

		// 先订阅再回填，避免漏掉 agent 继续执行时的开头增量
		s.resumeToolCalls(ctx, w, r, req, ref, conv, pending,
			toolResultsFromMessages(req.Messages), dir, sub, time.Now().UnixMilli())
		return
	}

	// ---- 历史差分 ----
	stored := conv.snapshotLast()
	mode, delta := Diff(stored, req.Messages)

	// DiffReset 必须先于「工具注册」处理：resetSession 会注销本会话的工具桥，
	// 若先注册再重置，新会话就会没有工具（agent 只剩内置工具）。
	if mode == DiffReset && conv.snapshotSessionID() != "" {
		if s.cfg.ScopeKey && explicit == "" {
			// scope 模式：明确退化为「一个 scope 一个上下文」——不重置，追加最后一条 user。
			if d := lastUserTurn(req.Messages); len(d) > 0 {
				s.log.Infof("scope mode: appending last user turn to shared context (scope=%s)", conv.Key)
				mode, delta = DiffAppend, d
			} else {
				s.resetSession(conv)
			}
		} else if d, ok := TolerateReset(stored, req.Messages); ok {
			s.log.Infof("history mismatch tolerated, appending last user turn (key=%s)", conv.Key)
			mode, delta = DiffAppend, d
		} else {
			s.resetSession(conv)
		}
	}

	// ---- 工具：注册/更新（客户端声明了 tools）或注销（不再声明）----
	if s.cfg.ToolCalling {
		switch {
		case len(req.Tools) > 0:
			if err := s.ensureTools(ctx, conv, dir, req.Tools); err != nil {
				// 客户端明确声明了 tools 却注册不上：必须让它知道。
				// 静默降级成"无工具"更糟 —— 模型会凭自己的能力硬答，
				// 客户端却以为工具可用（实测上游对某些 directory 会 500）。
				if s.cfg.ToolSoftFail {
					s.log.Warnf("tool bridge registration failed (soft-fail, continuing without tools): %v", err)
				} else {
					s.log.Warnf("tool bridge registration failed: %v", err)
					writeOpenAIError(w, http.StatusBadGateway, "api_error",
						fmt.Sprintf("cannot expose your tools to the upstream agent: %v "+
							"(directory=%s). Set BRIDGE_TOOL_SOFT_FAIL=true to continue without tools.",
							err, dir),
						"tool_registration_failed")
					return
				}
			}
		case conv.mcpName != "":
			s.removeTools(ctx, conv, dir)
		}
	}

	if mode == DiffNone {
		// 缓存里是空回复时不能回放 —— 上游偶发空回复（实测有过一次 91s 后返回空），
		// 一旦缓存下来，之后每个完全相同的请求都会拿到这个空结果，看起来像"桥坏了"。
		// 这种情况当作"没有可用缓存"，退回重跑最后一条 user 消息。
		if cached := conv.snapshotResponse(); responseHasContent(cached) {
			s.replyCached(w, r, req, ref, conv, cached)
			return
		}
		retry := lastUserTurn(req.Messages)
		if len(retry) == 0 {
			s.replyCached(w, r, req, ref, conv, nil)
			return
		}
		s.log.Debugf("no usable cached response for %s; re-running last user turn", conv.Key)
		delta = retry
		mode = DiffAppend
	}

	// 要新建会话（sessionID 为空）时必须发完整历史：库里可能还留着上一轮历史快照，
	// Diff 会算成 append 只发增量，而新会话没有旧上下文。清 session_id 后重建时靠这里兜底。
	if conv.snapshotSessionID() == "" {
		delta = req.Messages
	}

	// ---- 会话就绪 ----
	newSession, err := s.ensureSession(ctx, conv, ref, agent, dir, req.Messages)
	if err != nil {
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}

	// ---- 发 prompt（必须先订阅事件再发，否则丢开头）----
	planText := FlattenDelta(delta)

	// 图片：data URI 直传，http(s) 下载转 base64。失败的 URL 以文字说明补进 prompt，
	// 免得模型以为用户根本没发图（实测会出现"未看到图片"这种误导性回答）。
	files, failed := ExtractAttachments(ctx, delta, s.httpc)
	// 模型不支持该模态就别硬塞（例如纯文本模型收到图片）：丢弃并说明。
	if m := FindModel(ocModels, ref); len(files) > 0 {
		var dropped []OCFileAttach
		files, dropped = filterAttachmentsByModel(files, m)
		for _, f := range dropped {
			failed = append(failed, f.Name)
		}
	}
	if len(failed) > 0 {
		s.log.Warnf("attachments dropped for %s: %d item(s)", conv.Key, len(failed))
		planText += attachFailureNote(failed)
	}
	if len(files) > 0 {
		s.log.Debugf("attaching %d file(s) to prompt", len(files))
	}

	sid := conv.snapshotSessionID()
	setSessionHeaders(w, conv)
	sub := s.bus.Subscribe(sid, 512)
	defer sub.cancel()

	promptAt := time.Now().UnixMilli()
	resp, err := s.up.Prompt(ctx, sid, OCPromptReq{Text: planText, Files: files})
	if err != nil && len(files) > 0 {
		// 上游对附件格式挑剔，附件导致失败时降级重发一次纯文本，保住对话本身
		s.log.Warnf("prompt with %d attachment(s) failed, retrying as plain text: %v", len(files), err)
		planText = FlattenDelta(delta) + attachFailureNote(failed)
		files = nil
		resp, err = s.up.Prompt(ctx, sid, OCPromptReq{Text: planText})
	}
	if err != nil {
		// 本次刚建的空会话直接丢弃，否则它会以「有 sessionID 但无历史」的
		// 状态留在桶里，下次请求得靠 DiffReset 兜底重建，白白多一轮。
		if newSession {
			s.resetSession(conv)
		} else if isSessionGone(err) {
			// 上游把会话弄丢了（OpenCode 重启/清理）：清掉本地引用，
			// 下次请求会重建会话并重放完整历史（见上面的 sessionID=="" 分支）。
			s.log.Warnf("upstream session %s is gone (key=%s); will recreate on next request", sid, conv.Key)
			s.resetSession(conv)
		}
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}
	if resp != nil && resp.Data.Time.Created > 0 {
		promptAt = resp.Data.Time.Created
	}
	s.log.Debugf("prompt sent sid=%s key=%s delta=%d bytes model=%s",
		sid, conv.key(), len(planText), ref.String())

	// 无论成败都快照历史，避免客户端下次差分失败导致重复发送
	conv.setLast(cloneMessages(req.Messages))
	s.persistConv(conv)

	// ---- 流式 / 非流式 ----
	if req.Stream {
		s.streamCompletion(w, r, req, ref, conv, sub, promptAt, planText)
		return
	}
	s.blockingCompletion(ctx, w, req, ref, conv, sub, promptAt)
}

func (c *Conversation) key() string { return c.Key }

// resolveModel 解析客户端传来的 model 字符串。
// 支持三种"用上游默认"的写法，避免客户端/文档写死具体模型名：
//
//	"" / "default" / "auto"
//
// 先过一遍别名映射（BRIDGE_MODEL_MAP，如 claude-3-5-sonnet=...），
// 其余交给 ResolveModel 按 provider/model 或裸名解析。
func (s *Server) resolveModel(ctx context.Context, raw string, list []OCModel) (OCModelRef, error) {
	if isJanusAlias(raw) {
		ref, err := s.resolveJanusModel(ctx, list)
		if err == nil {
			s.rememberModel(ref)
		}
		return ref, err
	}
	if isDefaultAlias(raw) {
		ref, err := s.resolveDefaultModel(ctx, list)
		if err == nil {
			s.rememberModel(ref)
		}
		return ref, err
	}
	ref, err := ResolveModel(s.mapModelName(raw), list)
	if err == nil {
		// 客户端显式给了真实模型：记下来，供 claude-* 别名兜底（无需配置）。
		s.rememberModel(ref)
	}
	return ref, err
}

// mapModelName 应用 BRIDGE_MODEL_MAP（键不区分大小写）；无映射则原样返回。
// 键以 '*' 结尾表示前缀匹配（如 claude-opus*=... 匹配 claude-opus-4-1）。
func (s *Server) mapModelName(raw string) string {
	raw = stripContextSuffix(strings.TrimSpace(raw))
	if len(s.cfg.ModelMap) == 0 {
		return raw
	}
	lower := strings.ToLower(raw)
	if to, ok := s.cfg.ModelMap[lower]; ok {
		return to
	}
	// 前缀匹配取最长者
	bestLen, bestTo := -1, ""
	for k, to := range s.cfg.ModelMap {
		if !strings.HasSuffix(k, "*") {
			continue
		}
		p := strings.TrimSuffix(k, "*")
		if p != "" && strings.HasPrefix(lower, p) && len(p) > bestLen {
			bestLen, bestTo = len(p), to
		}
	}
	if bestLen >= 0 {
		return bestTo
	}
	return raw
}

// stripContextSuffix 去掉 Claude Code 的长上下文标记后缀。
//
// CC 里 `ANTHROPIC_MODEL=mimo-v2.5-pro[1m]` 表示"启用 1M 上下文"，
// 官方文档明确会在发送前自行 strip（[1m] 只作用于客户端选型与 beta 头），
// 所以正常情况下上游收到的是 `mimo-v2.5-pro`。但第三方客户端/代理可能原样透传，
// 这里做兜底，避免 `mimo-v2.5-pro[1m]` 在模型列表里匹配不到。
func stripContextSuffix(raw string) string {
	if !strings.HasSuffix(raw, "]") {
		return raw
	}
	i := strings.LastIndex(raw, "[")
	if i <= 0 {
		return raw
	}
	switch strings.ToLower(strings.TrimSpace(raw[i+1 : len(raw)-1])) {
	case "1m", "200k", "1000k":
		return strings.TrimSpace(raw[:i])
	default:
		return raw
	}
}

// resolveDefaultModel 使用桥配置的默认模型覆盖；未配置时才跟随上游默认。
// 订阅 opencode-go 套餐的用户可以把 default 固定到 go provider，避免误用
// opencode/fledge-alpha-free 这条默认线路。
func (s *Server) resolveDefaultModel(ctx context.Context, list []OCModel) (OCModelRef, error) {
	if configured := strings.TrimSpace(s.cfg.DefaultModel); configured != "" {
		ref, err := ResolveModel(configured, list)
		if err != nil {
			return OCModelRef{}, fmt.Errorf("configured default model %q: %w", configured, err)
		}
		return ref, nil
	}
	def, err := s.models.Default(ctx, s.up, s.cfg.Directory)
	if err != nil {
		return OCModelRef{}, fmt.Errorf("cannot resolve upstream default model: %w", err)
	}
	return OCModelRef{ProviderID: def.ProviderID, ID: def.ID}, nil
}

// isJanusAlias 判断客户端是否在用 janus 虚拟模型（web 里选的默认模型）。
// 用产品名做别名，避免和已有的 default（指向上游默认）撞名。
func isJanusAlias(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "janus")
}

// resolveJanusModel 解析 janus 虚拟模型：
// web 运行时选择 > BRIDGE_DEFAULT_MODEL > 上游默认。
func (s *Server) resolveJanusModel(ctx context.Context, list []OCModel) (OCModelRef, error) {
	runtime := strings.TrimSpace(s.runtimeDefaultModel())
	if runtime != "" {
		ref, err := ResolveModel(runtime, list)
		if err == nil {
			return ref, nil
		}
		// 启动瞬间模型列表可能为空/不完整：强制刷新一次再试，避免落到上游默认
		//（常是免费档模型，经 API 会 403 "free tier"）。
		if fresh, ferr := s.models.Get(ctx, s.up, s.cfg.Directory, true); ferr == nil {
			if ref2, err2 := ResolveModel(runtime, fresh); err2 == nil {
				return ref2, nil
			}
		}
		s.log.Warnf("janus default model %q unresolvable, falling back: %v", runtime, err)
	}
	return s.resolveDefaultModel(ctx, list)
}

// isDefaultAlias 判断客户端是否在请求"上游默认模型"。
// 空字符串也算：很多客户端允许留空模型，此时用默认最符合直觉。
func isDefaultAlias(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "default", "auto":
		return true
	default:
		return false
	}
}

// resetSession 丢弃当前上游 session，让 ensureSession 重建一个干净的。
// 删除放后台：客户端不该为会话回收等待。
func (s *Server) resetSession(conv *Conversation) {
	old := conv.snapshotSessionID()
	oldMCP := conv.mcpName
	conv.setSessionID("")
	conv.clearLast()
	conv.setResponse(nil)
	conv.model = OCModelRef{}
	conv.agent = ""
	// 重开会话意味着 agent 状态全丢，挂起的工具调用也必须作废，
	if conv.mcpName != "" {
		conv.mcpName = ""
		conv.toolsFP = ""
		conv.toolSess = nil
		conv.pendingTools = nil
		conv.toolsRegAt = time.Time{}
		s.tools.Unregister(conv.Key)
	}
	// 立刻把旧 MCP server 从上游删掉：OpenCode 会把同 location 的所有 MCP
	// server 暴露给每个 session，残留的旧 server 会被 agent 调用并得到
	// "stale (session was reset)"。以前只靠 janitor 兜底，中间有空窗。
	if oldMCP != "" {
		dir := conv.directory
		if dir == "" {
			dir = s.cfg.Directory
		}
		go func(name string) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.up.RemoveMCP(ctx, dir, name); err != nil {
				s.log.Debugf("remove stale mcp %s after reset: %v", name, err)
			} else {
				s.log.Infof("removed stale mcp server %s (session reset)", name)
			}
		}(oldMCP)
	}
	if old == "" {
		return
	}
	s.log.Infof("history reset, dropping session %s (key=%s)", old, conv.Key)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.up.DeleteSession(ctx, old); err != nil {
			s.log.Debugf("delete stale session %s: %v", old, err)
		}
	}()
}

// ensureSession 保证上游会话存在且 model/agent/目录一致。
func (s *Server) ensureSession(ctx context.Context, conv *Conversation,
	ref OCModelRef, agent, dir string, msgs []ChatMessage) (created bool, err error) {

	if conv.snapshotSessionID() == "" {
		loc := &struct {
			Directory string `json:"directory"`
		}{Directory: dir}
		in := CreateSessionReq{
			Title:    firstUserTitle(msgs, 40),
			Agent:    agent,
			Model:    &ref,
			Location: loc,
		}
		sess, err := s.up.CreateSession(ctx, in)
		if err != nil {
			return false, err
		}
		conv.setSessionID(sess.ID)
		s.markOwnedSession(sess.ID)
		conv.model = ref
		conv.agent = agent
		conv.directory = dir
		s.persistConv(conv)
		s.log.Infof("session created %s dir=%s model=%s", sess.ID, dir, ref.String())
		return true, nil
	}

	s.markOwnedSession(conv.snapshotSessionID())

	if conv.model != ref {
		if err := s.up.SetModel(ctx, conv.snapshotSessionID(), ref); err != nil {
			return false, fmt.Errorf("switch model: %w", err)
		}
		conv.model = ref
		s.log.Debugf("session %s model -> %s", conv.snapshotSessionID(), ref.String())
	}
	if conv.agent != "" && conv.agent != agent {
		if err := s.up.SetAgent(ctx, conv.snapshotSessionID(), agent); err != nil {
			return false, fmt.Errorf("switch agent: %w", err)
		}
		conv.agent = agent
	}
	s.persistConv(conv)
	return false, nil
}

// replyCached 处理 DiffNone：内容没变，直接复用上次结果。
// responseHasContent 判断缓存的回复是否值得回放。
// 空回复（正文与推理都为空）不算，需要重跑。
func responseHasContent(r *ChatResponse) bool {
	if r == nil || len(r.Choices) == 0 {
		return false
	}
	m := r.Choices[0].Message
	return strings.TrimSpace(m.Content) != "" || strings.TrimSpace(m.ReasoningContent) != ""
}

// lastUserTurn 取出历史里最后一次 user 发言（用于空回复时重跑）。
func lastUserTurn(msgs []ChatMessage) []ChatMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return []ChatMessage{msgs[i]}
		}
	}
	return nil
}

func (s *Server) replyCached(w http.ResponseWriter, r *http.Request,
	req ChatRequest, ref OCModelRef, conv *Conversation, cached *ChatResponse) {

	if cached != nil {
		if req.Stream {
			s.writeCachedStream(w, req, *cached)
			return
		}
		writeJSON(w, http.StatusOK, cached)
		return
	}

	// 没有可用缓存（例如重启过）：用历史最后一条 assistant 兜底
	text := lastAssistant(req.Messages)
	resp := buildResponse("chatcmpl-"+newID(), time.Now().Unix(),
		s.echoModel(ref, req.Model), runResult{text: text, finish: "stop"})
	if req.Stream {
		s.writeCachedStream(w, req, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) writeCachedStream(w http.ResponseWriter, req ChatRequest, resp ChatResponse) {
	sw, err := newSSE(w, resp.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", err.Error(), "")
		return
	}
	_ = sw.begin()
	_ = sw.deltaReasoning(resp.Choices[0].Message.ReasoningContent)
	_ = sw.deltaText(resp.Choices[0].Message.Content)
	_ = sw.finish(resp.Choices[0].FinishReason, resp.Usage,
		req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
}

// modelName 返回回显给客户端的 model 名。
// 刻意不带 variant：思考强度是通过独立的 reasoning_effort 参数表达的，
// 混进模型名会让客户端以为换了模型。
func modelName(ref OCModelRef, fallback string) string {
	if ref.ProviderID != "" && ref.ID != "" {
		return ref.ProviderID + "/" + ref.ID
	}
	return fallback
}

// echoModel 按 BRIDGE_MODEL_ECHO 决定响应 model 字段：
// request=回显客户端请求里的原始名（如 claude-sonnet-4-5），real（默认）=回显真实模型。
func (s *Server) echoModel(ref OCModelRef, requested string) string {
	if s.cfg.ModelEcho == "request" && strings.TrimSpace(requested) != "" {
		return strings.TrimSpace(requested)
	}
	return modelName(ref, requested)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func readBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("empty body")
	}
	defer r.Body.Close()
	return readAllLimit(r.Body, max)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
