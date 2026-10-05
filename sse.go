package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------- SSE 写出 ----------

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher

	id      string
	created int64
	model   string

	mu      sync.Mutex
	started bool
	closed  bool
}

func newSSE(w http.ResponseWriter, model string) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support flushing")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	return &sseWriter{
		w:       w,
		flusher: flusher,
		id:      "chatcmpl-" + newID(),
		created: time.Now().Unix(),
		model:   model,
	}, nil
}

func (s *sseWriter) writeChunk(c ChatChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("data: ")); err != nil {
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

// begin 发送首个 role chunk。
func (s *sseWriter) begin() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	empty := ""
	return s.writeChunk(ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{
			Index: 0,
			Delta: Delta{Role: "assistant", Content: &empty},
		}},
	})
}

func (s *sseWriter) deltaText(text string) error {
	if text == "" {
		return nil
	}
	return s.writeChunk(ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{
			Index: 0,
			Delta: Delta{Content: &text},
		}},
	})
}

func (s *sseWriter) deltaReasoning(text string) error {
	if text == "" {
		return nil
	}
	return s.writeChunk(ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{
			Index: 0,
			Delta: Delta{ReasoningContent: &text},
		}},
	})
}

// keepAlive 发一个空的 delta 数据块做保活。
// 不用 SSE 注释（`: ping`）：部分客户端（如 CodeBuddy）只在收到 `data:` 时
// 才重置读超时，注释不算，于是空闲 >30s 就报「模型超过 30 秒未返回数据」。
// OpenAI 兼容客户端对空 delta 块是标准处理，不会产生额外内容。
func (s *sseWriter) keepAlive() error {
	return s.writeChunk(ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{}}},
	})
}

func (s *sseWriter) finish(reason string, u *Usage, _ bool) error {
	ch := ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{
			Index:        0,
			Delta:        Delta{},
			FinishReason: &reason,
		}},
	}
	if err := s.writeChunk(ch); err != nil {
		return err
	}

	// 始终带上 usage（不依赖客户端的 stream_options.include_usage）：
	// 不少客户端（如 CodeBuddy）不显式请求它，但仍要靠 usage 统计输出与缓存命中。
	// 单独一个 choices 为空的 chunk 是 OpenAI 的标准写法，标准客户端会正确解析。
	if u != nil {
		usageOnly := ChatChunk{
			ID:      s.id,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []ChunkChoice{},
			Usage:   u,
		}
		if err := s.writeChunk(usageOnly); err != nil {
			return err
		}
	}
	return s.done()
}

// markClosed 让后续所有写操作变成 no-op。
//
// 必须由 handler 在返回前调用（defer）。否则客户端断开、handler 先返回后，
// runEvents 协程可能仍在写 http.ResponseWriter —— 而 net/http 正在收尾同一个
// 连接，构成 use-after-return 数据竞争（-race 实测抓到过）。
// 与 writeChunk 共用 s.mu，所以 markClosed 返回后再不可能有写入在飞。
func (s *sseWriter) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// ping 写一行 SSE 注释做心跳。长跑工具时可能几分钟没有增量，
// 没有心跳会被中间代理/客户端读超时掐断。
func (s *sseWriter) ping() {
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

func (s *sseWriter) done() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if _, err := s.w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// ---------- 工具调用 ----------

// toolCalls 按 OpenAI 流式格式发出 tool_calls delta。
//
// 每个调用一个 chunk：带 id/type/name/arguments，index 对应并发序号。
// 之后由 finish 发 finish_reason="tool_calls"。
func (s *sseWriter) toolCalls(pending []*pendingCall) error {
	for i, p := range pending {
		idx := i
		tc := ToolCall{
			ID:       p.CallID,
			Type:     "function",
			Function: &FunctionCall{Name: p.ToolName, Arguments: p.Args},
			Index:    &idx,
		}
		if err := s.writeChunk(ChatChunk{
			ID:      s.id,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []ChunkChoice{{
				Index: 0,
				Delta: Delta{ToolCalls: []ToolCall{tc}},
			}},
		}); err != nil {
			return err
		}
	}
	return nil
}

// buildToolCallResponse 组装非流式的 tool_calls 响应。
func buildToolCallResponse(id, model string, pending []*pendingCall) ChatResponse {
	calls := make([]ToolCall, 0, len(pending))
	for _, p := range pending {
		calls = append(calls, ToolCall{
			ID:       p.CallID,
			Type:     "function",
			Function: &FunctionCall{Name: p.ToolName, Arguments: p.Args},
		})
	}
	return ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      AssistantMsg{Role: "assistant", Content: "", ToolCalls: calls},
			FinishReason: "tool_calls",
		}},
		Usage: &Usage{},
	}
}

