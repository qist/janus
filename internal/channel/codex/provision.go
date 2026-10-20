package codex

import (
	"fmt"
	"strings"

	"janus/internal/agent"
)

// provision.go —— Codex CLI 的 Harness 实现：把 janus 接进 Codex。
//
// 只生成「怎么接」的描述（env + config.toml 片段 + 建议命令），不落盘。
// 落地方式由调用方决定；真正把 Codex 当 harness 驱动（子进程 + 事件流）见后续阶段。
//
// 依据的 Codex 配置形态（~/.codex/config.toml）：
//
//	model_provider = "janus"
//	model = "..."
//	[model_providers.janus]
//	name = "Janus"; base_url = ".../v1"; wire_api = "responses"; env_key = "JANUS_API_KEY"
//	[mcp_servers.janus]
//	url = "..."; enabled = true

// Name 是 harness 名（对应 agents.kind）。
const Name = "codex"

// providerID 是写进 config.toml 的 provider 键名。
const providerID = "janus"

// KeyEnv 是 API key 的环境变量名——不把明文写进配置文件。
const KeyEnv = "JANUS_API_KEY"

// Harness 是 Codex 的 Provision 实现。
type Harness struct{}

// Name 实现 agent.Harness。
func (Harness) Name() string { return Name }

// Provision 生成 Codex 侧的接入配置。
func (Harness) Provision(in agent.ProvisionInput) (agent.ProvisionResult, error) {
	base := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if base == "" {
		return agent.ProvisionResult{}, fmt.Errorf("codex: base URL is required")
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}

	var b strings.Builder
	b.WriteString("# janus：模型走 janus，厂商凭据只留在 janus 侧。\n")
	b.WriteString("model_provider = " + tomlStr(providerID) + "\n")
	if m := firstModel(in.Models); m != "" {
		b.WriteString("model = " + tomlStr(m) + "\n")
	}

	b.WriteString("\n[model_providers." + providerID + "]\n")
	b.WriteString("name = \"Janus\"\n")
	b.WriteString("base_url = " + tomlStr(base) + "\n")
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("env_key = " + tomlStr(KeyEnv) + "\n")
	b.WriteString("requires_openai_auth = false\n")

	if in.WantsMCP() {
		b.WriteString("\n[mcp_servers." + in.MCPNameOr() + "]\n")
		b.WriteString("url = " + tomlStr(strings.TrimSpace(in.MCPURL)) + "\n")
		b.WriteString("enabled = true\n")
	}

	res := agent.ProvisionResult{
		Env: map[string]string{KeyEnv: in.APIKey},
		Files: []agent.ProvisionFile{{
			Path:    "~/.codex/config.toml",
			Format:  "toml",
			Content: b.String(),
			Merge:   true,
		}},
		Command: []string{"codex", "exec", "--json"},
		Notes: []string{
			"Codex 走 Responses API（wire_api=responses），janus 的 /v1/responses 已支持。",
			"provider 相关键必须写在用户级 ~/.codex/config.toml，项目级会被忽略。",
		},
	}
	if in.WantsMCP() {
		res.Notes = append(res.Notes,
			"mcp_servers 让 Codex 能调用客户端声明的工具（工具在客户端执行）。")
	}
	return res, nil
}

// firstModel 返回第一个非空模型名。
func firstModel(models []string) string {
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

// tomlStr 把字符串转成 TOML 基础字符串（只做必要的转义）。
func tomlStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
