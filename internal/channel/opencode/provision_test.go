package opencode

import (
	"encoding/json"
	"testing"

	"janus/internal/agent"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not valid json: %v\n%s", err, s)
	}
	return m
}

func TestProvisionBridgeInjectsProviderAndOrchestrator(t *testing.T) {
	h := NewHarness("ob-")
	res, err := h.Provision(agent.ProvisionInput{
		BaseURL: "http://127.0.0.1:2810",
		APIKey:  "<agent key>",
		Models:  []string{"claude-opus-4-1"},
		Mode:    agent.ToolModeBridge,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	env := decode(t, res.Env["OPENCODE_CONFIG_CONTENT"])

	prov, ok := env["provider"].(map[string]any)["janus"].(map[string]any)
	if !ok {
		t.Fatalf("provider.janus missing: %+v", env)
	}
	opts := prov["options"].(map[string]any)
	if opts["baseURL"] != "http://127.0.0.1:2810/v1" || opts["apiKey"] != "<agent key>" {
		t.Fatalf("provider options = %+v", opts)
	}
	if _, ok := prov["models"].(map[string]any)["claude-opus-4-1"]; !ok {
		t.Fatalf("model missing: %+v", prov["models"])
	}
	if _, ok := env["agents"].(map[string]any)["orchestrator"]; !ok {
		t.Fatalf("orchestrator agent missing: %+v", env)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "~/.config/opencode/opencode.jsonc" {
		t.Fatalf("files = %+v", res.Files)
	}
}

func TestProvisionNoneModeOmitsOrchestrator(t *testing.T) {
	h := NewHarness("ob-")
	res, err := h.Provision(agent.ProvisionInput{
		BaseURL: "http://x", APIKey: "k", Mode: agent.ToolModeNone,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	env := decode(t, res.Env["OPENCODE_CONFIG_CONTENT"])
	if _, ok := env["agents"]; ok {
		t.Fatalf("none mode should not define agents: %+v", env)
	}
	if _, ok := env["provider"]; !ok {
		t.Fatalf("provider should still be injected: %+v", env)
	}
}

func TestNewHarnessDefaultToolPrefix(t *testing.T) {
	if h := NewHarness(""); h.toolPrefix != defaultToolPrefix {
		t.Fatalf("toolPrefix = %q", h.toolPrefix)
	}
}
