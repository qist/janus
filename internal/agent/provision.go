package agent

import (
	"sort"
	"strings"
	"sync"
)

// provision.go —— 「把 janus 接进某个 harness」的统一描述。
//
// 与 AgentRuntime 的分工：
//   - AgentRuntime 回答「怎么驱动一个已在跑的 harness 跑会话」（会话型链路）。
//   - Harness       回答「怎么让一个 harness 用上 janus」（配置/注册，含 MCP）。
//
// 一个 harness（codex / claude-code / opencode / …）通常既实现 Harness
// （必做：告诉用户/进程怎么接），也可能实现 AgentRuntime（可选：被 janus 驱动）。
// 只有 Harness 是厂商无关、纯函数、可测的，所以先做它——不再把
// 「怎么写配置、怎么注册 MCP」硬编码在某个 handler 里。

// ToolMode 对应旧 README「工具执行模式」的 A/B/C 三选一。
type ToolMode string

const (
	// ToolModeNative：harness 用自带的内置工具，在 janus 主机执行（旧 A，agent=build）。
	ToolModeNative ToolMode = "native"
	// ToolModeBridge：janus 把客户端声明的 tools 经 MCP 暴露给 harness，工具在客户端执行（旧 B）。
	ToolModeBridge ToolMode = "bridge"
	// ToolModeNone：harness 手里没有工具，只做纯文本问答（旧 C，等价 direct 的形状）。
	ToolModeNone ToolMode = "none"
)

// ParseToolMode 容错解析工具模式；空值/未知值一律回落 bridge（与旧默认一致）。
func ParseToolMode(s string) ToolMode {
	switch ToolMode(strings.ToLower(strings.TrimSpace(s))) {
	case ToolModeNative:
		return ToolModeNative
	case ToolModeNone:
		return ToolModeNone
	default:
		return ToolModeBridge
	}
}

// ProvisionInput 是把 janus 接进某个 harness 所需的全部输入。
// 纯数据、无副作用，便于翻译成任意 harness 的配置形态。
type ProvisionInput struct {
	Harness   string   // agents.id（仅作展示/日志）
	BaseURL   string   // janus 对外基址（不含 /v1）
	APIKey    string   // agent key（展示用占位或真实 key；写进 env，不落文件）
	MCPURL    string   // janus 的 MCP 端点；ToolModeBridge 时非空
	MCPName   string   // 注册到 harness 的 MCP server 名，缺省 janus
	Models    []string // 希望 harness 看到的模型名（可选）
	Mode      ToolMode // 工具执行模式
	Directory string   // 工作目录/项目路径（可选）
}

// MCPNameOr 返回 MCP server 名（缺省 janus，并做键名安全化）。
func (in ProvisionInput) MCPNameOr() string {
	n := strings.TrimSpace(in.MCPName)
	if n == "" {
		n = "janus"
	}
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		}
	}
	if b.Len() == 0 {
		return "janus"
	}
	return b.String()
}

// WantsMCP 表示该模式下是否要把 janus 的 MCP server 注册进 harness。
func (in ProvisionInput) WantsMCP() bool {
	return in.Mode == ToolModeBridge && strings.TrimSpace(in.MCPURL) != ""
}

// ProvisionFile 是一份需要写入（或合并进）harness 的配置文件。
type ProvisionFile struct {
	Path    string `json:"path"`            // 目标路径（可含 ~）
	Format  string `json:"format"`          // toml | json
	Content string `json:"content"`         // 完整片段内容
	Merge   bool   `json:"merge,omitempty"` // true=合并进已有文件（保留其余键）
}

// ProvisionResult 描述「怎么把 janus 接进该 harness」。由调用方决定是落盘、
// 设 env，还是仅展示给用户。
type ProvisionResult struct {
	Env     map[string]string `json:"env,omitempty"`
	Files   []ProvisionFile   `json:"files,omitempty"`
	Command []string          `json:"command,omitempty"` // 建议的启动命令
	Notes   []string          `json:"notes,omitempty"`   // 需要人工注意的点
}

// Harness 是一个可被 janus 配置的 agent 运行时。
//
// 实现放在各自的 channel 包里（internal/channel/<harness>），因为它们依赖
// 该 harness 的配置形态；agent 包只保留协议无关的类型。
type Harness interface {
	Name() string
	Provision(in ProvisionInput) (ProvisionResult, error)
}

// HarnessRegistry 是 Harness 注册表：按名注册/查找，保留注册顺序供展示。
type HarnessRegistry struct {
	mu     sync.RWMutex
	byName map[string]Harness
	order  []string
}

// NewHarnessRegistry 建一个空注册表。
func NewHarnessRegistry() *HarnessRegistry {
	return &HarnessRegistry{byName: map[string]Harness{}}
}

// Register 注册（或覆盖）一个 harness。
func (r *HarnessRegistry) Register(h Harness) {
	if r == nil || h == nil {
		return
	}
	name := h.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; !exists {
		r.order = append(r.order, name)
	}
	r.byName[name] = h
}

// Get 按名取 harness。
func (r *HarnessRegistry) Get(name string) (Harness, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.byName[strings.TrimSpace(name)]
	return h, ok
}

// Names 返回已注册的 harness 名（注册顺序）。
func (r *HarnessRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// SortNames 返回排序后的名字（稳定输出，测试/展示用）。
func (r *HarnessRegistry) SortNames() []string {
	names := r.Names()
	sort.Strings(names)
	return names
}
