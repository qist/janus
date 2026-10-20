// Package agent 定义 Agent 面（harness）的统一抽象。
//
// janus 的模型面（internal/gateway 与 api 里的透传）只换 model 字段后转发，
// 与 harness 无关；而「跑 agent、建会话、流式产出、工具调用」这套是 harness
// 特有的。AgentRuntime 把后者收敛成一组方法：
//
//	opencode → 桥到本机 OpenCode server（会话/流式/工具/权限都在那边）
//	direct   → 不建会话，直接走模型面透传（纯 LLM 客户端）
//
// 未来的 codex / claude-code adapter 只需实现本接口即可接入（见 docs/REFACTOR.md §3.5）。
// 工具桥（MCP）留在 janus 侧，因为 MCP 是通用协议，不绑定 harness。
package agent

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
)

// 内置 runtime 名。
const (
	NameOpenCode = "opencode"
	NameDirect   = "direct"
)

// AgentRuntime 是一个 harness 适配器。
//
// 方法语义：
//   - EnsureReady 确保 runtime 可用（进程在跑、端点可达）；幂等。
//   - CreateSession 建一个会话，返回 harness 侧的会话 id。
//   - Prompt 提交一轮输入，返回事件流；通道在终止事件后关闭。
//   - SetModel / Interrupt / Close 分别是改模型 / 打断 / 释放。
type AgentRuntime interface {
	Name() string
	EnsureReady(ctx context.Context) error
	CreateSession(ctx context.Context, spec SessionSpec) (string, error)
	Prompt(ctx context.Context, sid string, req PromptReq) (<-chan Event, error)
	SetModel(ctx context.Context, sid, model string) error
	Interrupt(ctx context.Context, sid string) error
	Close(ctx context.Context) error
}

// SessionSpec 是建会话所需的最小信息。
type SessionSpec struct {
	Key          string // 逻辑会话键（janus 侧分桶用，可作会话标题）
	Directory    string // 工作目录（harness 侧项目路径）
	Agent        string // harness 内的 agent 名（如 opencode 的 build/orchestrator）
	Model        string // 初始模型（"provider/model[:variant]"）
	ProviderID   string // 已解析的 provider（可选）
	CredentialID string // 已解析的凭据（可选）

	// 以下三项用于 CLI 型 harness（codex / claude-code）：它们是被 janus 起的
	// 子进程，需要被告知「怎么用 janus」（模型面走哪个地址 + 用哪个 key，
	// 工具面把哪个 MCP 端点注册进去）。opencode 型 runtime 不需要，留空即可。
	GatewayBaseURL string // janus 基址，CLI 拿它当模型 provider
	GatewayAPIKey  string // 给 CLI 用的 agent key
	MCPURL         string // janus 的 MCP 端点（工具桥；空=不注册工具）
	MCPName        string // 注册到 harness 的 MCP server 名，缺省 janus
}

// Message 是 Prompt 的历史消息（协议无关的精简形态）。
type Message struct {
	Role       string
	Content    string
	ToolCallID string
	Name       string
}

// Tool 是客户端声明的工具（协议无关）。
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// PromptReq 是一轮输入。
type PromptReq struct {
	Model    string
	Messages []Message
	Tools    []Tool
	Stream   bool
	// System 是系统提示（direct 等无会话 runtime 使用）。
	System string
}

// EventType 是事件类型。
type EventType string

const (
	EventText       EventType = "text"        // 正文增量
	EventReasoning  EventType = "reasoning"   // 思考增量
	EventToolCall   EventType = "tool_call"   // 工具调用（含入参）
	EventToolResult EventType = "tool_result" // 工具结果
	EventUsage      EventType = "usage"       // 用量（累计或分段）
	EventError      EventType = "error"       // 失败
	EventDone       EventType = "done"        // 终止（正常/中断均以它收尾）
)

// ToolCall 是一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Usage 是一次用量（各 runtime 自行决定含义：分段或累计）。
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	ReasoningTokens int64
	CostUSD         float64
}

// Event 是 Prompt 事件流里的一项。
type Event struct {
	Type     EventType
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
	Error    string
	Finish   string // done 时的收尾原因（stop/length/interrupted…）
}

// Registry 是 runtime 注册表：按名注册/查找，保留注册顺序供面板展示。
type Registry struct {
	mu     sync.RWMutex
	byName map[string]AgentRuntime
	order  []string
}

// NewRegistry 建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{byName: map[string]AgentRuntime{}}
}

// Register 注册（或覆盖）一个 runtime。
func (r *Registry) Register(rt AgentRuntime) {
	if r == nil || rt == nil {
		return
	}
	name := rt.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; !exists {
		r.order = append(r.order, name)
	}
	r.byName[name] = rt
}

// Get 按名取 runtime。
func (r *Registry) Get(name string) (AgentRuntime, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.byName[name]
	return rt, ok
}

// Names 返回已注册的 runtime 名（注册顺序）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// List 返回已注册的 runtime（注册顺序）。
func (r *Registry) List() []AgentRuntime {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentRuntime, 0, len(r.order))
	for _, n := range r.order {
		if rt, ok := r.byName[n]; ok {
			out = append(out, rt)
		}
	}
	return out
}

// SortNames 返回排序后的名字（仅测试/展示用，稳定输出）。
func (r *Registry) SortNames() []string {
	names := r.Names()
	sort.Strings(names)
	return names
}
