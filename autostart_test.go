package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestFindOpenCodeBinaryOverride(t *testing.T) {
	got, err := findOpenCodeBinary("/custom/opencode")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/opencode" {
		t.Fatalf("got %q", got)
	}
}

func TestFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("bad port %d", p)
	}
}

func TestRandomPasswordUnique(t *testing.T) {
	a, b := randomPassword(), randomPassword()
	if a == "" || b == "" {
		t.Fatal("empty password")
	}
	if a == b {
		t.Fatal("expected different passwords")
	}
}

// 自动生成的 OpenCode 内联配置：必须是合法 JSON，且含 orchestrator 白名单。
func TestOpenCodeInlineConfig(t *testing.T) {
	s := opencodeInlineConfig(&Config{ToolCalling: true})
	if s == "" {
		t.Fatal("empty config")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, s)
	}
	for _, want := range []string{`"orchestrator"`, `"effect":"deny"`, `"execute"`, `"ob-*"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

// 禁用复用 + 禁用拉起时，必须报错而不是去发现/拉起。
func TestEnsureUpstreamNoReuseNoSpawn(t *testing.T) {
	if _, err := EnsureUpstream(context.Background(), NewLogger("error"),
		&Config{ReuseExternal: false}, false); err == nil {
		t.Fatal("expected error")
	}
}

// 本桥拉起的 argv 必须能被 /proc 发现逻辑识别。
func TestSpawnedArgvDiscoverable(t *testing.T) {
	args := []string{"/root/.opencode/bin/opencode", "serve", "--hostname", "127.0.0.1", "--port", "12345"}
	if !isOpenCodeServe(args) {
		t.Fatal("spawned argv not recognized as opencode serve")
	}
	if got := flagValue(args, "--port"); got != "12345" {
		t.Fatalf("port = %q", got)
	}
}
