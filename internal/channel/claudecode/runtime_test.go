package claudecode

import (
	"encoding/json"
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
		Model:          "claude-sonnet-4-5",
		GatewayBaseURL: "https://janus.example.com",
		GatewayAPIKey:  "sk-janus",
		MCPURL:         "https://janus.example.com/mcp/babcdef",
		MCPName:        "janus",
	}
}

func TestRuntimeSpecArgsNew(t *testing.T) {
	spec := RuntimeSpec("")
	if spec.Name != Name || spec.Bin != "claude" {
		t.Fatalf("spec = %s/%s", spec.Name, spec.Bin)
	}
	args := spec.ArgsNew(cfg(), "explain this")

	if !containsSeq(args, "-p", "explain this") {
		t.Fatalf("prompt 应紧跟 -p: %v", args)
	}
	if !containsSeq(args, "--output-format", "stream-json", "--verbose") {
		t.Fatalf("缺 stream-json/verbose: %v", args)
	}
	if !containsSeq(args, "--model", "claude-sonnet-4-5") {
		t.Fatalf("缺 --model: %v", args)
	}
	if !containsSeq(args, "--allowedTools", "mcp__janus") {
		t.Fatalf("缺 --allowedTools: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "--resume") {
		t.Fatalf("首轮不应带 --resume: %v", args)
	}
}

func TestRuntimeSpecMCPConfigIsInlineHTTP(t *testing.T) {
	spec := RuntimeSpec("")
	args := spec.ArgsNew(cfg(), "x")

	i := indexOf(args, "--mcp-config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("缺 --mcp-config: %v", args)
	}
	var got struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &got); err != nil {
		t.Fatalf("--mcp-config 不是合法 JSON: %v (%s)", err, args[i+1])
	}
	srv, ok := got.MCPServers["janus"]
	if !ok {
		t.Fatalf("mcpServers 里没有 janus: %s", args[i+1])
	}
	// 远程 HTTP server 必须显式 type=http，否则 Claude Code 会当 stdio 起进程。
	if srv.Type != "http" || srv.URL != "https://janus.example.com/mcp/babcdef" {
		t.Fatalf("mcp server = %+v", srv)
	}
}

func TestRuntimeSpecArgsResume(t *testing.T) {
	spec := RuntimeSpec("/usr/local/bin/claude")
	args := spec.ArgsResume(cfg(), "sess-9", "continue")
	if !containsSeq(args, "--resume", "sess-9") {
		t.Fatalf("缺 --resume <id>: %v", args)
	}
}

func TestRuntimeSpecWithoutMCPOrModel(t *testing.T) {
	spec := RuntimeSpec("")
	args := spec.ArgsNew(agent.SessionConfig{}, "hi")
	joined := strings.Join(args, " ")
	for _, banned := range []string{"--mcp-config", "--allowedTools", "--model"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("未配置时不应出现 %s: %v", banned, args)
		}
	}
}

func TestRuntimeSpecEnv(t *testing.T) {
	spec := RuntimeSpec("")
	env := spec.Env(cfg())
	if !containsSeq(env, "ANTHROPIC_BASE_URL=https://janus.example.com/anthropic") {
		t.Fatalf("env = %v（Anthropic base 应以 /anthropic 结尾）", env)
	}
	if !containsSeq(env, "ANTHROPIC_AUTH_TOKEN=sk-janus") {
		t.Fatalf("env = %v", env)
	}
	if got := spec.Env(agent.SessionConfig{}); got != nil {
		t.Fatalf("无配置时不应注入 env: %v", got)
	}
}

