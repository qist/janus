package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// toolbridge.go —— 把 OpenAI 客户端声明的 tools 暴露给 OpenCode agent。
//
// 整体链路：
//
//	客户端声明 tools[]
//	  → 桥为该会话注册一个 MCP server，tools/list 返回这些工具
//	  → OpenCode agent 决定调用 → MCP tools/call 打到桥
//	  → 桥「挂起」这个调用，并把 tool_calls 作为 finish_reason=tool_calls 回给客户端
//	  → 客户端执行完，在后续请求里带 role:"tool" 结果
//	  → 桥把结果回填给挂起的 MCP 调用，agent 继续跑
//
// 挂起是必须的：MCP 的 tools/call 是一次同步 JSON-RPC，我们必须等客户端把
// 真实结果送回来才能应答。中间这段时间上游 agent 就停在这次工具调用上。

const (
	// mcpNamePrefix 是注册到 OpenCode 的 MCP server 名前缀，便于识别/清理。
	mcpNamePrefix = "ob-"

	// mcpToolPrefix 给暴露给 agent 的工具名加会话命名空间。
	// 多个会话同时注册 MCP server 时，工具名不会互相覆盖；
	// 桥在转成 OpenAI tool_calls 时会还原成客户端原本的名字。
	mcpToolPrefix = "ob_"

	// defaultToolWait 是工具调用挂起的最长时间（等客户端回填结果）。
	defaultToolWait = 5 * time.Minute

	// toolCallIDPrefix OpenAI 风格的 tool_call id 前缀。
	toolCallIDPrefix = "call_"
)

// ToolResult 是客户端回填的工具执行结果。
type ToolResult struct {
	Content  string
	IsError  bool
	TimedOut bool // 客户端在 ToolCallWait 内没回结果（通常意味着客户端已离开）
}

// serverToolFunc 是"由桥自己执行"的工具（如 Claude Code 的 web_search 服务端工具）。
// 返回 (文本结果, isError)。
type serverToolFunc func(ctx context.Context, args string) (string, bool)

// pendingCall 是 agent 发起、等待客户端执行的一个工具调用。
type pendingCall struct {
	CallID   string // OpenAI tool_call id，回给客户端
	ToolName string // 客户端原本的工具名（已去掉命名空间）
	MCPName  string // 暴露给 agent 的名字（带命名空间）
	Args     string // JSON 字符串
	ConvKey  string // 所属会话 key（日志定位用）

	ord    int64 // 到达顺序（parallel=false 时按它逐个返回）
	result chan ToolResult
	once   sync.Once
}

func (p *pendingCall) complete(r ToolResult) {
	p.once.Do(func() { p.result <- r })
}

// toolSession 是一次会话的工具上下文。
type toolSession struct {
	token   string
	key     string
	mcpName string // 注册到 OpenCode 的 MCP server 名
	tools   []ToolSpec

	mu      sync.Mutex
	pending map[string]*pendingCall // MCPName -> pending
	ordSeq  int64                   // 到达序号
	waiters []chan struct{}         // 有新 pending 时通知执行器
	closed  bool

	// sessionID 是当前会话对应的上游 session id（会话重建会更新）。
	// 用于「客户端走了」时中断上游，避免孤儿 agent 一直调工具。
	sessionID string

	// idle 拒绝日志限流（防刷屏）
	lastRejectLog atomic.Int64 // 上次打印时间（毫秒）
	rejectCount   atomic.Int64 // 距上次日志以来被抑制的拒绝次数

	// serverTools 由桥自己执行的工具（如 Claude Code 的 web_search），不甩给客户端。
	serverTools map[string]serverToolFunc
}

// ToolBridge 管理所有会话的 MCP 工具上下文。
type ToolBridge struct {
	mu        sync.RWMutex
	byToken   map[string]*toolSession
	byKey     map[string]*toolSession
	byName    map[string]*toolSession
	wait      time.Duration // 长等待（执行类工具，或未列出的工具）
	waitFast  time.Duration // 短等待（只读/编辑类工具）；0=不启用
	fastTools map[string]bool
	log       *Logger
}

func NewToolBridge(log *Logger, wait, waitFast time.Duration, fastTools map[string]bool) *ToolBridge {
	if wait <= 0 {
		wait = defaultToolWait
	}
	if waitFast <= 0 || waitFast >= wait {
		waitFast = 0
	}
	return &ToolBridge{
		byToken:   map[string]*toolSession{},
		byKey:     map[string]*toolSession{},
		byName:    map[string]*toolSession{},
		wait:      wait,
		waitFast:  waitFast,
		fastTools: fastTools,
		log:       log,
	}
}

// waitFor 按工具类型选等待时长：已知的只读/编辑类走短等待，其余（含未知）走长等待。
func (b *ToolBridge) waitFor(p *pendingCall) time.Duration {
	if b.waitFast > 0 && b.fastTools[strings.ToLower(p.ToolName)] {
		return b.waitFast
	}
	return b.wait
}

