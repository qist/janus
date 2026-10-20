package agent

import "testing"

func TestParseToolMode(t *testing.T) {
	cases := map[string]ToolMode{
		"":       ToolModeBridge,
		"bridge": ToolModeBridge,
		"BRIDGE": ToolModeBridge,
		"native": ToolModeNative,
		" none ": ToolModeNone,
		"bogus":  ToolModeBridge,
	}
	for in, want := range cases {
		if got := ParseToolMode(in); got != want {
			t.Errorf("ParseToolMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProvisionInputMCPName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "janus"},
		{"  ", "janus"},
		{"janus", "janus"},
		{"My Server!", "myserver"},
		{"!!!", "janus"},
		{"Janus_MCP-2", "janus_mcp-2"},
	}
	for _, c := range cases {
		got := ProvisionInput{MCPName: c.in}.MCPNameOr()
		if got != c.want {
			t.Errorf("MCPNameOr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProvisionInputWantsMCP(t *testing.T) {
	cases := []struct {
		mode ToolMode
		url  string
		want bool
	}{
		{ToolModeBridge, "http://x/mcp/t", true},
		{ToolModeBridge, "", false},
		{ToolModeNative, "http://x/mcp/t", false},
		{ToolModeNone, "http://x/mcp/t", false},
	}
	for _, c := range cases {
		in := ProvisionInput{Mode: c.mode, MCPURL: c.url}
		if got := in.WantsMCP(); got != c.want {
			t.Errorf("WantsMCP(%s,%q) = %v, want %v", c.mode, c.url, got, c.want)
		}
	}
}

type fakeHarness struct{ name string }

func (f fakeHarness) Name() string { return f.name }
func (f fakeHarness) Provision(ProvisionInput) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}

func TestHarnessRegistry(t *testing.T) {
	var nilReg *HarnessRegistry
	if _, ok := nilReg.Get("x"); ok {
		t.Fatal("nil registry should miss")
	}
	nilReg.Register(fakeHarness{"x"}) // 不应 panic

	reg := NewHarnessRegistry()
	reg.Register(fakeHarness{"b"})
	reg.Register(fakeHarness{"a"})
	reg.Register(nil)
	reg.Register(fakeHarness{"b"}) // 覆盖：不重复进 order

	if got := reg.Names(); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("Names() = %v, want [b a]", got)
	}
	if got := reg.SortNames(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("SortNames() = %v, want [a b]", got)
	}
	if h, ok := reg.Get("a"); !ok || h.Name() != "a" {
		t.Fatalf("Get(a) = %v, %v", h, ok)
	}
	if _, ok := reg.Get("zzz"); ok {
		t.Fatal("Get(zzz) should miss")
	}
}
