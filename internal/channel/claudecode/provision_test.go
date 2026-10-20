package claudecode

import (
	"encoding/json"
	"strings"
	"testing"

	"janus/internal/agent"
)

func TestProvisionBridge(t *testing.T) {
	res, err := Harness{}.Provision(agent.ProvisionInput{
		BaseURL: "http://127.0.0.1:2810/",
		APIKey:  "<agent key>",
		MCPURL:  "http://127.0.0.1:2810/mcp/abc",
		Mode:    agent.ToolModeBridge,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := res.Env["ANTHROPIC_BASE_URL"]; got != "http://127.0.0.1:2810/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := res.Env["ANTHROPIC_AUTH_TOKEN"]; got != "<agent key>" {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN = %q", got)
	}
	if len(res.Files) != 1 || res.Files[0].Path != ".mcp.json" {
		t.Fatalf("files = %+v", res.Files)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(res.Files[0].Content), &cfg); err != nil {
		t.Fatalf("mcp.json not valid json: %v\n%s", err, res.Files[0].Content)
	}
	srv, ok := cfg.MCPServers["janus"]
	if !ok || srv.Type != "http" || srv.URL != "http://127.0.0.1:2810/mcp/abc" {
		t.Fatalf("mcpServers.janus = %+v", cfg.MCPServers)
	}
}

func TestProvisionNoMCPFileWhenNotBridge(t *testing.T) {
	res, err := Harness{}.Provision(agent.ProvisionInput{
		BaseURL: "http://x", MCPURL: "http://x/mcp/t", Mode: agent.ToolModeNative,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(res.Files) != 0 {
		t.Fatalf("want no files, got %+v", res.Files)
	}
	if res.Env["ANTHROPIC_BASE_URL"] != "http://x/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", res.Env["ANTHROPIC_BASE_URL"])
	}
}

func TestProvisionDoesNotDoubleAnthropicSuffix(t *testing.T) {
	res, err := Harness{}.Provision(agent.ProvisionInput{BaseURL: "http://x/anthropic"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := res.Env["ANTHROPIC_BASE_URL"]; got != "http://x/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if strings.HasSuffix(res.Env["ANTHROPIC_BASE_URL"], "/anthropic/anthropic") {
		t.Fatal("suffix doubled")
	}
}

func TestProvisionRequiresBase(t *testing.T) {
	if _, err := (Harness{}).Provision(agent.ProvisionInput{}); err == nil {
		t.Fatal("want error for empty base URL")
	}
}
