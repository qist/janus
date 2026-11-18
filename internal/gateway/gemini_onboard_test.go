package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGeminiOnboardsWhenNoProject covers the fresh-account path: Code Assist
// returns no project, so the adapter must create one before generating.
func TestGeminiOnboardsWhenNoProject(t *testing.T) {
	var onboards, generated int
	var onboardBody map[string]any
	var project string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			io.WriteString(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/v1internal:onboardUser":
			onboards++
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &onboardBody)
			io.WriteString(w, `{"done":true,"response":{"cloudaicompanionProject":"proj-free"}}`)
		case "/v1internal:generateContent":
			generated++
			raw, _ := io.ReadAll(r.Body)
			var envelope map[string]any
			_ = json.Unmarshal(raw, &envelope)
			project, _ = envelope["project"].(string)
			io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL, ProtocolGemini)
	client.APIKey = "google-token"

	body := chatBody(t, map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, _, err := client.Chat(context.Background(), "gemini-2.5-pro", body); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if onboards != 1 {
		t.Fatalf("onboard calls = %d", onboards)
	}
	if onboardBody["tierId"] != "free-tier" {
		t.Fatalf("tierId = %v", onboardBody["tierId"])
	}
	metadata, _ := onboardBody["metadata"].(map[string]any)
	if metadata["pluginType"] != "GEMINI" {
		t.Fatalf("metadata = %v", metadata)
	}
	if project != "proj-free" {
		t.Fatalf("generation project = %q", project)
	}
	if generated != 1 {
		t.Fatalf("generate calls = %d", generated)
	}
}
