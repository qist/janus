package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"

	"janus/internal/agent"
)

// provision.go —— Claude Code 的 Harness 实现：把 janus 接进 Claude Code。
//
// 只生成「怎么接」的描述（env + .mcp.json 片段 + 建议命令），不落盘。
//
// 依据的 Claude Code 形态：
//   - 模型：经 ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN 指向 janus 的 Anthropic 端点；
//   - MCP：项目级 .mcp.json 的 mcpServers（远程 HTTP 必须显式 "type":"http"，
//     否则会被当成 stdio server）。

// Name 是 harness 名（对应 agents.kind）。
const Name = "claude-code"

// Harness 是 Claude Code 的 Provision 实现。
type Harness struct{}

// Name 实现 agent.Harness。
func (Harness) Name() string { return Name }

// Provision 生成 Claude Code 侧的接入配置。
func (Harness) Provision(in agent.ProvisionInput) (agent.ProvisionResult, error) {
	base := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if base == "" {
		return agent.ProvisionResult{}, fmt.Errorf("claude-code: base URL is required")
	}
	// Claude Code 自己拼 /v1/messages，所以 base 到 /anthropic 为止。
	anthropicBase := base
	if !strings.HasSuffix(anthropicBase, "/anthropic") {
		anthropicBase += "/anthropic"
	}

	res := agent.ProvisionResult{
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":   anthropicBase,
			"ANTHROPIC_AUTH_TOKEN": in.APIKey,
		},
		Command: []string{"claude", "--print", "--output-format", "stream-json"},
		Notes: []string{
			"Claude Code 走 Anthropic Messages，janus 的 /anthropic/v1/messages 已支持。",
			"远程 HTTP MCP server 必须显式写 \"type\":\"http\"，否则会被当成 stdio 命令。",
		},
	}

	if in.WantsMCP() {
		cfg := map[string]any{
			"mcpServers": map[string]any{
				in.MCPNameOr(): map[string]any{
					"type": "http",
					"url":  strings.TrimSpace(in.MCPURL),
				},
			},
		}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return agent.ProvisionResult{}, err
		}
		res.Files = []agent.ProvisionFile{{
			Path:    ".mcp.json",
			Format:  "json",
			Content: string(b),
			Merge:   true,
		}}
	}
	return res, nil
}
