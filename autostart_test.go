package main

import "testing"

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
