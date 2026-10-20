package codex

import (
	"reflect"
	"strings"
	"testing"

	"janus/internal/agent"
)

func containsSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func cfg() agent.SessionConfig {
	return agent.SessionConfig{
		Key:            "k",
		Directory:      "/work/repo",
		Model:          "gpt-5-codex",
		GatewayBaseURL: "https://janus.example.com/",
		GatewayAPIKey:  "sk-janus",
		MCPURL:         "https://janus.example.com/mcp/babcdef",
		MCPName:        "janus",
	}
}

func TestRuntimeSpecArgsNew(t *testing.T) {
	spec := RuntimeSpec("")
	if spec.Name != Name || spec.Bin != "codex" {
		t.Fatalf("spec = %s/%s", spec.Name, spec.Bin)
	}
	args := spec.ArgsNew(cfg(), "fix the bug")

	// -c 覆盖（根命令选项，必须在 exec 前）
	if args[0] != "-c" {
		t.Fatalf("args 应以 -c 开头: %v", args)
	}
	if !containsSeq(args, "-c", `model_provider="janus"`) {
		t.Fatalf("缺少 provider 覆盖: %v", args)
	}
	if !containsSeq(args, "-c", `model_providers.janus.base_url="https://janus.example.com/v1"`) {
		t.Fatalf("base_url 未规整为 /v1 或缺失: %v", args)
	}
	if !containsSeq(args, "-c", `model_providers.janus.wire_api="responses"`) {
		t.Fatalf("缺少 wire_api: %v", args)
	}
	if !containsSeq(args, "-c", `mcp_servers.janus.url="https://janus.example.com/mcp/babcdef"`) {
		t.Fatalf("缺少 MCP 注册: %v", args)
	}
	// exec 必须在 -c 之后
	ci, ei := indexOf(args, "-c"), indexOf(args, "exec")
	if ei < 0 || ci > ei {
		t.Fatalf("exec 位置不对: %v", args)
	}
	if !containsSeq(args, "exec", "--json", "--skip-git-repo-check", "--cd", "/work/repo", "--model", "gpt-5-codex") {
		t.Fatalf("缺少 exec 选项: %v", args)
	}
	if args[len(args)-1] != "fix the bug" {
		t.Fatalf("prompt 应作为最后一个位置参数: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "resume") {
		t.Fatalf("首轮不应带 resume: %v", args)
	}
}

func TestRuntimeSpecArgsResume(t *testing.T) {
	spec := RuntimeSpec("/usr/local/bin/codex")
	args := spec.ArgsResume(cfg(), "thread-42", "continue")

	if !containsSeq(args, "exec", "resume", "thread-42") {
		t.Fatalf("缺少 exec resume <id>: %v", args)
	}
	if !containsSeq(args, "resume", "thread-42", "--json") {
		t.Fatalf("resume 子命令后才能跟选项: %v", args)
	}
	if args[len(args)-1] != "continue" {
		t.Fatalf("prompt 应最后: %v", args)
	}
}

func TestRuntimeSpecArgsWithoutGatewayOrMCP(t *testing.T) {
	spec := RuntimeSpec("")
	args := spec.ArgsNew(agent.SessionConfig{Model: "m"}, "hi")

	if containsSeq(args, "-c", `model_providers.janus.base_url=""`) {
		t.Fatalf("空 base URL 不应写进配置: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "mcp_servers") {
		t.Fatalf("未声明 MCP 时不应注册: %v", args)
	}
	if containsSeq(args, "--cd", "") {
		t.Fatalf("空目录不应传 --cd: %v", args)
	}
}

func TestRuntimeSpecEnv(t *testing.T) {
	spec := RuntimeSpec("")
	env := spec.Env(cfg())
	if !reflect.DeepEqual(env, []string{"JANUS_API_KEY=sk-janus"}) {
		t.Fatalf("env = %v", env)
	}
	if got := spec.Env(agent.SessionConfig{}); got != nil {
		t.Fatalf("无 key 时不应注入 env: %v", got)
	}
	// 不把明文 key 写进 argv
	if strings.Contains(strings.Join(spec.ArgsNew(cfg(), "x"), " "), "sk-janus") {
		t.Fatal("argv 里不应出现明文 key")
	}
}

func TestGatewayV1(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"https://a.example.com":       "https://a.example.com/v1",
		"https://a.example.com/":      "https://a.example.com/v1",
		"https://a.example.com/v1":    "https://a.example.com/v1",
		"https://a.example.com/v1/":   "https://a.example.com/v1",
		"https://a.example.com/proxy": "https://a.example.com/proxy/v1",
	}
	for in, want := range cases {
		if got := gatewayV1(in); got != want {
			t.Errorf("gatewayV1(%q) = %q, want %q", in, got, want)
		}
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// 事件样本取自 codex exec --json 的协议（codex-rs exec_events.rs）。
func TestParseEvent(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantType agent.EventType
		check    func(t *testing.T, e agent.Event)
		wantSID  string
		wantNone bool
	}{
		{
			name:     "thread.started 给出 thread_id",
			line:     `{"type":"thread.started","thread_id":"th-1"}`,
			wantSID:  "th-1",
			wantNone: true,
		},
		{
			name:     "正文",
			line:     `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"hello"}}`,
			wantType: agent.EventText,
			check:    func(t *testing.T, e agent.Event) { assertEq(t, "text", e.Text, "hello") },
		},
		{
			name:     "思考",
			line:     `{"type":"item.completed","item":{"id":"r1","type":"reasoning","text":"thinking"}}`,
			wantType: agent.EventReasoning,
			check:    func(t *testing.T, e agent.Event) { assertEq(t, "text", e.Text, "thinking") },
		},
		{
			name:     "命令执行开始 → 工具调用",
			line:     `{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"ls -la","status":"in_progress"}}`,
			wantType: agent.EventToolCall,
			check: func(t *testing.T, e agent.Event) {
				assertEq(t, "name", e.ToolCall.Name, "shell")
				assertEq(t, "id", e.ToolCall.ID, "c1")
				if !strings.Contains(string(e.ToolCall.Arguments), "ls -la") {
					t.Fatalf("arguments = %s", e.ToolCall.Arguments)
				}
			},
		},
		{
			name:     "命令执行完成 → 工具结果",
			line:     `{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"ls","aggregated_output":"a\nb","exit_code":0,"status":"completed"}}`,
			wantType: agent.EventToolResult,
			check: func(t *testing.T, e agent.Event) {
				assertEq(t, "id", e.ToolCall.ID, "c1")
				assertEq(t, "text", e.Text, "a\nb")
			},
		},
		{
			name:     "MCP 工具调用",
			line:     `{"type":"item.started","item":{"id":"m1","type":"mcp_tool_call","server":"janus","tool":"read_file","arguments":{"path":"a.go"},"status":"in_progress"}}`,
			wantType: agent.EventToolCall,
			check: func(t *testing.T, e agent.Event) {
				assertEq(t, "name", e.ToolCall.Name, "mcp:janus.read_file")
				assertEq(t, "args", string(e.ToolCall.Arguments), `{"path":"a.go"}`)
			},
		},
		{
			name:     "MCP 工具结果错误",
			line:     `{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"janus","tool":"read_file","error":{"message":"boom"},"status":"failed"}}`,
			wantType: agent.EventToolResult,
			check:    func(t *testing.T, e agent.Event) { assertEq(t, "text", e.Text, "boom") },
		},
		{
			name:     "用量",
			line:     `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":7,"reasoning_output_tokens":3}}`,
			wantType: agent.EventUsage,
			check: func(t *testing.T, e agent.Event) {
				assertEq(t, "in", e.Usage.InputTokens, int64(100))
				assertEq(t, "out", e.Usage.OutputTokens, int64(7))
				assertEq(t, "cache", e.Usage.CacheReadTokens, int64(40))
				assertEq(t, "reasoning", e.Usage.ReasoningTokens, int64(3))
			},
		},
		{
			name:     "turn.failed",
			line:     `{"type":"turn.failed","error":{"message":"model refused"}}`,
			wantType: agent.EventError,
			check:    func(t *testing.T, e agent.Event) { assertEq(t, "error", e.Error, "model refused") },
		},
		{
			name:     "顶层 error",
			line:     `{"type":"error","message":"stream broken"}`,
			wantType: agent.EventError,
			check:    func(t *testing.T, e agent.Event) { assertEq(t, "error", e.Error, "stream broken") },
		},
		{
			name:     "未知类型忽略",
			line:     `{"type":"turn.started"}`,
			wantNone: true,
		},
		{
			name:     "非 JSON 行忽略",
			line:     `warning: something`,
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs, sid := ParseEvent([]byte(tc.line))
			if tc.wantSID != "" && sid != tc.wantSID {
				t.Fatalf("sid = %q, want %q", sid, tc.wantSID)
			}
			if tc.wantNone {
				if len(evs) != 0 {
					t.Fatalf("应为空，得到 %+v", evs)
				}
				return
			}
			if len(evs) != 1 {
				t.Fatalf("事件数 = %d (%+v)", len(evs), evs)
			}
			if evs[0].Type != tc.wantType {
				t.Fatalf("type = %s, want %s", evs[0].Type, tc.wantType)
			}
			if tc.check != nil {
				tc.check(t, evs[0])
			}
		})
	}
}

func assertEq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}
