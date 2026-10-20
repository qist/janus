package agent

import (
	"context"
	"testing"
)

type stubRuntime struct {
	name     string
	ready    bool
	sessions int
}

func (s *stubRuntime) Name() string { return s.name }
func (s *stubRuntime) EnsureReady(context.Context) error {
	s.ready = true
	return nil
}
func (s *stubRuntime) CreateSession(context.Context, SessionSpec) (string, error) {
	s.sessions++
	return "sid", nil
}
func (s *stubRuntime) Prompt(context.Context, string, PromptReq) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventDone}
	close(ch)
	return ch, nil
}
func (s *stubRuntime) SetModel(context.Context, string, string) error { return nil }
func (s *stubRuntime) Interrupt(context.Context, string) error        { return nil }
func (s *stubRuntime) Close(context.Context) error                    { return nil }

func TestRegistryRegisterGetList(t *testing.T) {
	reg := NewRegistry()
	oc := &stubRuntime{name: NameOpenCode}
	di := &stubRuntime{name: NameDirect}
	reg.Register(oc)
	reg.Register(di)

	if _, ok := reg.Get(NameOpenCode); !ok {
		t.Fatal("opencode not found")
	}
	if _, ok := reg.Get("missing"); ok {
		t.Fatal("unexpected runtime for unknown name")
	}
	names := reg.Names()
	if len(names) != 2 || names[0] != NameOpenCode || names[1] != NameDirect {
		t.Fatalf("names = %v (want registration order)", names)
	}
	if got := reg.List(); len(got) != 2 || got[0].Name() != NameOpenCode {
		t.Fatalf("list = %v", got)
	}
}

// 重复注册同名 runtime 覆盖旧值，但不重复出现在顺序里。
func TestRegistryReplaceKeepsOrder(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubRuntime{name: NameOpenCode})
	replacement := &stubRuntime{name: NameOpenCode}
	reg.Register(replacement)
	if len(reg.Names()) != 1 {
		t.Fatalf("names = %v", reg.Names())
	}
	got, _ := reg.Get(NameOpenCode)
	if got != replacement {
		t.Fatal("replacement did not take effect")
	}
}

// 空注册表（nil）上的查询要安全。
func TestRegistryNilSafe(t *testing.T) {
	var reg *Registry
	if _, ok := reg.Get("x"); ok {
		t.Fatal("nil registry returned a runtime")
	}
	if reg.Names() != nil || reg.List() != nil {
		t.Fatal("nil registry should return nothing")
	}
	reg.Register(&stubRuntime{name: "x"}) // 不应 panic
}

func TestStubRuntimeInterface(t *testing.T) {
	// 编译期确保 stub 满足接口。
	var _ AgentRuntime = (*stubRuntime)(nil)
}
