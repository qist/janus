package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"janus/internal/agent"
	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/gateway"
	"janus/internal/logx"
	"janus/internal/upstream"
)

// newAgentRuntimeServer 构造一个带 provider/credential 的直连环境，upstream 返回
// 给定的响应体（用于 direct runtime 的事件解析）。
func newAgentRuntimeServer(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.SaveProvider(db.Provider{
		ID: "vendor", Type: gateway.ProtocolOpenAIChat, BaseURL: up.URL,
		AuthKind: "api_key", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if _, err := st.CreateCredential("vendor", db.CredentialInput{
		Label: "main", Secret: map[string]any{"api_key": "sk-real"},
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	return &Server{
		cfg:    config.Config{MaxBodyBytes: 1 << 20, ModelEcho: "real"},
		log:    logx.NewLogger("error"),
		db:     st,
		router: gateway.New(st),
		proxyc: up.Client(),
	}
}

func collectEvents(t *testing.T, ch <-chan agent.Event) []agent.Event {
	t.Helper()
	var out []agent.Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-t.Context().Done():
			t.Fatal("event stream did not close")
			return out
		}
	}
}

// direct runtime 把一次非流式透传解析成事件流。
func TestDirectRuntimePromptJSON(t *testing.T) {
	srv := newAgentRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": in["model"],
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "hello from upstream"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
		})
	})

	rt := &directRuntime{s: srv}
	ctx := withDirectContext(context.Background(), Principal{Kind: db.KeyKindAgent}, "")
	ch, err := rt.Prompt(ctx, "sid-1", agent.PromptReq{
		Model:    "vendor/cheap",
		Messages: []agent.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	events := collectEvents(t, ch)

	var text string
	var usage *agent.Usage
	done := false
	for _, e := range events {
		switch e.Type {
		case agent.EventText:
			text += e.Text
		case agent.EventUsage:
			usage = e.Usage
		case agent.EventDone:
			done = true
		}
	}
	if text != "hello from upstream" {
		t.Fatalf("text = %q (events=%+v)", text, events)
	}
	if usage == nil || usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if !done {
		t.Fatal("missing done event")
	}
}

// direct runtime 也能解析 SSE 流式响应。
func TestDirectRuntimePromptStream(t *testing.T) {
	srv := newAgentRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	rt := &directRuntime{s: srv}
	ctx := withDirectContext(context.Background(), Principal{Kind: db.KeyKindAgent}, "")
	ch, err := rt.Prompt(ctx, "sid-2", agent.PromptReq{
		Model:    "vendor/cheap",
		Messages: []agent.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	events := collectEvents(t, ch)

	var text string
	var finish string
	usageSeen := false
	for _, e := range events {
		switch e.Type {
		case agent.EventText:
			text += e.Text
		case agent.EventUsage:
			usageSeen = true
		case agent.EventDone:
			finish = e.Finish
		}
	}
	if text != "hello" {
		t.Fatalf("text = %q (events=%+v)", text, events)
	}
	if !usageSeen || finish != "stop" {
		t.Fatalf("usage=%v finish=%q", usageSeen, finish)
	}
}

// 无 model 时明确报错，而不是打出空请求。
func TestDirectRuntimeRequiresModel(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolOpenAIChat)
	rt := &directRuntime{s: srv}
	if _, err := rt.Prompt(context.Background(), "", agent.PromptReq{}); err == nil {
		t.Fatal("want error when model is empty")
	}
}

// 注册表带上内置的 opencode / direct 两个 runtime。
func TestAgentRegistryHasBuiltins(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolOpenAIChat)
	reg := srv.agentRegistry()
	for _, name := range []string{agent.NameOpenCode, agent.NameDirect} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("runtime %q not registered (names=%v)", name, reg.Names())
		}
	}
}

// agent key 选择 direct runtime；普通客户端走 opencode。
func TestAgentRuntimeSelection(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolOpenAIChat)

	agentReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	agentReq.Header.Set("Authorization", "Bearer sk-agent")
	if rt := srv.agentRuntimeFor(agentReq, Principal{Kind: db.KeyKindAgent}); rt == nil || rt.Name() != agent.NameDirect {
		t.Fatalf("agent key should select direct, got %v", rt)
	}

	directReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	directReq.Header.Set("X-OpenCode-Agent", "direct")
	if rt := srv.agentRuntimeFor(directReq, Principal{Kind: db.KeyKindClient}); rt == nil || rt.Name() != agent.NameDirect {
		t.Fatalf("explicit agent=direct should select direct, got %v", rt)
	}

	plainReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if rt := srv.agentRuntimeFor(plainReq, Principal{Kind: db.KeyKindClient}); rt == nil || rt.Name() != agent.NameOpenCode {
		t.Fatalf("plain client should select opencode, got %v", rt)
	}
}

