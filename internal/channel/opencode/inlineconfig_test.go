package opencode

import (
	"encoding/json"
	"testing"

	"janus/internal/config"
)

func decodeInline(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, s)
	}
	return m
}

// 未开启注入时，内联配置里不应出现 provider（保持既有行为）。
func TestInlineConfigNoProviderByDefault(t *testing.T) {
	m := decodeInline(t, InlineConfig(&config.Config{}, "ob-"))
	if _, ok := m["provider"]; ok {
		t.Fatalf("provider injected without opt-in: %s", m)
	}
}

// 开启注入且给了 key 时，注入 janus provider（baseURL 带 /v1、模型清单完整）。
func TestInlineConfigInjectsJanusProvider(t *testing.T) {
	cfg := &config.Config{
		OpencodeProviderInject:  true,
		OpencodeProviderBaseURL: "http://127.0.0.1:2810",
		OpencodeProviderAPIKey:  "sk-agent",
		OpencodeProviderModels:  []string{"opencode-go/mimo-v2.5-pro", " vendor/cheap ", ""},
	}
	m := decodeInline(t, InlineConfig(cfg, "ob-"))

	prov, ok := m["provider"].(map[string]any)
	if !ok {
		t.Fatalf("no provider block: %s", m)
	}
	janus, ok := prov["janus"].(map[string]any)
	if !ok {
		t.Fatalf("no janus provider: %v", prov)
	}
	if janus["npm"] != "@ai-sdk/openai-compatible" {
		t.Fatalf("npm = %v", janus["npm"])
	}
	opts, _ := janus["options"].(map[string]any)
	if opts["baseURL"] != "http://127.0.0.1:2810/v1" {
		t.Fatalf("baseURL = %v", opts["baseURL"])
	}
	if opts["apiKey"] != "sk-agent" {
		t.Fatalf("apiKey = %v", opts["apiKey"])
	}
	models, _ := janus["models"].(map[string]any)
	if len(models) != 2 {
		t.Fatalf("models = %v", models)
	}
	if _, ok := models["opencode-go/mimo-v2.5-pro"]; !ok {
		t.Fatalf("model list = %v", models)
	}
	if _, ok := models["vendor/cheap"]; !ok {
		t.Fatalf("model list should be trimmed = %v", models)
	}

	// agents 仍然要在（注入 provider 不能顶掉 orchestrator）。
	if _, ok := m["agents"]; !ok {
		t.Fatalf("agents missing after provider injection: %s", m)
	}
}

// 缺 key 或未开启时都不注入。
func TestJanusProviderConfigGuards(t *testing.T) {
	if got := JanusProviderConfig(nil); got != nil {
		t.Fatalf("nil config = %v", got)
	}
	if got := JanusProviderConfig(&config.Config{OpencodeProviderInject: true}); got != nil {
		t.Fatalf("missing key should skip injection: %v", got)
	}
	got := JanusProviderConfig(&config.Config{
		OpencodeProviderInject:  true,
		OpencodeProviderAPIKey:  "k",
		OpencodeProviderBaseURL: "http://x:1/v1",
	})
	opts, _ := got["options"].(map[string]any)
	if opts["baseURL"] != "http://x:1/v1" {
		t.Fatalf("baseURL should not double-append /v1: %v", opts["baseURL"])
	}
}

func TestDeriveJanusBaseURL(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:2810":   "http://127.0.0.1:2810",
		":2810":          "http://127.0.0.1:2810",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"[::]:2810":      "http://127.0.0.1:2810",
		"0.0.0.0":        "http://127.0.0.1:2810",
		"":               "",
	}
	for in, want := range cases {
		if got := DeriveJanusBaseURL(in); got != want {
			t.Fatalf("DeriveJanusBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
