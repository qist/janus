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
	"time"
)

func TestGeminiNonStreamAndProjectCache(t *testing.T) {
	var loads atomic.Int32
	var generation map[string]any
	var messages []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			loads.Add(1)
			io.WriteString(w, `{"cloudaicompanionProject":"proj-123"}`)
		case "/v1internal:generateContent":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &generation)
			request := generation["request"].(map[string]any)
			if contents, ok := request["contents"].([]any); ok {
				for _, item := range contents {
					messages = append(messages, item.(map[string]any))
				}
			}
			io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[
				{"text":"from gemini"},
				{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}
			]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4,"cachedContentTokenCount":2}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL, ProtocolGemini)
	client.APIKey = "google-token"

	body := chatBody(t, map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "read a.go"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "read_file", "arguments": `{"path":"a.go"}`,
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "package a"},
		},
		"max_tokens": 256,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "parameters": map[string]any{"type": "object"},
		}}},
	})

	raw, used, err := client.Chat(context.Background(), "gemini-2.5-pro", body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Envelope: model + project + request wrapper.
	if generation["model"] != "gemini-2.5-pro" || generation["project"] != "proj-123" {
		t.Fatalf("envelope = %v", generation)
	}
	request := generation["request"].(map[string]any)
	if request["systemInstruction"] == nil {
		t.Fatalf("systemInstruction missing: %v", request)
	}
	if request["generationConfig"].(map[string]any)["maxOutputTokens"] != float64(256) {
		t.Fatalf("generationConfig = %v", request["generationConfig"])
	}

	// Roles and tool shapes.
	if len(messages) != 3 {
		t.Fatalf("contents = %v", messages)
	}
	if messages[0]["role"] != "user" {
		t.Fatalf("first content = %v", messages[0])
	}
	assistant := messages[1]
	if assistant["role"] != "model" {
		t.Fatalf("assistant content = %v", assistant)
	}
	parts := assistant["parts"].([]any)
	if parts[0].(map[string]any)["functionCall"] == nil {
		t.Fatalf("functionCall missing: %v", parts)
	}
	toolResult := messages[2]
	if toolResult["role"] != "user" {
		t.Fatalf("tool result role = %v", toolResult["role"])
	}
	responsePart := toolResult["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if responsePart["name"] != "read_file" {
		t.Fatalf("functionResponse = %v", responsePart)
	}

	// Response conversion.
	var completion map[string]any
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "from gemini" {
		t.Fatalf("content = %v", message["content"])
	}
	if len(message["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_calls = %v", message["tool_calls"])
	}
	if used.PromptTokens != 9 || used.CompletionTokens != 4 || used.CachedTokens != 2 {
		t.Fatalf("usage = %+v", used)
	}

	// A second call reuses the cached project.
	if _, _, err := client.Chat(context.Background(), "gemini-2.5-pro", body); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if loads.Load() != 1 {
		t.Fatalf("loadCodeAssist calls = %d", loads.Load())
	}
}

func TestGeminiStreamTranslation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("alt = %q", r.URL.Query().Get("alt"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"he"}]}}]}}` + "\n\n",
			`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"llo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}}` + "\n\n",
		} {
			io.WriteString(w, frame)
		}
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL, ProtocolGemini)
	client.APIKey = "google-token"
	client.projects = map[string]cachedProject{"": {project: "proj", expires: timeNowPlusHour()}}

	var out bytes.Buffer
	body := chatBody(t, map[string]any{
		"model":    "gemini-2.5-flash",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	used, err := client.ChatStream(context.Background(), "gemini-2.5-flash", body, &out)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if used.PromptTokens != 3 || used.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", used)
	}
	stream := out.String()
	if !strings.Contains(stream, `"content":"he"`) || !strings.Contains(stream, `"content":"llo"`) {
		t.Fatalf("stream = %s", stream)
	}
	if !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("missing terminator:\n%s", stream)
	}
	if strings.Contains(stream, "streamGenerateContent") {
		t.Fatalf("upstream payload leaked: %s", stream)
	}
}

func timeNowPlusHour() time.Time {
	return time.Now().Add(time.Hour)
}