// mcpServerName 把会话 key 变成合法的 MCP server 名。
func mcpServerName(key string) string {
	var b strings.Builder
	b.WriteString(mcpNamePrefix)
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// mcpRegName 拼出注册到 OpenCode 的实际 server 名：基名 + 工具集指纹。
//
// 为什么带指纹：OpenCode 对同名 MCP server 重新 PUT 时不会重新拉 tools/list，
// 模型会一直看到旧工具。统一用"指纹名"后，工具集变化就是新 server，必然重列。
func mcpRegName(base, fingerprint string) string {
	fp := fingerprint
	if len(fp) > 8 {
		fp = fp[:8]
	}
	if fp == "" {
		return base
	}
	return base + "-" + fp
}

// Register 为会话登记（或复用）工具上下文。工具集变化时原地更新。
func (b *ToolBridge) Register(key string, tools []ToolSpec) *toolSession {
	b.mu.Lock()
	defer b.mu.Unlock()

	if s, ok := b.byKey[key]; ok {
		s.setTools(tools)
		return s
	}
	s := &toolSession{
		token:   randomToken(),
		key:     key,
		mcpName: mcpServerName(key),
		tools:   normalizeTools(tools),
		pending: map[string]*pendingCall{},
	}
	b.byToken[s.token] = s
	b.byKey[key] = s
	// 注意：不要把基名塞进 byName —— 实际注册用的是带指纹的名字（Alias 登记）。
	// 否则 OpenCode 里残留的旧基名会被误判为"活跃"，janitor 永远清不掉。
	return s
}

// Get 按 token 找会话（MCP 请求用）。
func (b *ToolBridge) Get(token string) *toolSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.byToken[token]
}

// ByKey 按会话 key 找上下文。
func (b *ToolBridge) ByKey(key string) *toolSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.byKey[key]
}

// Alias 为会话登记一个"实际注册到 OpenCode 的名字"。
//
// 工具集变化时会用带指纹的新名字重新注册（见 toolflow.ensureTools），
// 这里让 janitor 的 byName 对账认得新名字，避免被当成残留删掉。
func (b *ToolBridge) Alias(s *toolSession, name string) {
	if s == nil || name == "" {
		return
	}
	b.mu.Lock()
	b.byName[name] = s
	b.mu.Unlock()
}

// RegisteredNames 返回当前所有已注册的 MCP server 名（给 janitor 对账用）。
func (b *ToolBridge) RegisteredNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.byName))
	for n := range b.byName {
		out = append(out, n)
	}
	return out
}

// Unregister 移除会话的工具上下文，并唤醒所有挂起的调用（以错误收尾）。
func (b *ToolBridge) Unregister(key string) {
	b.mu.Lock()
	s := b.byKey[key]
	if s != nil {
		delete(b.byKey, key)
		delete(b.byToken, s.token)
		// 会话可能登记过多个名字（工具集变化换指纹名），全部清掉，
		// 这样 janitor 会把它们当残留回收。
		for n, x := range b.byName {
			if x == s {
				delete(b.byName, n)
			}
		}
	}
	b.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// ---------- toolSession ----------

func (s *toolSession) setTools(tools []ToolSpec) {
	s.mu.Lock()
	s.tools = normalizeTools(tools)
	s.mu.Unlock()
}

func (s *toolSession) snapshotTools() []ToolSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ToolSpec, len(s.tools))
	copy(out, s.tools)
	return out
}

// mcpToolName 返回暴露给 agent 的工具名（带会话命名空间）。
func (s *toolSession) mcpToolName(original string) string {
	return mcpToolPrefix + s.key + "_" + original
}

// originalName 从暴露名还原成客户端原本的工具名。
func (s *toolSession) originalName(mcp string) (string, bool) {
	prefix := mcpToolPrefix + s.key + "_"
	if !strings.HasPrefix(mcp, prefix) {
		return "", false
	}
	return strings.TrimPrefix(mcp, prefix), true
}

// park 登记一个挂起的工具调用，返回 pendingCall。
func (s *toolSession) park(callID, original, mcpName, args string) *pendingCall {
	p := &pendingCall{
		CallID:   callID,
		ToolName: original,
		MCPName:  mcpName,
		Args:     args,
		ConvKey:  s.key,
		result:   make(chan ToolResult, 1),
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		p.complete(ToolResult{Content: "bridge: session closed", IsError: true})
		return p
	}
	s.ordSeq++
	p.ord = s.ordSeq
	s.pending[mcpName] = p
	waiters := s.waiters
	s.mu.Unlock()

	// 唤醒等待"有新工具调用"的执行器
	for _, w := range waiters {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	return p
}

// watch 返回一个通道，有新挂起调用时会被通知一次。
func (s *toolSession) watch() chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if len(s.pending) > 0 {
		ch <- struct{}{}
	}
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()
	return ch
}

