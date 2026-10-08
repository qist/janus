package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeIDE(t *testing.T) {
	cases := map[string]string{
		"hertz":                                "trae",
		"CodeBuddyIDE/4.12.1 CodeBuddy/4.12.1": "codebuddy",
		"":                                     "unknown",
		"SomeIDE/1.0":                          "someide",
	}
	for in, want := range cases {
		if got := normalizeIDE(in); got != want {
			t.Errorf("normalizeIDE(%q)=%q want %q", in, got, want)
		}
	}
}

func TestScopeKeyStableAndDistinct(t *testing.T) {
	a := scopeKey("codebuddy", "qist/tvfusion")
	b := scopeKey("codebuddy", "qist/tvfusion")
	c := scopeKey("trae", "qist/tvfusion")
	if a != b {
		t.Fatalf("同输入应稳定: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("不同 IDE 应不同: %s", a)
	}
	if a[:2] != "s:" {
		t.Fatalf("scope key 前缀应为 s: : %s", a)
	}
}

func TestExtractProjectRoot(t *testing.T) {
	// Trae：Primary working directory 优先，忽略系统/Trae 自身路径
	trae := []ChatMessage{
		{Role: "user", Content: MessageContent{Text: "<system-reminder>\n# Environment\n- Primary working directory: /opt/tvgate\n"}},
		{Role: "tool", Content: MessageContent{Text: "read /usr/local/x /root/.trae-cn/y"}},
	}
	if got := extractProjectRoot(trae); got != "/opt/tvgate" {
		t.Fatalf("trae got %q want /opt/tvgate", got)
	}
	// Copilot Chat：workspace folders
	cop := []ChatMessage{
		{Role: "user", Content: MessageContent{Text: "<workspace_info> I am working in a workspace with the following folders: - /opt/sqlite I am working in a workspace that has the following structure:"}},
	}
	if got := extractProjectRoot(cop); got != "/opt/sqlite" {
		t.Fatalf("copilot got %q want /opt/sqlite", got)
	}
	// 兜底：最频繁路径（排除系统目录）
	msgs := []ChatMessage{
		{Role: "tool", Content: MessageContent{Text: "read /opt/tvfusion/a.go and /opt/tvfusion/b.go"}},
		{Role: "assistant", Content: MessageContent{Text: "cd /opt/tvfusion/x"}},
	}
	if got := extractProjectRoot(msgs); got != "/opt/tvfusion" {
		t.Fatalf("got %q want /opt/tvfusion", got)
	}
	if got := extractProjectRoot(nil); got != "" {
		t.Fatalf("empty got %q", got)
	}
}

func TestProjectIdentityPriority(t *testing.T) {
	s := &Server{cfg: Config{Project: "", ProjectMap: map[string]string{`D:\proj\tvfusion`: "qist/tvfusion"}}}
	// map 命中（Windows 路径归一）
	if got := s.projectIdentity(`D:\proj\tvfusion`, nil); got != "qist/tvfusion" {
		t.Errorf("map: got %q", got)
	}
	// 显式 BRIDGE_PROJECT 最高
	s.cfg.Project = "explicit"
	if got := s.projectIdentity("/whatever", nil); got != "explicit" {
		t.Errorf("explicit: got %q", got)
	}
	// Workspace Folder 兜底 → 取 basename
	s.cfg.Project = ""
	msgs := []ChatMessage{{Role: "user", Content: MessageContent{Text: "<user_info> Workspace Folder: /home/u/tvfusion\nOS: linux"}}}
	if got := s.projectIdentity("", msgs); got != "tvfusion" {
		t.Errorf("workspace folder: got %q", got)
	}
	// 都没有 → default
	if got := s.projectIdentity("", nil); got != "default" {
		t.Errorf("default: got %q", got)
	}
}

// TestSessionDir 验证远程 + mode B 的会话目录选择链：
// header（存在才用）> 消息抽取的项目根（存在才用）> per-scope 中性目录。
func TestSessionDir(t *testing.T) {
	ws := t.TempDir()
	s := &Server{cfg: Config{WorkspacesDir: ws, Directory: "/deploy"}}
	sk := scopeKey("testide", "testproj")
	neutral := filepath.Join(ws, strings.TrimPrefix(sk, scopeKeyPrefix))

	// 1. header 目录在 janus 主机上真实存在 → 直接用
	exists := t.TempDir()
	if got := s.sessionDir(exists, nil, sk); got != exists {
		t.Fatalf("existing header dir: got %q want %q", got, exists)
	}

	// 2. header 是客户端侧路径、janus 上不存在（远程 + mode B，如 Windows 路径）
	//    → 不直接使用，落到 per-scope 中性目录，且目录被创建
	got := s.sessionDir(`C:\Users\dev\proj`, nil, sk)
	if got != neutral {
		t.Fatalf("missing header dir: got %q want %q", got, neutral)
	}
	if fi, err := os.Stat(neutral); err != nil || !fi.IsDir() {
		t.Fatalf("neutral dir not created: %v", err)
	}

	// 3. header 为空，消息抽到真实存在的项目根 → 用它
	msgs := []ChatMessage{{Role: "user", Content: MessageContent{Text: "Primary working directory: " + exists}}}
	if got := s.sessionDir("", msgs, sk); got != exists {
		t.Fatalf("msg-extracted dir: got %q want %q", got, exists)
	}

	// 4. header 为空且消息里抽不到存在的路径 → 中性目录
	if got := s.sessionDir("", nil, sk); got != neutral {
		t.Fatalf("fallback dir: got %q want %q", got, neutral)
	}

	// 5. header 是客户端侧路径、但消息里能抽到真实存在的目录 → 用消息抽取的
	if got := s.sessionDir(`/nonexistent-on-janus`, msgs, sk); got != exists {
		t.Fatalf("header missing + msg found: got %q want %q", got, exists)
	}
}