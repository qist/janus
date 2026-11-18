package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"janus/internal/auth"
	"janus/internal/gateway"
	"janus/internal/usage"
)

const (
	chatKey  = "chat-key-for-tests"
	readKey  = "read-key-for-tests"
	upstream = "upstream-key"
)

type seen struct {
	mu    sync.Mutex
	model string
	auth  string
}

type env struct {
	srv    *httptest.Server
	ledger *usage.MemoryLedger
	seen   *seen
}

func (e *env) upstreamModel() string {
	e.seen.mu.Lock()
	defer e.seen.mu.Unlock()
	return e.seen.model
}

func (e *env) upstreamAuthorization() string {
	e.seen.mu.Lock()
	defer e.seen.mu.Unlock()
	return e.seen.auth
}

func newEnv(t *testing.T) *env {
	t.Helper()

	recorded := &seen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		model, _ := body["model"].(string)
		recorded.mu.Lock()
		recorded.model = model
		recorded.auth = r.Header.Get("Authorization")
		recorded.mu.Unlock()

		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for _, chunk := range []string{
				`data: {"id":"1","model":"real-model","choices":[{"delta":{"content":"he"}}]}` + "\n\n",
				`data: {"id":"1","model":"real-model","choices":[{"delta":{"content":"llo"}}]}` + "\n\n",
				`data: {"id":"1","model":"real-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n\n",
				"data: [DONE]\n\n",
			} {
				io.WriteString(w, chunk)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","model":"real-model","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	t.Cleanup(up.Close)

	client, err := gateway.New(gateway.Config{
		Name:    "test",
		BaseURL: up.URL + "/v1",
		APIKey:  upstream,
		Model:   "real-model",
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "agent-chat", Scopes: []auth.Scope{auth.ScopeChat, auth.ScopeRead}})
	keys.Add(readKey, auth.Identity{AgentID: "agent-read", Scopes: []auth.Scope{auth.ScopeRead}})

	ledger := usage.NewMemoryLedger(nil)
	server := New(map[string]*gateway.Client{
		"real-model": client,
		"fast":       client,
	}, keys, ledger)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	return &env{srv: ts, ledger: ledger, seen: recorded}
}

func post(t *testing.T, e *env, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	return body.Error.Code
}

func TestAuthRequired(t *testing.T) {
	e := newEnv(t)
	resp := post(t, e, "", `{"model":"fast","messages":[]}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "unauthenticated" {
		t.Fatalf("code = %q", code)
	}
}

func TestScopeRequired(t *testing.T) {
	e := newEnv(t)
	resp := post(t, e, readKey, `{"model":"fast","messages":[]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
}

func TestModelsList(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+chatKey)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "list" || len(body.Data) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if body.Data[0].ID != "fast" || body.Data[1].ID != "real-model" {
		t.Fatalf("models = %+v", body.Data)
	}
}

func TestChatForwardsAndRecordsUsage(t *testing.T) {
	e := newEnv(t)

	resp := post(t, e, chatKey, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	raw := readAll(t, resp)
	if !strings.Contains(raw, `"hello"`) {
		t.Fatalf("body = %s", raw)
	}
	if got := e.upstreamModel(); got != "real-model" {
		t.Fatalf("upstream model = %q, want real-model", got)
	}
	if got := e.upstreamAuthorization(); got != "Bearer "+upstream {
		t.Fatalf("upstream auth = %q", got)
	}

	totals, err := e.ledger.Totals(t.Context(), usage.Filter{KeyID: "agent-chat"})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if totals.Entries != 1 || totals.PromptTokens != 5 || totals.CompletionTokens != 2 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestUnknownModel(t *testing.T) {
	e := newEnv(t)
	resp := post(t, e, chatKey, `{"model":"nope","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "model_not_found" {
		t.Fatalf("code = %q", code)
	}
}

func TestInvalidJSON(t *testing.T) {
	e := newEnv(t)
	resp := post(t, e, chatKey, `{"model":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
}

func TestMissingModel(t *testing.T) {
	e := newEnv(t)
	resp := post(t, e, chatKey, `{"messages":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "model_required" {
		t.Fatalf("code = %q", code)
	}
}

func TestUpstreamErrorMapsToBadGateway(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer up.Close()

	client, _ := gateway.New(gateway.Config{BaseURL: up.URL, Model: "m", APIKey: "k"})
	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "a", Scopes: []auth.Scope{auth.ScopeChat}})
	srv := httptest.NewServer(New(map[string]*gateway.Client{"m": client}, keys, nil).Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+chatKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if body.Error.Code != "upstream_error" {
		t.Fatalf("code = %q", body.Error.Code)
	}
}

func TestStreamPassthroughAndUsage(t *testing.T) {
	e := newEnv(t)

	resp := post(t, e, chatKey, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	raw := readAll(t, resp)
	if !strings.Contains(raw, `"he"`) || !strings.Contains(raw, `"llo"`) || !strings.Contains(raw, "[DONE]") {
		t.Fatalf("stream body = %s", raw)
	}

	totals, err := e.ledger.Totals(t.Context(), usage.Filter{KeyID: "agent-chat"})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if totals.Entries != 1 || totals.PromptTokens != 5 || totals.CompletionTokens != 2 {
		t.Fatalf("stream usage = %+v", totals)
	}
}

func TestStreamUpstreamErrorRetriesThenFails(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "slow down")
	}))
	defer up.Close()

	client, _ := gateway.New(gateway.Config{BaseURL: up.URL, Model: "m", APIKey: "k"})
	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "a", Scopes: []auth.Scope{auth.ScopeChat}})
	srv := httptest.NewServer(New(map[string]*gateway.Client{"m": client}, keys, nil).Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+chatKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestSetRoutesHotSwap(t *testing.T) {
	upstream := func(text string) *gateway.Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+text+`"}}]}`)
		}))
		t.Cleanup(srv.Close)
		client, err := gateway.New(gateway.Config{BaseURL: srv.URL, Model: text})
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}
		return client
	}

	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "agent", Scopes: []auth.Scope{auth.ScopeChat}})

	server := New(map[string]*gateway.Client{"old": upstream("A")}, keys, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	if names := server.Models(); len(names) != 1 || names[0] != "old" {
		t.Fatalf("models = %v", names)
	}

	// Swap without recreating the HTTP server: the console changes provider
	// config while requests keep coming.
	server.SetRoutes(map[string]*gateway.Client{"new": upstream("B")})
	if names := server.Models(); len(names) != 1 || names[0] != "new" {
		t.Fatalf("models after swap = %v", names)
	}

	call := func(model string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+chatKey)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(raw)
	}

	if resp, _ := call("new"); resp.StatusCode != http.StatusOK {
		t.Fatalf("new model status = %d", resp.StatusCode)
	}
	resp, body := call("old")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Fatalf("old model = %d %s", resp.StatusCode, body)
	}
}