// handleListRuntimes 列出 runtime；带 model 时给出解析结果。
func TestHandleListRuntimes(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolOpenAIChat)

	rec := httptest.NewRecorder()
	srv.handleListRuntimes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runtimes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Runtimes []struct {
			Name string `json:"name"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// opencode / direct / codex / claude-code
	if len(resp.Runtimes) != 4 {
		t.Fatalf("runtimes = %+v", resp.Runtimes)
	}

	rec2 := httptest.NewRecorder()
	srv.handleListRuntimes(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/runtimes?model=vendor/cheap", nil))
	var resp2 struct {
		Runtime string `json:"runtime"`
		Route   struct {
			ProviderID string `json:"provider_id"`
			UpstreamID string `json:"upstream_id"`
		} `json:"route"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resp2.Runtime != agent.NameDirect || resp2.Route.ProviderID != "vendor" || resp2.Route.UpstreamID != "cheap" {
		t.Fatalf("resolve = %+v", resp2)
	}
}

// 上游事件 → Agent 事件的映射（opencode 适配器的核心）。
func TestOCEventToAgent(t *testing.T) {
	mk := func(typ, data string) (agent.Event, bool) {
		e, terminal := ocEventToAgent(upstream.OCEvent{Type: typ, Data: json.RawMessage(data)}, "s1")
		if e == nil {
			return agent.Event{}, terminal
		}
		return *e, terminal
	}

	if e, _ := mk("session.text.delta", `{"sessionID":"s1","delta":"hi"}`); e.Type != agent.EventText || e.Text != "hi" {
		t.Fatalf("text delta = %+v", e)
	}
	if e, _ := mk("session.reasoning.delta", `{"sessionID":"s1","delta":"why"}`); e.Type != agent.EventReasoning {
		t.Fatalf("reasoning delta = %+v", e)
	}
	if e, _ := mk("session.tool.input.ended", `{"sessionID":"s1","id":"t1","name":"read","text":"{\"path\":\"a\"}"}`); e.Type != agent.EventToolCall || e.ToolCall == nil || e.ToolCall.Name != "read" {
		t.Fatalf("tool call = %+v", e)
	}
	if e, _ := mk("session.step.ended", `{"sessionID":"s1","finish":"stop","cost":0.01,"tokens":{"input":5,"output":2}}`); e.Type != agent.EventUsage || e.Usage == nil || e.Usage.InputTokens != 5 {
		t.Fatalf("usage = %+v", e)
	}
	if e, term := mk("session.execution.succeeded", `{"sessionID":"s1"}`); e.Type != agent.EventDone || !term {
		t.Fatalf("done = %+v terminal=%v", e, term)
	}
	if _, term := mk("session.execution.interrupted", `{"sessionID":"s1"}`); !term {
		t.Fatal("interrupted should be terminal")
	}
	// 别的会话的事件要丢弃
	if e, term := mk("session.text.delta", `{"sessionID":"other","delta":"x"}`); e.Type != "" || term {
		t.Fatalf("foreign session leaked: %+v", e)
	}
}

func TestParseOCModelRef(t *testing.T) {
	ref, ok := parseOCModelRef("vendor/cheap")
	if !ok || ref.ProviderID != "vendor" || ref.ID != "cheap" || ref.Variant != "" {
		t.Fatalf("plain = %+v ok=%v", ref, ok)
	}
	ref, ok = parseOCModelRef("opencode-go/mimo-v2.5-pro:low")
	if !ok || ref.ProviderID != "opencode-go" || ref.ID != "mimo-v2.5-pro" || ref.Variant != "low" {
		t.Fatalf("variant = %+v ok=%v", ref, ok)
	}
	if _, ok := parseOCModelRef("cheap"); ok {
		t.Fatal("bare id should not parse")
	}
	if _, ok := parseOCModelRef(""); ok {
		t.Fatal("empty should not parse")
	}
}
