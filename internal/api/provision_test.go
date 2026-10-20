package api

import (
	"testing"

	"janus/internal/agent"
	"janus/internal/config"
)

func TestToolModeMapping(t *testing.T) {
	cases := []struct {
		agentName string
		calling   bool
		want      agent.ToolMode
	}{
		{"build", false, agent.ToolModeNative},
		{"build", true, agent.ToolModeBridge},
		{"orchestrator", true, agent.ToolModeBridge},
		{"orchestrator", false, agent.ToolModeNone},
		{"plan", false, agent.ToolModeNone},
	}
	for _, c := range cases {
		s := &Server{cfg: config.Config{Agent: c.agentName, ToolCalling: c.calling}}
		if got := s.toolMode(); got != c.want {
			t.Errorf("toolMode(agent=%q,calling=%v) = %q, want %q",
				c.agentName, c.calling, got, c.want)
		}
	}
}

func TestHarnessRegistryLookup(t *testing.T) {
	s := &Server{}
	for _, kind := range []string{"opencode", "codex", "claude-code"} {
		h, ok := s.harnessFor(kind)
		if !ok || h == nil {
			t.Fatalf("harnessFor(%q) not registered", kind)
		}
		if h.Name() != kind {
			t.Errorf("harnessFor(%q).Name() = %q", kind, h.Name())
		}
	}
	if _, ok := s.harnessFor("nope"); ok {
		t.Fatal("unknown kind should miss")
	}
}
