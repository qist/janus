package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// antigravityServer mimics the v1internal endpoints: metadata calls plus one
// generation call. loadProject is empty to force the onboarding path.
func antigravityServer(t *testing.T, loadProject string) (*httptest.Server, *antigravityCapture) {
	t.Helper()
	capture := &antigravityCapture{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			capture.loads.Add(1)
			capture.loadUA = r.Header.Get("User-Agent")
			capture.loadBody = readBody(r)
			if loadProject == "" {
				io.WriteString(w, `{"currentTier":{"id":"free-tier"}}`)
				return
			}
			io.WriteString(w, `{"cloudaicompanionProject":"`+loadProject+`","currentTier":{"id":"free-tier"}}`)
		case "/v1internal:onboardUser":
			capture.onboards.Add(1)
			capture.onboardBody = readBody(r)
			io.WriteString(w, `{"done":true,"response":{"cloudaicompanionProject":"proj-onboarded"}}`)
		case "/v1internal:generateContent":
			capture.path = r.URL.Path
			capture.ua = r.Header.Get("User-Agent")
			capture.auth = r.Header.Get("Authorization")
			_ = json.Unmarshal(readBody(r), &capture.envelope)
			io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"from antigravity"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}}`)
		case "/v1internal:streamGenerateContent":
			capture.path = r.URL.Path
			capture.ua = r.Header.Get("User-Agent")
			_ = json.Unmarshal(readBody(r), &capture.envelope)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"streamed"}]}}]}}`+"\n\n")
			io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}}`+"\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, capture
}

type antigravityCapture struct {
	loads       atomic.Int32
	onboards    atomic.Int32
	loadUA      string
	loadBody    []byte
	onboardBody []byte
	path        string
	ua          string
	auth        string
	envelope    map[string]any
}

func readBody(r *http.Request) []byte {
	raw, _ := io.ReadAll(r.Body)
	return raw
}

func TestAntigravityEnvelopeAndOnboarding(t *testing.T) {
	// No project from loadCodeAssist: the adapter must onboard once.
	server, capture := antigravityServer(t, "")

	client := newProtocolClient(t, server.URL, ProtocolAntigravity)
	client.Credentials = staticSource{credential: Credential{ID: "cred_ag", APIKey: "google-token", OAuth: true}}

	body := chatBody(t, map[string]any{
		"model": "claude-sonnet-4-5",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "parameters": map[string]any{"type": "object"},
		}}},
		"max_tokens": 512,
	})

	raw, used, err := client.Chat(context.Background(), "claude-sonnet-4-5", body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if capture.path != "/v1internal:generateContent" {
		t.Fatalf("path = %q", capture.path)
	}
	if capture.auth != "Bearer google-token" {
		t.Fatalf("authorization = %q", capture.auth)
	}
	if !strings.HasPrefix(capture.ua, "antigravity/hub/") {
		t.Fatalf("user agent = %q", capture.ua)
	}
	if capture.loadUA != capture.ua {
		t.Fatalf("metadata call UA = %q, generation UA = %q", capture.loadUA, capture.ua)
	}

	// Envelope metadata is what separates this dialect from plain Code Assist.
	if capture.envelope["userAgent"] != "antigravity" || capture.envelope["requestType"] != "agent" {
		t.Fatalf("envelope = %v", capture.envelope)
	}
	requestID, _ := capture.envelope["requestId"].(string)
	if !strings.HasPrefix(requestID, "agent-") || len(requestID) < 12 {
		t.Fatalf("requestId = %q", requestID)
	}
	if capture.envelope["project"] != "proj-onboarded" {
		t.Fatalf("project = %v", capture.envelope["project"])
	}
	request := capture.envelope["request"].(map[string]any)
	if request["systemInstruction"] == nil {
		t.Fatalf("systemInstruction missing: %v", request)
	}
	if request["generationConfig"].(map[string]any)["maxOutputTokens"] != float64(512) {
		t.Fatalf("generationConfig = %v", request["generationConfig"])
	}
	mode := request["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]
	if mode != "VALIDATED" {
		t.Fatalf("toolConfig mode = %v", mode)
	}

	// Onboarding used the tier reported by loadCodeAssist.
	var onboard map[string]any
	if err := json.Unmarshal(capture.onboardBody, &onboard); err != nil {
		t.Fatalf("decode onboard body: %v", err)
	}
	if onboard["tierId"] != "free-tier" {
		t.Fatalf("tierId = %v", onboard["tierId"])
	}
	metadata := onboard["metadata"].(map[string]any)
	if metadata["ideType"] != "ANTIGRAVITY" || metadata["pluginType"] != "GEMINI" {
		t.Fatalf("metadata = %v", metadata)
	}
	if capture.loads.Load() != 1 || capture.onboards.Load() != 1 {
		t.Fatalf("loads = %d, onboards = %d", capture.loads.Load(), capture.onboards.Load())
	}

	var completion map[string]any
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "from antigravity" {
		t.Fatalf("content = %v", message["content"])
	}
	if used.PromptTokens != 7 || used.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", used)
	}

	// A second call reuses the cached project: no more metadata round trips.
	if _, _, err := client.Chat(context.Background(), "claude-sonnet-4-5", body); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if capture.loads.Load() != 1 || capture.onboards.Load() != 1 {
		t.Fatalf("project lookup repeated: loads=%d onboards=%d", capture.loads.Load(), capture.onboards.Load())
	}
}

func TestAntigravityUsesProjectFromLoadCodeAssist(t *testing.T) {
	server, capture := antigravityServer(t, "proj-ready")

	client := newProtocolClient(t, server.URL, ProtocolAntigravity)
	client.APIKey = "google-token"

	body := chatBody(t, map[string]any{
		"model":    "gemini-3-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, _, err := client.Chat(context.Background(), "gemini-3-pro", body); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if capture.envelope["project"] != "proj-ready" {
		t.Fatalf("project = %v", capture.envelope["project"])
	}
	if capture.onboards.Load() != 0 {
		t.Fatalf("onboard called for an account that already has a project")
	}
	// No tools means no toolConfig: an empty VALIDATED block would change
	// behaviour for a request that never asked for tools.
	request := capture.envelope["request"].(map[string]any)
	if _, ok := request["toolConfig"]; ok {
		t.Fatalf("unexpected toolConfig: %v", request["toolConfig"])
	}
}

func TestAntigravityStreamCarriesEnvelope(t *testing.T) {
	server, capture := antigravityServer(t, "proj-ready")

	client := newProtocolClient(t, server.URL, ProtocolAntigravity)
	client.APIKey = "google-token"

	var out bytes.Buffer
	body := chatBody(t, map[string]any{
		"model":    "gemini-3-pro",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	used, err := client.ChatStream(context.Background(), "gemini-3-pro", body, &out)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if capture.path != "/v1internal:streamGenerateContent" {
		t.Fatalf("path = %q", capture.path)
	}
	if capture.envelope["userAgent"] != "antigravity" || capture.envelope["requestType"] != "agent" {
		t.Fatalf("stream envelope = %v", capture.envelope)
	}
	if capture.envelope["project"] != "proj-ready" {
		t.Fatalf("project = %v", capture.envelope["project"])
	}
	stream := out.String()
	if !strings.Contains(stream, `"content":"streamed"`) || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("stream = %s", stream)
	}
	if used.PromptTokens != 2 || used.CompletionTokens != 1 {
		t.Fatalf("usage = %+v", used)
	}
}
