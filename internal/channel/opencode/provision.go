package opencode

import (
	"encoding/json"
	"strings"

	"janus/internal/agent"
)

// provision.go —— OpenCode 的 Harness 实现：把 janus 接进 OpenCode。
//
// 两种落地方式共用同一份内容：
//   - 自动：janus 拉起 OpenCode 时经 OPENCODE_CONFIG_CONTENT 内联注入（见 InlineConfig）；
//   - 手动：把 Files 里的片段合并进 ~/.config/opencode/opencode.jsonc。
//
// 注意：OpenCode 的 MCP server 是 janus 运行期通过 API 动态注册的（见 api/toolflow.go），
// 不写进配置文件，所以这里 ToolModeBridge 只影响 orchestrator agent 是否暴露。

// Name 是 harness 名（对应 agents.kind）。
const Name = "opencode"

// defaultToolPrefix 是工具桥注册到 OpenCode 的 MCP server 名前缀（仅兜底）。
const defaultToolPrefix = "ob-"

// Harness 是 OpenCode 的 Provision 实现。toolPrefix 与工具桥注册名一致。
type Harness struct {
	toolPrefix string
}

// NewHarness 建一个 OpenCode provisioner。
func NewHarness(toolPrefix string) *Harness {
	if strings.TrimSpace(toolPrefix) == "" {
		toolPrefix = defaultToolPrefix
	}
	return &Harness{toolPrefix: toolPrefix}
}

// Name 实现 agent.Harness。
func (h *Harness) Name() string { return Name }

// Provision 生成 OpenCode 侧的接入配置。
func (h *Harness) Provision(in agent.ProvisionInput) (agent.ProvisionResult, error) {
	inline := map[string]any{}
	if in.Mode == agent.ToolModeBridge {
		inline["agents"] = map[string]any{"orchestrator": OrchestratorAgent(h.toolPrefix)}
	}
	if prov := ProviderConfig(in.BaseURL, in.APIKey, in.Models); prov != nil {
		inline["provider"] = map[string]any{"janus": prov}
	}

	raw, err := json.Marshal(inline)
	if err != nil {
		return agent.ProvisionResult{}, err
	}
	pretty, err := json.MarshalIndent(inline, "", "  ")
	if err != nil {
		return agent.ProvisionResult{}, err
	}

	res := agent.ProvisionResult{
		Env: map[string]string{"OPENCODE_CONFIG_CONTENT": string(raw)},
		Files: []agent.ProvisionFile{{
			Path:    "~/.config/opencode/opencode.jsonc",
			Format:  "json",
			Content: string(pretty),
			Merge:   true,
		}},
		Notes: []string{
			"OpenCode 的 MCP server 由 janus 运行期动态注册，无需写入配置。",
			"若用 janus 拉起的 OpenCode，同一份内容会经 OPENCODE_CONFIG_CONTENT 自动注入。",
		},
	}
	return res, nil
}
