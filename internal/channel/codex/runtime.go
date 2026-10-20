package codex

import (
	"encoding/json"
	"strconv"
	"strings"

	"janus/internal/agent"
)

// runtime.go —— Codex CLI 的 AgentRuntime：把 codex 当子进程驱动。
//
// codex exec 在非交互模式下把事件按 JSONL 写到 stdout，类型见 codex-rs
// exec_events.rs：thread.started / turn.started / item.started|updated|completed
// （item.type ∈ agent_message|reasoning|command_execution|file_change|mcp_tool_call|
// web_search|todo_list|error）/ turn.completed / turn.failed / error。
//
// 多轮会话：首轮记下 thread.started 里的 thread_id，之后用
// `codex exec resume <thread_id>` 续接。
//
// 依据的 CLI 形态：
//
//	codex -c model_provider="janus" -c model_providers.janus.base_url="<janus>/v1" \
//	      exec --json --skip-git-repo-check --cd <dir> --model <m> "<prompt>"
//	codex -c ... exec resume <thread_id> --json --skip-git-repo-check "<prompt>"
//
// -c 是根命令的配置覆盖选项（会覆盖 ~/.codex/config.toml），因此放在 exec 之前。

// DefaultBin 是 codex 可执行文件的默认名。
const DefaultBin = "codex"

// RuntimeSpec 返回驱动 Codex CLI 的 CLISpec。
func RuntimeSpec(bin string) agent.CLISpec {
	if strings.TrimSpace(bin) == "" {
		bin = DefaultBin
	}
	return agent.CLISpec{
		Name: Name,
		Bin:  bin,
		ArgsNew: func(cfg agent.SessionConfig, prompt string) []string {
			args := configOverrides(cfg)
			args = append(args, "exec")
			args = append(args, execOptions(cfg)...)
			return append(args, prompt)
		},
		ArgsResume: func(cfg agent.SessionConfig, harnessID, prompt string) []string {
			args := configOverrides(cfg)
			args = append(args, "exec", "resume", harnessID)
			args = append(args, execOptions(cfg)...)
			return append(args, prompt)
		},
		Env: func(cfg agent.SessionConfig) []string {
			if strings.TrimSpace(cfg.GatewayAPIKey) == "" {
				return nil
			}
			return []string{KeyEnv + "=" + cfg.GatewayAPIKey}
		},
		Parse: ParseEvent,
	}
}

// configOverrides 生成把 Codex 指向 janus 的 -c 覆盖（根命令选项）。
func configOverrides(cfg agent.SessionConfig) []string {
	args := []string{
		"-c", "model_provider=" + tomlStr(providerID),
		"-c", "model_providers." + providerID + ".name=" + tomlStr("Janus"),
		"-c", "model_providers." + providerID + ".wire_api=" + tomlStr("responses"),
		"-c", "model_providers." + providerID + ".env_key=" + tomlStr(KeyEnv),
		"-c", "model_providers." + providerID + ".requires_openai_auth=false",
	}
	if base := gatewayV1(cfg.GatewayBaseURL); base != "" {
		args = append(args, "-c", "model_providers."+providerID+".base_url="+tomlStr(base))
	}
	if cfg.WantsMCP() {
		name := cfg.MCPName
		args = append(args,
			"-c", "mcp_servers."+name+".url="+tomlStr(strings.TrimSpace(cfg.MCPURL)),
			"-c", "mcp_servers."+name+".enabled=true")
	}
	return args
}

// execOptions 是 exec 子命令共享的选项。
func execOptions(cfg agent.SessionConfig) []string {
	args := []string{"--json", "--skip-git-repo-check"}
	if d := strings.TrimSpace(cfg.Directory); d != "" {
		args = append(args, "--cd", d)
	}
	if m := strings.TrimSpace(cfg.Model); m != "" {
		args = append(args, "--model", m)
	}
	return args
}

// gatewayV1 规整 janus 基址为 Codex 期望的 "<base>/v1" 形态。
func gatewayV1(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}

// ---------- 事件解析 ----------

type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Message  string          `json:"message"`
	Item     *codexItem      `json:"item"`
	Usage    *codexUsage     `json:"usage"`
	Error    *codexErrorBody `json:"error"`
}

type codexErrorBody struct {
	Message string `json:"message"`
}