func TestGatewayAnthropic(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"https://a.example.com":  "https://a.example.com/anthropic",
		"https://a.example.com/": "https://a.example.com/anthropic",
		// 已经带 /anthropic（或带 /v1 的同源写法）不重复追加
		"https://a.example.com/anthropic":  "https://a.example.com/anthropic",
		"https://a.example.com/anthropic/": "https://a.example.com/anthropic",
	}
	for in, want := range cases {
		if got := gatewayAnthropic(in); got != want {
			t.Errorf("gatewayAnthropic(%q) = %q, want %q", in, got, want)
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

func TestParseEvent(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantType []agent.EventType
		check    func(t *testing.T, evs []agent.Event)
		wantSID  string
	}{
		{
			name:    "system init 给出 session_id",
			line:    `{"type":"system","subtype":"init","session_id":"sess-1","model":"x","tools":["Bash"]}`,
			wantSID: "sess-1",
		},
		{
			name:     "api_retry 忽略",
			line:     `{"type":"system","subtype":"api_retry","session_id":"sess-1"}`,
			wantType: nil,
		},
		{
			name:     "assistant 正文 + 工具调用 + 用量",
			line:     `{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"tu1","name":"Read","input":{"path":"a.go"}}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":4}}}`,
			wantType: []agent.EventType{agent.EventText, agent.EventToolCall, agent.EventUsage},
			check: func(t *testing.T, evs []agent.Event) {
				assertEq(t, "text", evs[0].Text, "hi")
				assertEq(t, "tool name", evs[1].ToolCall.Name, "Read")
				assertEq(t, "tool id", evs[1].ToolCall.ID, "tu1")
				assertEq(t, "tool args", string(evs[1].ToolCall.Arguments), `{"path":"a.go"}`)
				assertEq(t, "in", evs[2].Usage.InputTokens, int64(10))
				assertEq(t, "cache", evs[2].Usage.CacheReadTokens, int64(4))
			},
		},
		{
			name:     "assistant 思考块",
			line:     `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"let me see"}]}}`,
			wantType: []agent.EventType{agent.EventReasoning},
			check:    func(t *testing.T, evs []agent.Event) { assertEq(t, "text", evs[0].Text, "let me see") },
		},
		{
			name:     "user 的 tool_result（字符串 content）",
			line:     `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"file contents"}]}}`,
			wantType: []agent.EventType{agent.EventToolResult},
			check: func(t *testing.T, evs []agent.Event) {
				assertEq(t, "id", evs[0].ToolCall.ID, "tu1")
				assertEq(t, "text", evs[0].Text, "file contents")
			},
		},
		{
			name:     "user 的 tool_result（块数组 content）",
			line:     `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}}`,
			wantType: []agent.EventType{agent.EventToolResult},
			check:    func(t *testing.T, evs []agent.Event) { assertEq(t, "text", evs[0].Text, "a\nb") },
		},
		{
			name:     "result 带用量与成本",
			line:     `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s","total_cost_usd":0.0123,"num_turns":2,"usage":{"input_tokens":30,"output_tokens":5}}`,
			wantType: []agent.EventType{agent.EventUsage},
			check: func(t *testing.T, evs []agent.Event) {
				assertEq(t, "in", evs[0].Usage.InputTokens, int64(30))
				assertEq(t, "cost", evs[0].Usage.CostUSD, 0.0123)
			},
		},
		{
			name:     "result 失败",
			line:     `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"hit limit","usage":{"input_tokens":1}}`,
			wantType: []agent.EventType{agent.EventUsage, agent.EventError},
			check:    func(t *testing.T, evs []agent.Event) { assertEq(t, "error", evs[1].Error, "hit limit") },
		},
		{
			name:     "未知类型忽略",
			line:     `{"type":"stream_event","event":{"type":"content_block_delta"}}`,
			wantType: nil,
		},
		{
			name:     "非 JSON 忽略",
			line:     `not json`,
			wantType: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs, sid := ParseEvent([]byte(tc.line))
			if tc.wantSID != "" && sid != tc.wantSID {
				t.Fatalf("sid = %q, want %q", sid, tc.wantSID)
			}
			if len(evs) != len(tc.wantType) {
				t.Fatalf("事件数 = %d (%+v), want %d", len(evs), evs, len(tc.wantType))
			}
			for i, want := range tc.wantType {
				if evs[i].Type != want {
					t.Fatalf("evs[%d].type = %s, want %s", i, evs[i].Type, want)
				}
			}
			if tc.check != nil {
				tc.check(t, evs)
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
