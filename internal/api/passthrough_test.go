package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/gateway"
	"janus/internal/logx"
)

// newPassthroughServer 构造一个带 provider / credential / 路由的直连环境。
// upstream 是假的厂商端点，返回请求体（便于断言我们到底发了什么）。
func newPassthroughServer(t *testing.T, providerType string) (*Server, *httptest.Server, *db.Store) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		out := map[string]any{
			"path":   r.URL.Path,
			"auth":   r.Header.Get("Authorization"),
			"apiKey": r.Header.Get("x-api-key"),
			"body":   in,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(upstream.Close)

	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(st.Close)

	if _, err := st.SaveProvider(db.Provider{
		ID: "vendor", Type: providerType, BaseURL: upstream.URL,
		AuthKind: "api_key", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if _, err := st.CreateCredential("vendor", db.CredentialInput{
		Label: "main", Secret: map[string]any{"api_key": "sk-real"},
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	srv := &Server{
		cfg:    config.Config{MaxBodyBytes: 1 << 20, ModelEcho: "real"},
		log:    logx.NewLogger("error"),
		db:     st,
		router: gateway.New(st),
		proxyc: upstream.Client(),
	}
	return srv, upstream, st
}

// 直连只改 model 字段，凭据由 janus 注入，客户端的 key 不外泄。
func TestProxyModelRewritesModelAndInjectsCredential(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolOpenAIChat)

	body := `{"model":"vendor/cheap","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-janus-agent-client")
	rec := httptest.NewRecorder()

	if !srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIChat, []byte(body), openAIFail(rec)) {
		t.Fatal("proxyModel did not take over the request")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Path   string         `json:"path"`
		Auth   string         `json:"auth"`
		Body   map[string]any `json:"body"`
		APIKey string         `json:"apiKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode upstream echo: %v", err)
	}
	if got.Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", got.Path)
	}
	if got.Auth != "Bearer sk-real" {
		t.Errorf("upstream authorization = %q, want injected credential", got.Auth)
	}
	if got.Body["model"] != "cheap" {
		t.Errorf("upstream model = %v, want stripped provider prefix", got.Body["model"])
	}
	if got.Body["temperature"] != 0.3 {
		t.Errorf("temperature was not passed through: %v", got.Body["temperature"])
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Error("upstream response headers were not forwarded")
	}
}

// Anthropic provider 用 x-api-key + /v1/messages 端点。
func TestProxyModelAnthropicEndpoint(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolAnthropic)

	body := `{"model":"vendor/claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if !srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolAnthropic, []byte(body), anthropicFail(rec)) {
		t.Fatal("proxyModel did not take over the request")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Path   string `json:"path"`
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode upstream echo: %v", err)
	}
	if got.Path != "/v1/messages" {
		t.Errorf("upstream path = %q", got.Path)
	}
	if got.APIKey != "sk-real" {
		t.Errorf("x-api-key = %q", got.APIKey)
	}
}

// 没有翻译实现的协议组合直接拒绝，而不是把请求原样发给说不通的厂商。
// （anthropic ↔ openai-chat 现在有 translate.go，见 TestProxyModelTranslates*。）
func TestProxyModelProtocolMismatch(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolAnthropic)

	body := `{"model":"vendor/m","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIResponses, []byte(body), openAIFail(rec))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "protocol_mismatch") &&
		!strings.Contains(rec.Body.String(), "speaks") {
		t.Errorf("unexpected error body: %s", rec.Body.String())
	}
}

// agent key 默认直连；client key 仍走会话桥；direct 可显式开启。
func TestDirectMode(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolOpenAIChat)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if !srv.directMode(r, Principal{Kind: db.KeyKindAgent}) {
		t.Error("agent key should use direct mode")
	}
	if srv.directMode(r, Principal{Kind: db.KeyKindClient}) {
		t.Error("client key should keep using the session bridge")
	}
	r.Header.Set("X-OpenCode-Agent", "direct")
	if !srv.directMode(r, Principal{Kind: db.KeyKindClient}) {
		t.Error("agent=direct should force direct mode")
	}
}

// 目录在 DB 里：直连模式的 /v1/models 不依赖 OpenCode 上游。
func TestListCatalogModels(t *testing.T) {
	srv, _, st := newPassthroughServer(t, gateway.ProtocolOpenAIChat)
	if _, err := st.SaveModel(db.Model{
		ProviderID: "vendor", UpstreamID: "cheap", DisplayName: "Cheap", Enabled: true,
	}); err != nil {
		t.Fatalf("save model: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.listCatalogModels(rec, Principal{Kind: db.KeyKindAgent})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "vendor/cheap" {
		t.Errorf("unexpected catalog: %+v", out.Data)
	}

	// provider 白名单外的 key 看不到该模型。
	rec = httptest.NewRecorder()
	srv.listCatalogModels(rec, Principal{Kind: db.KeyKindAgent, Providers: []string{"other"}})
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Data) != 0 {
		t.Errorf("provider whitelist was ignored: %+v", out.Data)
	}
}

