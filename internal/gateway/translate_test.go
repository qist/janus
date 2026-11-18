package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// staticSource hands out one fixed credential, so header shaping can be tested
// without a store or a pool.
type staticSource struct {
	credential Credential
}

func (s staticSource) Next(context.Context, string, string) (Credential, error) {
	return s.credential, nil
}

func (s staticSource) Report(context.Context, string, int, string, time.Duration) {}

func newProtocolClient(t *testing.T, base string, protocol Protocol) *Client {
	t.Helper()
	client, err := New(Config{
		Name:     "upstream",
		BaseURL:  base,
		Model:    "claude-sonnet-4-5",
		Protocol: string(protocol),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func chatBody(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

func TestAnthropicRequestTranslation(t *testing.T) {
	var seen map[string]any
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &seen); err != nil {
			t.Errorf("decode request: %v", err)
		}
		io.WriteString(w, `{
			"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"tool_use",
			"content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.go"}}],
			"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3}
		}`)
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL, ProtocolAnthropic)
	client.Credentials = staticSource{credential: Credential{ID: "cred_1", APIKey: "sk-ant"}}

	body := chatBody(t, map[string]any{
		"model": "fast",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "read a.go"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "read_file", "arguments": `{"path":"a.go"}`,
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "package a"},
		},
		"max_tokens": 256,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "description": "read a file",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}}},
		"tool_choice": "required",
	})

	raw, used, err := client.Chat(context.Background(), "claude-sonnet-4-5", body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Headers: API keys use x-api-key, never a bearer token.
	if headers.Get("x-api-key") != "sk-ant" {
		t.Fatalf("x-api-key = %q", headers.Get("x-api-key"))
	}
	if headers.Get("Authorization") != "" {
		t.Fatalf("unexpected bearer header: %q", headers.Get("Authorization"))
	}
	if headers.Get("anthropic-version") != anthropicVersion {
		t.Fatalf("anthropic-version = %q", headers.Get("anthropic-version"))
	}

	if seen["system"] != "be brief" {
		t.Fatalf("system = %v", seen["system"])
	}
	if seen["max_tokens"] != float64(256) {
		t.Fatalf("max_tokens = %v", seen["max_tokens"])
	}
	messages := seen["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v", messages)
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first role = %v", first["role"])
	}
	if messages[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("second message = %v", messages[1])
	}
	third := messages[2].(map[string]any)
	if third["role"] != "user" {
		t.Fatalf("third role = %v", third["role"])
	}
	blocks := third["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("third content = %v", blocks)
	}
	if seen["tool_choice"].(map[string]any)["type"] != "any" {
		t.Fatalf("tool_choice = %v", seen["tool_choice"])
	}

	// The response becomes a chat completion with a tool_call.
	var completion map[string]any
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "checking" {
		t.Fatalf("content = %v", message["content"])
	}
	calls := message["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "toolu_1" {
		t.Fatalf("tool_calls = %v", calls)
	}
	if used.PromptTokens != 11 || used.CompletionTokens != 7 || used.CachedTokens != 3 {
		t.Fatalf("usage = %+v", used)
	}
}

func TestAnthropicOAuthHeaders(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		io.WriteString(w, `{"id":"msg_2","model":"claude","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL+"/v1", ProtocolAnthropic)
	client.Credentials = staticSource{credential: Credential{ID: "cred_2", APIKey: "oauth-token", OAuth: true}}

	body := chatBody(t, map[string]any{
		"model":    "claude",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, _, err := client.Chat(context.Background(), "claude", body); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if headers.Get("Authorization") != "Bearer oauth-token" {
		t.Fatalf("authorization = %q", headers.Get("Authorization"))
	}
	if headers.Get("x-api-key") != "" {
		t.Fatalf("oauth token sent as x-api-key: %q", headers.Get("x-api-key"))
	}
	if !strings.Contains(headers.Get("anthropic-beta"), "oauth") {
		t.Fatalf("anthropic-beta = %q", headers.Get("anthropic-beta"))
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		frames := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_3","model":"claude-sonnet-4-5","usage":{"input_tokens":9,"cache_read_input_tokens":2}}}

`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

`,
			`event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"write_file"}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"b.go\"}"}}