func (s *toolSession) unwatch(ch chan struct{}) {
	s.mu.Lock()
	for i, w := range s.waiters {
		if w == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

// takePending 取出并清空当前所有挂起调用（按调用顺序）。
func (s *toolSession) takePending() []*pendingCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := make([]*pendingCall, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p)
	}
	s.pending = map[string]*pendingCall{}
	sort.Slice(out, func(i, j int) bool { return out[i].ord < out[j].ord })
	return out
}

// takeOldest 取出并移除最早挂起的那个调用（parallel=false 时逐个返回）。
// 没有则返回 nil。
func (s *toolSession) takeOldest() *pendingCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldest *pendingCall
	for _, p := range s.pending {
		if oldest == nil || p.ord < oldest.ord {
			oldest = p
		}
	}
	if oldest != nil {
		delete(s.pending, oldest.MCPName)
	}
	return oldest
}

func (s *toolSession) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// setServerTools 登记由桥自己执行的工具（不甩给客户端）。
func (s *toolSession) setServerTools(m map[string]serverToolFunc) {
	s.mu.Lock()
	if s.serverTools == nil {
		s.serverTools = map[string]serverToolFunc{}
	}
	for k, v := range m {
		s.serverTools[k] = v
	}
	s.mu.Unlock()
}

// serverTool 返回某个"桥自己执行"的工具；不存在返回 nil。
func (s *toolSession) serverTool(name string) serverToolFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serverTools[name]
}

// hasWaiter 报告是否有执行器正在等待新的挂起调用（= 该会话当前有在飞请求）。
func (s *toolSession) hasWaiter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters) > 0
}

// setSessionID 记录当前会话对应的上游 session id（会话重建时更新）。
func (s *toolSession) setSessionID(id string) {
	s.mu.Lock()
	s.sessionID = id
	s.mu.Unlock()
}

// getSessionID 返回当前会话对应的上游 session id。
func (s *toolSession) getSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// shouldLogReject 给"idle 会话被拒"的日志做限流：每个会话最多每 interval 打一条。
// 返回 (是否该打印, 自上次打印以来被抑制的条数)。
func (s *toolSession) shouldLogReject(interval time.Duration) (bool, int64) {
	now := time.Now().UnixMilli()
	last := s.lastRejectLog.Load()
	if last != 0 && now-last < interval.Milliseconds() {
		s.rejectCount.Add(1)
		return false, 0
	}
	s.lastRejectLog.Store(now)
	return true, s.rejectCount.Swap(0)
}

// waitForWaiter 最多等 d，等执行器注册 watch。返回期间是否出现过等待者。
//
// 用途：OpenCode 会把同一 location 下所有 MCP server 暴露给每个 session，
// 模型可能调到**别的会话**的 server。那个会话没有在飞请求，正常会一直挂到
// ToolCallWait（5 分钟）才报错；这里给"请求刚发出、watch 还没注册"留一点余量，
// 超时就直接拒绝，避免挂死。
func (s *toolSession) waitForWaiter(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if s.hasWaiter() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// close 关闭会话并让所有挂起调用失败返回。
func (s *toolSession) close() {
	s.mu.Lock()
	s.closed = true
	pend := make([]*pendingCall, 0, len(s.pending))
	for _, p := range s.pending {
		pend = append(pend, p)
	}
	s.pending = map[string]*pendingCall{}
	s.mu.Unlock()
	for _, p := range pend {
		p.complete(ToolResult{
			Content: "bridge: conversation ended before the client returned a tool result",
			IsError: true,
		})
	}
}

// ---------- 工具定义 ----------

// normalizeTools 去重并按名字排序，保证多次请求的工具集稳定可比。
func normalizeTools(tools []ToolSpec) []ToolSpec {
	seen := map[string]bool{}
	out := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		name := t.Function.Name
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Function.Name < out[j].Function.Name })
	return out
}

// toolsFingerprint 用于判断工具集是否变化（变化才需要重新注册/刷新 MCP）。
func toolsFingerprint(tools []ToolSpec) string {
	norm := normalizeTools(tools)
	b, _ := json.Marshal(norm)
	return fmt.Sprintf("%x", sum32(b))
}

func sum32(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// waitResult 等待客户端回填结果（或超时/上下文取消）。
func (b *ToolBridge) waitResult(ctx context.Context, p *pendingCall) ToolResult {
	wait := b.waitFor(p)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case r := <-p.result:
		return r
	case <-ctx.Done():
		return ToolResult{Content: "bridge: tool call aborted: " + ctx.Err().Error(), IsError: true}
	case <-t.C:
		if b.log != nil {
			b.log.Warnf("tool call timed out: client did not return a result within %s (tool=%s conv=%s); releasing agent",
				wait, p.ToolName, p.ConvKey)
		}
		return ToolResult{
			Content:  fmt.Sprintf("bridge: client did not return a tool result within %s", wait),
			IsError:  true,
			TimedOut: true,
		}
	}
}