// TestProxyTranslatesAnthropicUpstream proves the whole chain: a caller sends
// OpenAI chat, the upstream speaks Messages, and the caller gets chat back.
func TestProxyTranslatesAnthropicUpstream(t *testing.T) {
	var path string
	var headers http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":7}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`)
			return
		}
		io.WriteString(w, `{"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"hello from claude"}],"usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	t.Cleanup(up.Close)

	client, err := gateway.New(gateway.Config{
		Name:     "claude",
		BaseURL:  up.URL,
		APIKey:   "sk-ant-test",
		Model:    "claude-sonnet-4-5",
		Protocol: "anthropic",
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "agent-chat", Scopes: []auth.Scope{auth.ScopeChat}})
	ledger := usage.NewMemoryLedger(nil)
	server := New(map[string]*gateway.Client{"fast": client}, keys, ledger)

	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-API-Key", chatKey)
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := call(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if path != "/v1/messages" {
		t.Fatalf("upstream path = %q", path)
	}
	if headers.Get("x-api-key") != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", headers.Get("x-api-key"))
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello from claude" {
		t.Fatalf("content = %v", message["content"])
	}
	if usageField := completion["usage"].(map[string]any); usageField["prompt_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usageField)
	}

	rec = call(`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d", rec.Code)
	}
	stream := rec.Body.String()
	if !strings.Contains(stream, `"content":"hel"`) || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("stream = %s", stream)
	}
	if strings.Contains(stream, "content_block_delta") {
		t.Fatalf("raw anthropic events leaked: %s", stream)
	}

	totals, err := ledger.Totals(t.Context(), usage.Filter{KeyID: "agent-chat"})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Entries != 2 || totals.PromptTokens != 14 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestProxyTranslatesResponsesUpstream(t *testing.T) {
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"codex"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5-codex","status":"completed","usage":{"input_tokens":4,"output_tokens":2}}}

`)
	}))
	t.Cleanup(up.Close)

	client, err := gateway.New(gateway.Config{
		Name:     "codex",
		BaseURL:  up.URL + "/backend-api/codex",
		APIKey:   "codex-token",
		Model:    "gpt-5-codex",
		Protocol: "responses",
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	keys := auth.NewStaticKeys()
	keys.Add(chatKey, auth.Identity{AgentID: "agent-chat", Scopes: []auth.Scope{auth.ScopeChat}})
	server := New(map[string]*gateway.Client{"fast": client}, keys, usage.NewMemoryLedger(nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-API-Key", chatKey)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if path != "/backend-api/codex/responses" {
		t.Fatalf("upstream path = %q", path)
	}
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decode: %v", err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "codex" {
		t.Fatalf("content = %v", message["content"])
	}
}