// capturePassthrough 构造一个直连环境，上游只记录收到的请求，回一个最小响应。
// 用于断言「跨协议时我们到底向上游发了什么形状」。
func capturePassthrough(t *testing.T, providerType, reply string) (*Server, *http.Request, *[]byte) {
	t.Helper()
	var (
		gotReq  *http.Request
		gotBody []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Clone(r.Context())
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(upstream.Close)

	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.SaveProvider(db.Provider{
		ID: "vendor", Type: providerType, BaseURL: upstream.URL,
		AuthKind: "api_key", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if _, err := st.CreateCredential("vendor", db.CredentialInput{
		Label: "main", Secret: map[string]any{"api_key": "sk-real"},
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	srv := &Server{
		cfg:    config.Config{MaxBodyBytes: 1 << 20, ModelEcho: "real"},
		log:    logx.NewLogger("error"),
		db:     st,
		router: gateway.New(st),
		proxyc: upstream.Client(),
	}
	return srv, gotReq, &gotBody
}

// Anthropic 客户端打 OpenAI 厂商：请求翻译成 chat/completions 形状。
func TestProxyModelTranslatesAnthropicToOpenAI(t *testing.T) {
	srv, _, gotBody := capturePassthrough(t, gateway.ProtocolOpenAIChat,
		`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-x",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":2}}`)

	body := `{"model":"vendor/gpt-x","max_tokens":32,` +
		`"system":"be brief","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if !srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolAnthropic, []byte(body), anthropicFail(rec)) {
		t.Fatal("proxyModel did not take over the request")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var sent struct {
		Model     string           `json:"model"`
		MaxTokens int              `json:"max_tokens"`
		Messages  []map[string]any `json:"messages"`
		Tools     []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(*gotBody, &sent); err != nil {
		t.Fatalf("decode upstream body: %v; raw = %s", err, *gotBody)
	}
	if sent.Model != "gpt-x" {
		t.Errorf("upstream model = %q", sent.Model)
	}
	if sent.MaxTokens != 32 {
		t.Errorf("max_tokens = %d", sent.MaxTokens)
	}
	if len(sent.Messages) != 2 || sent.Messages[0]["role"] != "system" ||
		sent.Messages[0]["content"] != "be brief" {
		t.Errorf("system message not translated: %+v", sent.Messages)
	}
	if len(sent.Tools) != 1 || sent.Tools[0]["type"] != "function" {
		t.Fatalf("tools not translated: %+v", sent.Tools)
	}

	// 响应翻译回 Anthropic message。
	var out struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode translated response: %v; body = %s", err, rec.Body.String())
	}
	if out.Type != "message" || len(out.Content) != 1 || out.Content[0].Text != "hello" {
		t.Errorf("translated response = %+v", out)
	}
}

// OpenAI 客户端打 Anthropic 厂商：请求翻译成 messages 形状。
func TestProxyModelTranslatesOpenAIToAnthropic(t *testing.T) {
	srv, _, gotBody := capturePassthrough(t, gateway.ProtocolAnthropic,
		`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":1,"output_tokens":1}}`)

	body := `{"model":"vendor/claude-x","max_tokens":16,` +
		`"messages":[{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if !srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIChat, []byte(body), openAIFail(rec)) {
		t.Fatal("proxyModel did not take over the request")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var sent struct {
		Model     string           `json:"model"`
		System    any              `json:"system"`
		MaxTokens int              `json:"max_tokens"`
		Messages  []map[string]any `json:"messages"`
		Tools     []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(*gotBody, &sent); err != nil {
		t.Fatalf("decode upstream body: %v; raw = %s", err, *gotBody)
	}
	if sent.System != "be brief" {
		t.Errorf("system = %v", sent.System)
	}
	if sent.MaxTokens != 16 {
		t.Errorf("max_tokens = %d", sent.MaxTokens)
	}
	// user / assistant(tool_use) / user(tool_result)
	if len(sent.Messages) != 3 {
		t.Fatalf("messages = %+v", sent.Messages)
	}
	if sent.Messages[1]["role"] != "assistant" {
		t.Errorf("second message role = %v", sent.Messages[1]["role"])
	}
	blocks, _ := sent.Messages[1]["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks = %+v", blocks)
	}
	blk, _ := blocks[0].(map[string]any)
	if blk["type"] != "tool_use" || blk["id"] != "c1" || blk["name"] != "lookup" {
		t.Errorf("tool_use block = %+v", blk)
	}
	last, _ := sent.Messages[2]["content"].([]any)
	if len(last) != 1 {
		t.Fatalf("tool_result blocks = %+v", last)
	}
	tr, _ := last[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "c1" || tr["content"] != "ok" {
		t.Errorf("tool_result block = %+v", tr)
	}

	// 响应翻译回 chat.completion。
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode translated response: %v; body = %s", err, rec.Body.String())
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 ||
		out.Choices[0].Message.Content != "ok" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("translated response = %+v", out)
	}
}

// 跨协议时响应体要换信封：Anthropic message → OpenAI chat.completion。
func TestProxyModelTranslatesResponseEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
			`"content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	srv, _, st := newPassthroughServer(t, gateway.ProtocolAnthropic)
	if _, err := st.SaveProvider(db.Provider{
		ID: "vendor", Type: gateway.ProtocolAnthropic, BaseURL: upstream.URL,
		AuthKind: "api_key", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}

	body := `{"model":"vendor/claude-x","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIChat, []byte(body), openAIFail(rec))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode translated response: %v; body = %s", err, rec.Body.String())
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q", out.Object)
	}
	if out.Model != "vendor/claude-x" {
		t.Errorf("model echo = %q", out.Model)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %+v", out.Choices)
	}
	ch := out.Choices[0]
	if ch.Message.Content != "hello" {
		t.Errorf("content = %q", ch.Message.Content)
	}
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].Function.Name != "lookup" ||
		ch.Message.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Errorf("tool_calls = %+v", ch.Message.ToolCalls)
	}
}

func TestProxyModelAnthropicTierFallback(t *testing.T) {
	srv, _, _ := newPassthroughServer(t, gateway.ProtocolAnthropic)
	srv.setRuntimeDefaultModel("vendor/claude-x")

	body := `{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolAnthropic, []byte(body), anthropicFail(rec))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Body map[string]any `json:"body"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Body["model"] != "claude-x" {
		t.Errorf("upstream model = %v, want tier fallback claude-x", got.Body["model"])
	}
}

// 请求体改写必须是字节保真的：键顺序、数字写法、未知字段都不能变。
func TestReplaceWireModelKeepsBytes(t *testing.T) {
	in := `{"temperature":1.0,"model":"a/b","z":1e3,"nested":{"model":"keep"},"unicode":"中文"}`
	out, err := replaceWireModel([]byte(in), "real-model")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	want := `{"temperature":1.0,"model":"real-model","z":1e3,"nested":{"model":"keep"},"unicode":"中文"}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}

	// 没有 model 字段 → 插到最前面，其余原样。
	out, err = replaceWireModel([]byte(`{"a":1}`), "m")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if string(out) != `{"model":"m","a":1}` {
		t.Errorf("insert got %s", out)
	}

	// 非对象 body → 报错而不是静默篡改。
	if _, err := replaceWireModel([]byte(`[1,2]`), "m"); err == nil {
		t.Error("array body should be rejected")
	}
}

// SSE 流式：只改 model，事件结构、[DONE]、注释都原样保留。
func TestCopyStreamRewritesModel(t *testing.T) {
	src := ": ping\n\ndata: {\"id\":\"1\",\"model\":\"up\",\"choices\":[]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	copyStream(rec, strings.NewReader(src), "vendor/up")

	got := rec.Body.String()
	if !strings.Contains(got, `"model":"vendor/up"`) {
		t.Errorf("model not rewritten: %q", got)
	}
	if !strings.Contains(got, ": ping") || !strings.Contains(got, "data: [DONE]") {
		t.Errorf("stream framing damaged: %q", got)
	}
	if strings.Contains(got, `"model":"up"`) {
		t.Errorf("original model leaked: %q", got)
	}
}

// Anthropic 的 message_start 把 model 放在 message 对象里（第 2 层）。
func TestCopyStreamRewritesNestedModel(t *testing.T) {
	src := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{}}}\n\n"
	rec := httptest.NewRecorder()
	copyStream(rec, strings.NewReader(src), "real")

	got := rec.Body.String()
	if !strings.Contains(got, `"model":"real"`) {
		t.Errorf("nested model not rewritten: %q", got)
	}
}

// model_echo=request 时回显客户端原名；real 时回显 provider/upstream。
func TestProxyModelEcho(t *testing.T) {
	srv, _, st := newPassthroughServer(t, gateway.ProtocolOpenAIChat)
	if _, err := st.SaveModel(db.Model{
		ProviderID: "vendor", UpstreamID: "cheap", DisplayName: "Cheap", Enabled: true,
	}); err != nil {
		t.Fatalf("save model: %v", err)
	}

	for _, tc := range []struct{ echo, want string }{
		{"request", "my-alias"},
		{"real", "vendor/cheap"},
	} {
		srv.cfg.ModelEcho = tc.echo
		body := `{"model":"my-alias","messages":[{"role":"user","content":"hi"}]}`
		// 用全局路由把 my-alias 指到 vendor/cheap
		if _, err := st.SaveRoute(db.ModelRoute{
			Scope: db.RouteScopeGlobal, Alias: "my-alias", Target: "vendor/cheap", Enabled: true,
		}); err != nil {
			t.Fatalf("save route: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIChat, []byte(body), openAIFail(rec))
		if rec.Code != http.StatusOK {
			t.Fatalf("echo=%s status = %d, body = %s", tc.echo, rec.Code, rec.Body.String())
		}
		var got struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Model != tc.want {
			t.Errorf("echo=%s model = %q, want %q", tc.echo, got.Model, tc.want)
		}
	}
}

// 400/404 不该让凭据进冷却，401/429/5xx 才该。
func TestProxyModelCredentialCooldown(t *testing.T) {
	for _, tc := range []struct {
		status int
		cool   bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"message":"x"}}`))
		}))
		st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		if _, err := st.SaveProvider(db.Provider{
			ID: "vendor", Type: gateway.ProtocolOpenAIChat, BaseURL: upstream.URL,
			AuthKind: "api_key", Enabled: true,
		}); err != nil {
			t.Fatalf("save provider: %v", err)
		}
		cred, err := st.CreateCredential("vendor", db.CredentialInput{
			Label: "main", Secret: map[string]any{"api_key": "sk-real"},
		})
		if err != nil {
			t.Fatalf("create credential: %v", err)
		}
		srv := &Server{
			cfg:    config.Config{MaxBodyBytes: 1 << 20, ModelEcho: "real"},
			log:    logx.NewLogger("error"),
			db:     st,
			router: gateway.New(st),
			proxyc: upstream.Client(),
		}
		body := `{"model":"vendor/m","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIChat, []byte(body), openAIFail(rec))

		row, _ := st.GetCredential(cred.ID)
		if tc.cool && row.FailUntil == 0 {
			t.Errorf("status %d should put the credential in cooldown", tc.status)
		}
		if !tc.cool && row.FailUntil != 0 {
			t.Errorf("status %d should not cool the credential (fail_until=%d)", tc.status, row.FailUntil)
		}
		upstream.Close()
		st.Close()
	}
}

// base_url 三种写法都要拼对端点。
func TestUpstreamEndpoint(t *testing.T) {
	for _, tc := range []struct{ base, protocol, want string }{
		{"https://api.openai.com/v1", gateway.ProtocolOpenAIChat, "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com", gateway.ProtocolOpenAIChat, "https://api.openai.com/v1/chat/completions"},
		{"https://api.anthropic.com", gateway.ProtocolAnthropic, "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", gateway.ProtocolAnthropic, "https://api.anthropic.com/v1/messages"},
		{"https://host/v1/messages", gateway.ProtocolAnthropic, "https://host/v1/messages"},
		{"https://host/v1", gateway.ProtocolOpenAIResponses, "https://host/v1/responses"},
	} {
		got := upstreamEndpoint(db.Provider{BaseURL: tc.base}, tc.protocol)
		if got != tc.want {
			t.Errorf("base=%s protocol=%s → %s, want %s", tc.base, tc.protocol, got, tc.want)
		}
	}
	if got := upstreamEndpoint(db.Provider{}, gateway.ProtocolOpenAIChat); got != "" {
		t.Errorf("empty base_url should yield empty endpoint, got %q", got)
	}
}

// 直连模式下 Anthropic 模型列表来自 DB 目录，并带档位别名。
func TestListCatalogAnthropicModels(t *testing.T) {
	srv, _, st := newPassthroughServer(t, gateway.ProtocolAnthropic)
	if _, err := st.SaveModel(db.Model{
		ProviderID: "vendor", UpstreamID: "claude-x", DisplayName: "Claude X", Enabled: true,
	}); err != nil {
		t.Fatalf("save model: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.listCatalogAnthropicModels(rec, Principal{Kind: db.KeyKindAgent})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.HasMore {
		t.Error("has_more should be false")
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		if m.Type != "model" {
			t.Errorf("bad type: %+v", m)
		}
		ids[m.ID] = true
	}
	if !ids["claude-x"] {
		t.Errorf("catalog model missing: %v", ids)
	}
	if !ids["claude-sonnet-4-5"] {
		t.Errorf("tier alias missing: %v", ids)
	}
}
