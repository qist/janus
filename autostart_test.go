package main

import (
	"context"
	"encoding/json"
	"os"
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

// ---------- opencodeEnv：子进程环境组装 ----------

// clearClientEnv 在测试期间清掉 OPENCODE_CLIENT，保证断言不受执行环境
// （开发机 shell、CI）影响。注意不能用 t.Setenv("", ...)——那会在
// os.Environ() 里留下 "OPENCODE_CLIENT="，让"空值=不设置"误判为已设置。
func clearClientEnv(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("OPENCODE_CLIENT")
	if err := os.Unsetenv("OPENCODE_CLIENT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("OPENCODE_CLIENT", old)
		}
	})
}

// TestOpencodeEnvForwardsClient 锁死：OPENCODE_CLIENT 必须显式转发给子进程。
//
// janus.env 是当配置文件读的（JANUS_CONFIG），内容不进 os.Environ()，
// 所以如果只是把 OPENCODE_CLIENT 写进 janus.env、却不在这里追加，
// 子进程看到的仍是 systemd 的环境（无该变量）→ 上游请求头退回默认 cli。
func TestOpencodeEnvForwardsClient(t *testing.T) {
	clearClientEnv(t)

	env := opencodeEnv(&Config{OpencodeClient: "desktop"}, "pw")
	if !hasEnv(env, "OPENCODE_CLIENT=desktop") {
		t.Fatalf("OPENCODE_CLIENT 未转发给子进程；实际相关项: %v", grepPrefix(env, "OPENCODE_CLIENT"))
	}
	// 只应有一份，避免同名变量两份、子进程取值不定。
	if got := grepPrefix(env, "OPENCODE_CLIENT"); len(got) != 1 {
		t.Errorf("OPENCODE_CLIENT 应恰好一份，实际 %v", got)
	}

	// 空值 = 不设置，交给 OpenCode 自己的默认（cli）。
	for _, v := range []string{"", "   "} {
		env := opencodeEnv(&Config{OpencodeClient: v}, "pw")
		if got := grepPrefix(env, "OPENCODE_CLIENT"); len(got) != 0 {
			t.Errorf("OpencodeClient=%q 时不应设置 OPENCODE_CLIENT，得到 %v", v, got)
		}
	}

	// 托管标记与密码仍在（别把原有转发弄丢了）。
	env = opencodeEnv(&Config{OpencodeClient: "desktop"}, "secret")
	if !hasEnv(env, "OPENCODE_SERVER_PASSWORD=secret") {
		t.Error("OPENCODE_SERVER_PASSWORD 未转发")
	}
	if !hasEnv(env, managedUpstreamEnv+"="+managedUpstreamEnvValue) {
		t.Error("托管标记未转发")
	}
}

// TestOpencodeEnvDedupsInherited 断言继承环境里的同名值被剔除，
// 只保留显式转发的那一份（避免同名两份、子进程取值不定）。
func TestOpencodeEnvDedupsInherited(t *testing.T) {
	t.Setenv("OPENCODE_CLIENT", "cli") // 继承里已有 cli

	env := opencodeEnv(&Config{OpencodeClient: "desktop"}, "pw")
	got := grepPrefix(env, "OPENCODE_CLIENT")
	if len(got) != 1 || got[0] != "OPENCODE_CLIENT=desktop" {
		t.Fatalf("应只保留显式转发的一份，实际 %v", got)
	}
}

func hasEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func grepPrefix(env []string, prefix string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}
