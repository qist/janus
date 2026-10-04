package main

import (
	"context"
	"testing"
)

func TestParseModelMap(t *testing.T) {
	m := parseModelMap(" claude-3-5-sonnet = opencode-go/gpt-6-luna , claude-3-5-haiku=opencode-go/glm-5.3-flash ,bad,=")
	if m["claude-3-5-sonnet"] != "opencode-go/gpt-6-luna" {
		t.Fatalf("sonnet 映射: %+v", m)
	}
	if m["claude-3-5-haiku"] != "opencode-go/glm-5.3-flash" {
		t.Fatalf("haiku 映射: %+v", m)
	}
	if len(m) != 2 {
		t.Fatalf("只应有 2 条: %+v", m)
	}
	if parseModelMap("") == nil || len(parseModelMap("")) != 0 {
		t.Fatal("空串应返回空 map")
	}
}

// resolveModel 必须先过别名映射，再解析。
func TestResolveModelAppliesAlias(t *testing.T) {
	list := []OCModel{{ID: "gpt-6-luna", ProviderID: "opencode-go"}}
	srv := NewServer(Config{ModelMap: map[string]string{
		"claude-3-5-sonnet": "opencode-go/gpt-6-luna",
	}}, NewLogger("error"))

	ref, err := srv.resolveModel(context.Background(), "claude-3-5-sonnet", list)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ProviderID != "opencode-go" || ref.ID != "gpt-6-luna" {
		t.Fatalf("别名未生效: %+v", ref)
	}

	// 大小写不敏感
	if got := srv.mapModelName("CLAUDE-3-5-SONNET"); got != "opencode-go/gpt-6-luna" {
		t.Fatalf("大小写不敏感: %q", got)
	}
	// 无映射原样返回
	if got := srv.mapModelName("opencode-go/other"); got != "opencode-go/other" {
		t.Fatalf("无映射应原样: %q", got)
	}
}

// 前缀通配：claude-opus* 优先于 claude-*（最长前缀胜出）。
func TestMapModelNamePrefix(t *testing.T) {
	srv := NewServer(Config{ModelMap: map[string]string{
		"claude-opus*": "opencode-go/gpt-6-luna",
		"claude-*":     "opencode-go/deepseek-v4.1-flash",
	}}, NewLogger("error"))

	if got := srv.mapModelName("claude-opus-4-1-20250805"); got != "opencode-go/gpt-6-luna" {
		t.Fatalf("opus 前缀: %q", got)
	}
	if got := srv.mapModelName("claude-3-5-haiku-20241022"); got != "opencode-go/deepseek-v4.1-flash" {
		t.Fatalf("haiku 前缀: %q", got)
	}
	if got := srv.mapModelName("claude-sonnet-4-5"); got != "opencode-go/deepseek-v4.1-flash" {
		t.Fatalf("sonnet 前缀: %q", got)
	}
	// 完全不匹配则原样
	if got := srv.mapModelName("deepseek-chat"); got != "deepseek-chat" {
		t.Fatalf("不匹配应原样: %q", got)
	}
}
