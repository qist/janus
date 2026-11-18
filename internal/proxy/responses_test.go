package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"janus/internal/auth"
	"janus/internal/gateway"
	"janus/internal/usage"
)

func responsesPost(t *testing.T, srv *httptest.Server, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func responsesEnv(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *usage.MemoryLedger) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	client, err := gateway.New(gateway.Config{
		Name: "test", BaseURL: up.URL + "/v1", APIKey: upstream, Model: "real-model",
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "agent-chat", Scopes: []auth.Scope{auth.ScopeChat}})
	ledger := usage.NewMemoryLedger(nil)
	srv := httptest.NewServer(New(map[string]*gateway.Client{"fast": client}, keys, ledger).Handler())
	t.Cleanup(srv.Close)
	return srv, ledger
}

func TestResponsesInboundRequestShape(t *testing.T) {
	var seen map[string]any
	srv, ledger := responsesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		io.WriteString(w, `{"id":"c1","model":"real-model","choices":[{"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	})

	resp := responsesPost(t, srv, chatKey, `{
		"model":"fast",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		],
		"max_output_tokens":128,
		"tools":[{"type":"function","name":"read_file","description":"read","parameters":{"type":"object"}}]
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, readAll(t, resp))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(readAll(t, resp)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["object"] != "response" || !strings.HasPrefix(out["id"].(string), "resp_") {
		t.Fatalf("envelope = %v", out)
	}
	if out["status"] != "completed" {
		t.Fatalf("status = %v", out["status"])
	}
	output := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", output)
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("item = %v", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "hello there" {
		t.Fatalf("content = %v", content)
	}
	used := out["usage"].(map[string]any)
	if used["input_tokens"] != float64(7) || used["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", used)
	}

	// The upstream saw a plain chat request.
	messages := seen["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", messages)
	}
	if messages[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("user turn = %v", messages[1])
	}
	if seen["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %v", seen["max_tokens"])
	}
	tools := seen["tools"].([]any)[0].(map[string]any)
	if tools["type"] != "function" || tools["function"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("tools = %v", tools)
	}

	totals, err := ledger.Totals(t.Context(), usage.Filter{KeyID: "agent-chat"})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Entries != 1 || totals.PromptTokens != 7 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestResponsesInboundToolCall(t *testing.T) {
	srv, _ := responsesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"c2","model":"real-model","choices":[{"message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`)
	})

	resp := responsesPost(t, srv, chatKey, `{
		"model":"fast",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"read it"}]},
			{"type":"function_call","call_id":"call_0","name":"read_file","arguments":"{\"path\":\"x.go\"}"},
			{"type":"function_call_output","call_id":"call_0","output":"package x"}
		]
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, readAll(t, resp))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(readAll(t, resp)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	output := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", output)
	}
	call := output[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "read_file" {
		t.Fatalf("call = %v", call)
	}
	if call["arguments"] != `{"path":"a.go"}` {
		t.Fatalf("arguments = %v", call["arguments"])
	}
}

func TestResponsesInboundHistoryBecomesChat(t *testing.T) {
	var seen map[string]any
	srv, _ := responsesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		io.WriteString(w, `{"id":"c3","model":"real-model","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{}}`)
	})

	resp := responsesPost(t, srv, chatKey, `{
		"model":"fast",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"read a.go"}]},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"package a"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"now summarize"}]}
		]
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, readAll(t, resp))
	}
	_ = readAll(t, resp)

	messages := seen["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %v", messages)
	}
	if messages[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("call turn = %v", messages[1])
	}
	calls := messages[1].(map[string]any)["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool_calls = %v", calls)
	}
	if messages[2].(map[string]any)["role"] != "tool" || messages[2].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("tool result = %v", messages[2])
	}
}

func TestResponsesInboundStream(t *testing.T) {
	srv, _ := responsesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`data: {"id":"c4","model":"real-model","choices":[{"delta":{"content":"he"}}]}` + "\n\n",
			`data: {"id":"c4","model":"real-model","choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}` + "\n\n",
			`data: {"id":"c4","model":"real-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			io.WriteString(w, frame)
		}
	})

	resp := responsesPost(t, srv, chatKey,
		`{"model":"fast","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, readAll(t, resp))
	}
	stream := readAll(t, resp)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		`"delta":"he"`,
		`"delta":"llo"`,
		"event: response.output_item.done",
		"event: response.completed",
		`"output_tokens":2`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("chat terminator leaked into the responses stream:\n%s", stream)
	}
	if strings.Index(stream, "response.created") > strings.Index(stream, "output_text.delta") {
		t.Fatalf("created event is not first:\n%s", stream)
	}
}

func TestResponsesInboundErrors(t *testing.T) {
	srv, _ := responsesEnv(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"c5","model":"real-model","choices":[],"usage":{}}`)
	})

	resp := responsesPost(t, srv, chatKey, `{"model":"missing","input":"hi"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d", resp.StatusCode)
	}
	var failure map[string]any
	_ = json.Unmarshal([]byte(readAll(t, resp)), &failure)
	if failure["error"] == nil {
		t.Fatalf("error envelope = %v", failure)
	}

	resp = responsesPost(t, srv, "wrong", `{"model":"fast","input":"hi"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
}