// ---------- 错误输出 ----------

func writeOpenAIError(w http.ResponseWriter, status int, typ, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIErrorBody{Error: openAIError{
		Message: msg,
		Type:    typ,
		Param:   nil,
		Code:    code,
	}})
}

// isSessionGone 判断上游错误是否是"会话不存在"（OpenCode 重启/清理后常见）。
// 命中时应清掉本地对该 session 的引用，下次请求重建并重放完整历史。
func isSessionGone(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound &&
		strings.Contains(strings.ToLower(ae.Msg), "session")
}

// mapUpstreamError 把 OpenCode 的错误翻译成 OpenAI 语义。
//
// 是 Server 的方法，因为连接错误信息里要带上"当前实际在用的"上游地址 ——
// auto 发现模式下 OPENCODE_URL 环境变量是空的/过时的。
func (srv *Server) mapUpstreamError(err error) (status int, typ, msg string) {
	if err == nil {
		return http.StatusInternalServerError, "api_error", "internal error"
	}

	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusForbidden &&
			strings.Contains(strings.ToLower(ae.Msg), "free tier"):
			// 免费档仅限官方客户端：透传原因 + 可操作提示
			return http.StatusForbidden, "api_error",
				ae.Msg + "；该免费模型仅限 OpenCode 官方客户端使用，请改用 opencode-go/* 订阅模型，或在 OpenCode 里配置自己的 provider"
		case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
			return http.StatusBadGateway, "api_error", "opencode server authentication failed"
		case ae.Status == http.StatusNotFound:
			return http.StatusBadGateway, "api_error", "opencode resource not found: " + ae.Msg
		case ae.Status == http.StatusConflict:
			return http.StatusConflict, "invalid_request_error", "opencode session busy: " + ae.Msg
		case ae.Status == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limit_error", ae.Msg
		case ae.Status >= 500:
			return http.StatusBadGateway, "api_error", "opencode server error: " + ae.Msg
		default:
			return http.StatusBadGateway, "api_error", ae.Msg
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return 499, "api_error", "client disconnected"
	}
	if isConnErr(err) {
		return http.StatusServiceUnavailable, "api_error",
			"cannot reach opencode server at " + srv.up.endpoint()
	}
	return http.StatusBadGateway, "api_error", err.Error()
}

func isConnErr(err error) bool {
	s := err.Error()
	for _, k := range []string{
		"connection refused", "no such host", "i/o timeout",
		"connection reset by peer", "dial tcp",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// ---------- 小工具 ----------

// newID 生成响应 ID 的随机部分：128 位密码学随机，32 个十六进制字符。
//
// 刻意不用时间/进程/自增拼出来的伪随机：这个值会随响应返回给客户端，可能被
// 当作幂等键、日志关联键甚至缓存键。可预测或在高并发/多进程下有碰撞风险都不合适。
// 16 字节 crypto/rand 的碰撞概率可以忽略（生日界 2^64 量级）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Linux 上基本不会失败。真失败也不能让请求挂掉，
		// 退化成"时间 + 进程 + 递增"，保证唯一性，牺牲不可预测性。
		return fallbackID()
	}
	return hex.EncodeToString(b[:])
}

var (
	fallbackMu  sync.Mutex
	fallbackSeq uint64
)

func fallbackID() string {
	fallbackMu.Lock()
	fallbackSeq++
	n := fallbackSeq
	fallbackMu.Unlock()
	return fmt.Sprintf("%016x%016x", uint64(time.Now().UnixNano()),
		uint64(os.Getpid())<<32|(n&0xffffffff))
}

// errorEvent 在 SSE 流里发一个 OpenAI 风格的 error 对象并收尾。
//
// 用于"流已经开始、但这一轮注定没有内容"的情况（例如上游返回空回复）。
// 此时 HTTP 状态码已经写出去了（200），只能靠流内 error 事件让客户端知道失败，
// 而不是收到一个内容为空的"成功"响应。
func (s *sseWriter) errorEvent(typ, msg, code string) error {
	if typ == "" {
		typ = "api_error"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	b, err := json.Marshal(openAIErrorBody{Error: openAIError{
		Message: msg,
		Type:    typ,
		Param:   nil,
		Code:    code,
	}})
	if err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(b); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