type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Changes          json.RawMessage `json:"changes"`
	Query            string          `json:"query"`
	Error            *codexErrorBody `json:"error"`
}

// ParseEvent 把 codex 的一行 JSONL 翻译成事件；第二个返回值为线程 id（若有）。
func ParseEvent(line []byte) ([]agent.Event, string) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil, "" // 非事件行（harness 偶发写日志）忽略
	}
	switch ev.Type {
	case "thread.started":
		return nil, strings.TrimSpace(ev.ThreadID)

	case "turn.completed":
		if ev.Usage == nil {
			return nil, ""
		}
		return []agent.Event{{Type: agent.EventUsage, Usage: &agent.Usage{
			InputTokens:     ev.Usage.InputTokens,
			OutputTokens:    ev.Usage.OutputTokens,
			CacheReadTokens: ev.Usage.CachedInputTokens,
			ReasoningTokens: ev.Usage.ReasoningOutputTokens,
		}}}, ""

	case "turn.failed":
		return []agent.Event{{Type: agent.EventError, Error: codexErrText(ev.Error, "codex turn failed")}}, ""

	case "error":
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = codexErrText(ev.Error, "codex error")
		}
		return []agent.Event{{Type: agent.EventError, Error: msg}}, ""

	case "item.started":
		if ev.Item == nil {
			return nil, ""
		}
		if e, ok := codexToolCall(ev.Item); ok {
			return []agent.Event{e}, ""
		}
		return nil, ""

	case "item.completed":
		if ev.Item == nil {
			return nil, ""
		}
		return codexItemCompleted(ev.Item), ""
	}
	return nil, ""
}

// codexToolCall 把「工具类 item」的开始事件翻译成一次工具调用。
func codexToolCall(it *codexItem) (agent.Event, bool) {
	call := &agent.ToolCall{ID: it.ID}
	switch it.Type {
	case "command_execution":
		call.Name = "shell"
		call.Arguments = jsonArg(map[string]any{"command": it.Command})
	case "file_change":
		call.Name = "apply_patch"
		call.Arguments = it.Changes
	case "mcp_tool_call":
		call.Name = mcpToolName(it)
		call.Arguments = it.Arguments
	case "web_search":
		call.Name = "web_search"
		call.Arguments = jsonArg(map[string]any{"query": it.Query})
	default:
		return agent.Event{}, false
	}
	return agent.Event{Type: agent.EventToolCall, ToolCall: call}, true
}

// codexItemCompleted 处理终态 item：正文/思考/工具结果/错误。
func codexItemCompleted(it *codexItem) []agent.Event {
	switch it.Type {
	case "agent_message":
		if it.Text == "" {
			return nil
		}
		return []agent.Event{{Type: agent.EventText, Text: it.Text}}
	case "reasoning":
		if it.Text == "" {
			return nil
		}
		return []agent.Event{{Type: agent.EventReasoning, Text: it.Text}}
	case "error":
		return []agent.Event{{Type: agent.EventError, Error: codexErrText(it.Error, "codex item error")}}
	case "command_execution":
		text := it.AggregatedOutput
		if text == "" && it.ExitCode != nil {
			text = "exit code " + strconv.Itoa(*it.ExitCode)
		}
		return []agent.Event{toolResult(it.ID, text)}
	case "file_change":
		return []agent.Event{toolResult(it.ID, it.Status)}
	case "mcp_tool_call":
		text := ""
		if it.Error != nil && it.Error.Message != "" {
			text = it.Error.Message
		} else if len(it.Result) > 0 {
			text = string(it.Result)
		}
		return []agent.Event{toolResult(it.ID, text)}
	case "web_search":
		return []agent.Event{toolResult(it.ID, it.Status)}
	}
	return nil
}

func toolResult(id, text string) agent.Event {
	return agent.Event{Type: agent.EventToolResult, Text: text, ToolCall: &agent.ToolCall{ID: id}}
}

func mcpToolName(it *codexItem) string {
	name := strings.TrimSpace(it.Tool)
	if s := strings.TrimSpace(it.Server); s != "" {
		name = s + "." + name
	}
	if name == "" {
		name = "mcp"
	}
	return "mcp:" + name
}

func jsonArg(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func codexErrText(e *codexErrorBody, fallback string) string {
	if e != nil {
		if m := strings.TrimSpace(e.Message); m != "" {
			return m
		}
	}
	return fallback
}
