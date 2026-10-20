package codex

import (
	"strings"
	"testing"

	"janus/internal/agent"
)

func TestProvisionBridge(t *testing.T) {
	res, err := Harness{}.Provision(agent.ProvisionInput{
		BaseURL: "http://127.0.0.1:2810/",
		APIKey:  "<agent key>",
		MCPURL:  "http://127.0.0.1:2810/mcp/abc",
		MCPName: "janus",
		Models:  []string{"claude-opus-4-1"},
		Mode:    agent.ToolModeBridge,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := res.Env[KeyEnv]; got != "<agent key>" {
		t.Fatalf("env %s = %q", KeyEnv, got)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "~/.codex/config.toml" || !res.Files[0].Merge {
		t.Fatalf("files = %+v", res.Files)
	}
	body := res.Files[0].Content
	for _, want := range []string{
		`model_provider = "janus"`,
		`model = "claude-opus-4-1"`,
		"[model_providers.janus]",
		`base_url = "http://127.0.0.1:2810/v1"`,
		`wire_api = "responses"`,
		`env_key = "JANUS_API_KEY"`,
		"[mcp_servers.janus]",
		`url = "http://127.0.0.1:2810/mcp/abc"`,
		"enabled = true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml missing %q\n%s", want, body)
		}
	}
	if len(res.Command) == 0 || res.Command[0] != "codex" {
		t.Fatalf("command = %v", res.Command)
	}
}

func TestProvisionNoMCPWhenNotBridge(t *testing.T) {
	for _, mode := range []agent.ToolMode{agent.ToolModeNative, agent.ToolModeNone} {
		res, err := Harness{}.Provision(agent.ProvisionInput{
			BaseURL: "http://x", MCPURL: "http://x/mcp/t", Mode: mode,
		})
		if err != nil {
			t.Fatalf("Provision(%s): %v", mode, err)
		}
		if strings.Contains(res.Files[0].Content, "[mcp_servers") {
			t.Errorf("mode %s should not register MCP", mode)
		}
	}
}

func TestProvisionRequiresBase(t *testing.T) {
	if _, err := (Harness{}).Provision(agent.ProvisionInput{}); err == nil {
		t.Fatal("want error for empty base URL")
	}
}

func TestTomlStrEscaping(t *testing.T) {
	got := tomlStr("a\"b\\c")
	if got != `"a\"b\\c"` {
		t.Fatalf("tomlStr = %s", got)
	}
}
