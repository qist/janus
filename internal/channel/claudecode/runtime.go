package claudecode

import (
	"encoding/json"
	"strings"

	"janus/internal/agent"
)

// runtime.go —— Claude Code CLI 的 AgentRuntime：把 claude 当子进程驱动。
//
// claude -p --output-format stream-json --verbose 把事件按 JSONL 写到 stdout：
//
//	system(subtype=init)  首行，带 session_id / model / tools / mcp_servers
//	assistant             Anthropic Message，content 内含 text / thinking / tool_use
//	user                  content 内含 tool_result
//	system(subtype=api_retry)  重试提示（忽略）
//	result                末行，带 usage / total_cost_usd / session_id / is_error
//
// 多轮：首轮记下 session_id，之后 `claude -p --resume <session_id>` 续接。
//
// 刻意不开 --include-partial-messages：开了之后 assistant 正文会同时以
// stream_event 增量出现，两组事件会重复计数。本适配器只在 assistant 消息
// 完成时产出正文，粒度=一条消息（够用且不重不漏）。
//
// 依据的 CLI 形态：
//
//	claude -p "<prompt>" --output-format stream-json --verbose [--model M] \
//	       [--mcp-config '{...}'] [--allowedTools mcp__janus]
//	claude -p "<prompt>" --resume <session_id> ...

// DefaultBin 是 claude 可执行文件的默认名。
const DefaultBin = "claude"

// RuntimeSpec 返回驱动 Claude Code CLI 的 CLISpec。
func RuntimeSpec(bin string) agent.CLISpec {
	if strings.TrimSpace(bin) == "" {
		bin = DefaultBin
	}
	return agent.CLISpec{
		Name: Name,
		Bin:  bin,
		ArgsNew: func(cfg agent.SessionConfig, prompt string) []string {
			return ccArgs(cfg, "", prompt)
		},
		ArgsResume: func(cfg agent.SessionConfig, harnessID, prompt string) []string {
			return ccArgs(cfg, harnessID, prompt)
		},
		Env: func(cfg agent.SessionConfig) []string {
			var env []string
			if b := gatewayAnthropic(cfg.GatewayBaseURL); b != "" {
				env = append(env, "ANTHROPIC_BASE_URL="+b)
			}
			if k := strings.TrimSpace(cfg.GatewayAPIKey); k != "" {
				env = append(env, "ANTHROPIC_AUTH_TOKEN="+k)
			}
			return env
		},
		Parse: ParseEvent,
	}
}

// ccArgs 拼一轮 claude 的 argv；resumeID 非空时续接会话。
func ccArgs(cfg agent.SessionConfig, resumeID, prompt string) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if id := strings.TrimSpace(resumeID); id != "" {
		args = append(args, "--resume", id)
	}
	if m := strings.TrimSpace(cfg.Model); m != "" {
		args = append(args, "--model", m)
	}
	if cfg.WantsMCP() {
		args = append(args, "--mcp-config", mcpConfigJSON(cfg))
		// 无人值守：预先放行 janus 这一个 MCP server 的工具，避免停在权限提示上。
		args = append(args, "--allowedTools", "mcp__"+cfg.MCPName)
	}
	return args
}

// gatewayAnthropic 规整 janus 基址为 Claude Code 期望的 "<base>/anthropic"。
func gatewayAnthropic(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	if !strings.HasSuffix(base, "/anthropic") {
		base += "/anthropic"
	}
	return base
}

// mcpConfigJSON 生成 --mcp-config 的内联 JSON（远程 HTTP 必须显式 type=http）。
func mcpConfigJSON(cfg agent.SessionConfig) string {
	m := map[string]any{
		"mcpServers": map[string]any{
			cfg.MCPName: map[string]any{
				"type": "http",
				"url":  strings.TrimSpace(cfg.MCPURL),
			},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ---------- 事件解析 ----------

type ccEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	TotalCost float64         `json:"total_cost_usd"`
	NumTurns  int             `json:"num_turns"`
	Usage     *ccUsage        `json:"usage"`
}

type ccUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type ccMessage struct {
	Content []ccBlock `json:"content"`
	Usage   *ccUsage  `json:"usage"`
}

type ccBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type ccTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseEvent 把 claude 的一行 JSONL 翻译成事件；第二个返回值为会话 id（若有）。
func ParseEvent(line []byte) ([]agent.Event, string) {
	var ev ccEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil, ""
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			return nil, strings.TrimSpace(ev.SessionID)
		}
		return nil, ""

	case "assistant":
		var msg ccMessage
		if json.Unmarshal(ev.Message, &msg) != nil {
			return nil, ""
		}
		out := make([]agent.Event, 0, len(msg.Content)+1)
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					out = append(out, agent.Event{Type: agent.EventText, Text: b.Text})
				}
			case "thinking":
				if b.Thinking != "" {
					out = append(out, agent.Event{Type: agent.EventReasoning, Text: b.Thinking})
				}
			case "tool_use":
				out = append(out, agent.Event{Type: agent.EventToolCall,
					ToolCall: &agent.ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input}})
			}
		}
		if msg.Usage != nil {
			out = append(out, agent.Event{Type: agent.EventUsage, Usage: ccUsageToAgent(msg.Usage, 0)})
		}
		return out, ""

	case "user":
		var msg ccMessage
		if json.Unmarshal(ev.Message, &msg) != nil {
			return nil, ""
		}
		var out []agent.Event
		for _, b := range msg.Content {
			if b.Type == "tool_result" {
				out = append(out, agent.Event{Type: agent.EventToolResult,
					Text:     ccToolResultText(b.Content),
					ToolCall: &agent.ToolCall{ID: b.ToolUseID}})
			}
		}
		return out, ""

	case "result":
		out := []agent.Event{{Type: agent.EventUsage, Usage: ccUsageToAgent(ev.Usage, ev.TotalCost)}}
		if ev.IsError {
			msg := strings.TrimSpace(ev.Result)
			if msg == "" {
				msg = "claude run failed"
			}
			out = append(out, agent.Event{Type: agent.EventError, Error: msg})
		}
		return out, ""
	}
	return nil, ""
}

func ccUsageToAgent(u *ccUsage, cost float64) *agent.Usage {
	out := &agent.Usage{CostUSD: cost}
	if u != nil {
		out.InputTokens = u.InputTokens
		out.OutputTokens = u.OutputTokens
		out.CacheReadTokens = u.CacheReadInputTokens
	}
	return out
}

// ccToolResultText 兼容 tool_result 的两种 content：字符串或块数组。
func ccToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []ccTextBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}
