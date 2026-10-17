package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"janus/internal/protocol/shared"
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

	// 楔死取证（BRIDGE_WEDGE_FORENSICS）：开启时把写出的每个 data 块原样留底，
	// tool_calls 收尾的轮次由 handler 落盘（见 forensics.go），其余轮次丢弃。
	capOn  bool
	capBuf []byte
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
	if s.capOn {
		s.capBuf = append(s.capBuf, "data: "...)
		s.capBuf = append(s.capBuf, b...)
		s.capBuf = append(s.capBuf, '\n', '\n')
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

// captureBytes 返回已捕获的响应字节快照（未开启取证时为 nil）。
func (s *sseWriter) captureBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capBuf) == 0 {
		return nil
	}
	out := make([]byte, len(s.capBuf))
	copy(out, s.capBuf)
	return out
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
	if s.capOn {
		s.capBuf = append(s.capBuf, "data: [DONE]\n\n"...)
	}
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
	shared.WriteError(w, status, typ, msg, code)
}

// isSessionGone 判断上游错误是否是"会话不存在"（OpenCode 重启/清理后常见）。
// 命中时应清掉本地对该 session 的引用，下次请求重建并重放完整历史。
func isSessionGone(err error) bool { return shared.IsSessionGone(err) }

// mapUpstreamError 把 OpenCode 的错误翻译成 OpenAI 语义。
//
// 是 Server 的方法，因为连接错误信息里要带上"当前实际在用的"上游地址 ——
// auto 发现模式下 OPENCODE_URL 环境变量是空的/过时的。
func (srv *Server) mapUpstreamError(err error) (status int, typ, msg string) {
	return shared.MapUpstreamError(err, srv.up.Base())
}

// ---------- 小工具 ----------

// newID 生成响应 ID 的随机部分（128 位密码学随机，32 个十六进制字符）。
func newID() string { return shared.NewID() }

// fallbackID 是 crypto/rand 失败时的退化路径（时间 + 进程 + 递增）。
func fallbackID() string { return shared.FallbackID() }

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
