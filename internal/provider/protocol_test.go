package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientsDeriveProtocolFromChannel checks the wiring that unit tests in
// gateway cannot: a provider stored with only a channel id must produce a client
// that talks the channel's protocol, using the sealed credential.
func TestClientsDeriveProtocolFromChannel(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)

	var path string
	var apiKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		apiKey = r.Header.Get("x-api-key")
		io.WriteString(w, `{"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"end_turn",
			"content":[{"type":"text","text":"from claude"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "claude", Name: "Claude Code", Kind: KindOpenAI, Channel: "claude-code",
		BaseURL: upstream.URL,
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "claude", "work", CredentialAPIKey, "sk-ant-live"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "fast", ProviderID: "claude", UpstreamModel: "claude-sonnet-4-5",
	}); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	clients, err := svc.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	client, ok := clients["fast"]
	if !ok {
		t.Fatal("route not built")
	}
	if client.Protocol != "anthropic" {
		t.Fatalf("protocol = %q", client.Protocol)
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	raw, used, err := client.Chat(ctx, "claude-sonnet-4-5", body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if path != "/v1/messages" {
		t.Fatalf("path = %q", path)
	}
	if apiKey != "sk-ant-live" {
		t.Fatalf("x-api-key = %q", apiKey)
	}
	if used.PromptTokens != 3 || used.CredentialID == "" {
		t.Fatalf("usage = %+v", used)
	}
	var completion map[string]any
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "from claude" {
		t.Fatalf("content = %v", message["content"])
	}
}

// TestClientsUseGeminiForCodeAssist checks the wiring: the channel picks the
// protocol without a code change, and a chat request reaches generateContent.
func TestClientsUseGeminiForCodeAssist(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)

	var path string
	var envelope map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &envelope)
		io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"from code assist"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3}}}`)
	}))
	t.Cleanup(upstream.Close)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "gemini", Kind: KindCustom, Channel: "google-code-assist",
		BaseURL: upstream.URL,
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "gemini", "google", CredentialOAuth, "google-access-token"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "gemini-pro", ProviderID: "gemini", UpstreamModel: "gemini-2.5-pro",
	}); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	clients, err := svc.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	client := clients["gemini-pro"]
	if client == nil || client.Protocol != "gemini" {
		t.Fatalf("client = %+v", client)
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	raw, used, err := client.Chat(ctx, "gemini-2.5-pro", body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if path != "/v1internal:generateContent" {
		t.Fatalf("path = %q", path)
	}
	if envelope["model"] != "gemini-2.5-pro" {
		t.Fatalf("envelope = %v", envelope)
	}
	if used.PromptTokens != 4 || used.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", used)
	}
	var completion map[string]any
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "from code assist" {
		t.Fatalf("content = %v", message["content"])
	}
}