`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

`,
			`event: message_stop
data: {"type":"message_stop"}

`,
		}
		for _, frame := range frames {
			io.WriteString(w, frame)
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL, ProtocolAnthropic)
	client.Credentials = staticSource{credential: Credential{ID: "cred_3", APIKey: "sk-ant"}}

	var out bytes.Buffer
	body := chatBody(t, map[string]any{
		"model":    "claude",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	used, err := client.ChatStream(context.Background(), "claude", body, &out)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if used.PromptTokens != 9 || used.CompletionTokens != 5 || used.CachedTokens != 2 {
		t.Fatalf("usage = %+v", used)
	}

	text := out.String()
	if !strings.HasSuffix(strings.TrimSpace(text), "data: [DONE]") {
		t.Fatalf("stream not terminated:\n%s", text)
	}
	chunks := parseChunks(t, text)
	content := ""
	var args string
	finish := ""
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if delta, ok := choice.Delta["content"].(string); ok {
				content += delta
			}
			if calls, ok := choice.Delta["tool_calls"].([]any); ok {
				for _, raw := range calls {
					call := raw.(map[string]any)
					if function, ok := call["function"].(map[string]any); ok {
						if value, ok := function["arguments"].(string); ok {
							args += value
						}
					}
				}
			}
			if choice.FinishReason != nil {
				finish, _ = choice.FinishReason.(string)
			}
		}
	}
	if content != "Hello" {
		t.Fatalf("content = %q", content)
	}
	if args != `{"path":"b.go"}` {
		t.Fatalf("tool arguments = %q", args)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestResponsesRequestAndAggregation(t *testing.T) {
	var seen map[string]any
	var headers http.Header
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"done"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"cmd\":\"ls\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5-codex","status":"completed","usage":{"input_tokens":12,"output_tokens":4,"input_tokens_details":{"cached_tokens":6}}}}

`)
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL+"/backend-api/codex", ProtocolResponses)
	accountID := jwtToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-42"},
	})
	client.Credentials = staticSource{credential: Credential{ID: "cred_9", APIKey: accountID, OAuth: true}}

	body := chatBody(t, map[string]any{
		"model": "fast",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "list files"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "shell", "parameters": map[string]any{"type": "object"},
		}}},
		"max_tokens": 512,
	})

	raw, used, err := client.Chat(context.Background(), "gpt-5-codex", body)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if path != "/backend-api/codex/responses" {
		t.Fatalf("path = %q", path)
	}
	if seen["stream"] != true || seen["store"] != false {
		t.Fatalf("stream/store = %v/%v", seen["stream"], seen["store"])
	}
	if seen["instructions"] != "be brief" {
		t.Fatalf("instructions = %v", seen["instructions"])
	}
	if seen["max_output_tokens"] != float64(512) {
		t.Fatalf("max_output_tokens = %v", seen["max_output_tokens"])
	}
	input := seen["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %v", input)
	}
	tools := seen["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "shell" {
		t.Fatalf("tools = %v", tools)
	}
	if headers.Get("chatgpt-account-id") != "acct-42" {
		t.Fatalf("chatgpt-account-id = %q", headers.Get("chatgpt-account-id"))
	}
	if !strings.Contains(headers.Get("OpenAI-Beta"), "responses") {
		t.Fatalf("OpenAI-Beta = %q", headers.Get("OpenAI-Beta"))
	}

	var completion map[string]any
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("content = %v", message["content"])
	}
	calls := message["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool_calls = %v", calls)
	}
	if used.PromptTokens != 12 || used.CompletionTokens != 4 || used.CachedTokens != 6 {
		t.Fatalf("usage = %+v", used)
	}
}

func TestResponsesStreamTranslation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_2","model":"gpt-5-codex"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5-codex","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}

`)
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL+"/v1", ProtocolResponses)
	var out bytes.Buffer
	body := chatBody(t, map[string]any{
		"model":    "gpt-5-codex",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	used, err := client.ChatStream(context.Background(), "gpt-5-codex", body, &out)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if used.PromptTokens != 3 || used.CompletionTokens != 1 {
		t.Fatalf("usage = %+v", used)
	}
	chunks := parseChunks(t, out.String())
	content := ""
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if delta, ok := choice.Delta["content"].(string); ok {
				content += delta
			}
		}
	}
	if content != "hi" {
		t.Fatalf("content = %q", content)
	}
	if !strings.Contains(out.String(), "data: [DONE]") {
		t.Fatalf("missing terminator:\n%s", out.String())
	}
}

