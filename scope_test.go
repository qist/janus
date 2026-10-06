package main

import "testing"

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
	// Workspace Folder 兜底
	s.cfg.Project = ""
	msgs := []ChatMessage{{Role: "user", Content: MessageContent{Text: "<user_info> Workspace Folder: /home/u/tvfusion\nOS: linux"}}}
	if got := s.projectIdentity("", msgs); got != "/home/u/tvfusion" {
		t.Errorf("workspace folder: got %q", got)
	}
	// 都没有 → default
	if got := s.projectIdentity("", nil); got != "default" {
		t.Errorf("default: got %q", got)
	}
}