func TestAdapterFailoverOnQuota(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"insufficient quota"}}`)
			return
		}
		io.WriteString(w, `{"id":"msg_ok","model":"claude","content":[{"type":"text","text":"second account"}],"usage":{"input_tokens":2,"output_tokens":2}}`)
	}))
	defer server.Close()

	client := newProtocolClient(t, server.URL, ProtocolAnthropic)
	pool := &rotatingSource{credentials: []Credential{
		{ID: "cred_a", APIKey: "key-a"},
		{ID: "cred_b", APIKey: "key-b"},
	}}
	client.Credentials = pool

	body := chatBody(t, map[string]any{
		"model":    "claude",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, used, err := client.Chat(context.Background(), "claude", body); err != nil {
		t.Fatalf("Chat: %v", err)
	} else if used.CredentialID != "cred_b" {
		t.Fatalf("credential = %q", used.CredentialID)
	}
	if len(pool.reported) == 0 || pool.reported[0] != 429 {
		t.Fatalf("reports = %v", pool.reported)
	}
}

type rotatingSource struct {
	credentials []Credential
	next        int
	reported    []int
}

func (s *rotatingSource) Next(context.Context, string, string) (Credential, error) {
	credential := s.credentials[s.next%len(s.credentials)]
	s.next++
	return credential, nil
}

func (s *rotatingSource) Report(_ context.Context, id string, status int, _ string, _ time.Duration) {
	if status != 0 {
		s.reported = append(s.reported, status)
	}
}

type chunkChoice struct {
	Delta        map[string]any
	FinishReason any
}

type chunk struct {
	Choices []chunkChoice
}

func parseChunks(t *testing.T, stream string) []chunk {
	t.Helper()
	var out []chunk
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var decoded struct {
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason any            `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		item := chunk{}
		for _, choice := range decoded.Choices {
			item.Choices = append(item.Choices, chunkChoice{Delta: choice.Delta, FinishReason: choice.FinishReason})
		}
		out = append(out, item)
	}
	return out
}

func jwtToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestAPIEndpointShapes(t *testing.T) {
	cases := []struct {
		base     string
		resource string
		version  string
		want     string
	}{
		{"https://api.openai.com", "responses", "v1", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1", "responses", "v1", "https://api.openai.com/v1/responses"},
		{"https://chatgpt.com/backend-api/codex", "responses", "v1", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://api.anthropic.com", "messages", "v1", "https://api.anthropic.com/v1/messages"},
		{"https://api.deepseek.com/anthropic", "messages", "v1", "https://api.deepseek.com/anthropic/v1/messages"},
		{"https://api.anthropic.com/v1/", "messages", "v1", "https://api.anthropic.com/v1/messages"},
	}
	for _, tc := range cases {
		if got := apiEndpoint(tc.base, tc.resource, tc.version); got != tc.want {
			t.Fatalf("%s + %s = %s, want %s", tc.base, tc.resource, got, tc.want)
		}
	}
}

func TestParseProtocolRejectsUnknown(t *testing.T) {
	if _, ok := ParseProtocol("grpc"); ok {
		t.Fatal("accepted an unknown protocol")
	}
	if protocol, ok := ParseProtocol(""); !ok || protocol != ProtocolChat {
		t.Fatalf("empty protocol = %v/%v", protocol, ok)
	}
	if protocol, ok := ParseProtocol(" Responses "); !ok || protocol != ProtocolResponses {
		t.Fatalf("responses protocol = %v/%v", protocol, ok)
	}
}
