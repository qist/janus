package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/gateway"
	"janus/internal/logx"
)

func TestExtractUsageOpenAIChat(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`)
	got := extractUsage(raw)
	if got.Input != 100 || got.Output != 20 || got.CacheRead != 40 {
		t.Fatalf("openai chat usage = %+v", got)
	}
}

func TestExtractUsageAnthropic(t *testing.T) {
	raw := []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`)
	got := extractUsage(raw)
	if got.Input != 10 || got.Output != 5 || got.CacheRead != 3 || got.CacheWrite != 2 {
		t.Fatalf("anthropic usage = %+v", got)
	}
}

func TestExtractUsageStreamMergesMax(t *testing.T) {
	// message_start 给输入，message_delta 给累计输出；两者都是 SSE 行。
	raw := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n")
	got := extractUsage(raw)
	if got.Input != 12 || got.Output != 7 {
		t.Fatalf("stream usage = %+v", got)
	}
}

func TestExtractUsageResponsesEnvelope(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":3,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":2}}}}`)
	got := extractUsage(raw)
	if got.Input != 8 || got.Output != 3 || got.CacheRead != 5 || got.Reasoning != 2 {
		t.Fatalf("responses usage = %+v", got)
	}
}

func TestTailBufferKeepsTail(t *testing.T) {
	var tb tailBuffer
	tb.max = 8
	_, _ = tb.Write([]byte("0123456789ABCDEF"))
	if got := string(tb.Bytes()); got != "89ABCDEF" {
		t.Fatalf("tail = %q", got)
	}
}

func TestAggregateOpenAIStream(t *testing.T) {
	raw := []byte(`data: {"id":"c1","model":"wb","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"llo"}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"do","arguments":"{\"a\":"}}]}}]}` + "\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}` + "\n" +
		`data: {"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n" +
		"data: [DONE]\n")
	out := aggregateOpenAIStream(raw)
	var doc struct {
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
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("aggregated json: %v (%s)", err, out)
	}
	if doc.Object != "chat.completion" || doc.Model != "wb" {
		t.Fatalf("envelope = %+v", doc)
	}
	if len(doc.Choices) != 1 || doc.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %+v", doc.Choices)
	}
	if doc.Choices[0].FinishReason != "tool_calls" || len(doc.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("finish/tools = %+v", doc.Choices[0])
	}
	if tc := doc.Choices[0].Message.ToolCalls[0]; tc.ID != "t1" || tc.Function.Name != "do" || tc.Function.Arguments != `{"a":1}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if doc.Usage == nil || doc.Usage["prompt_tokens"].(float64) != 5 {
		t.Fatalf("usage = %+v", doc.Usage)
	}
}

// proxyModel 成功一次后，usage_events 里应有一条按 provider/credential/model 记账的记录。
func TestProxyModelRecordsLocalUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`))
	}))
	t.Cleanup(upstream.Close)

	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(st.Close)
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
	pricing, _ := json.Marshal([]map[string]float64{{
		"input_per_million": 2, "output_per_million": 10, "cache_read_per_million": 0.2,
	}})
	if _, err := st.SaveModel(db.Model{
		ID: "vendor/cheap", ProviderID: "vendor", UpstreamID: "cheap", Pricing: pricing, Enabled: true,
	}); err != nil {
		t.Fatalf("save model: %v", err)
	}

	srv := &Server{
		cfg:    config.Config{MaxBodyBytes: 1 << 20, ModelEcho: "real"},
		log:    logx.NewLogger("error"),
		db:     st,
		router: gateway.New(st),
		proxyc: upstream.Client(),
	}

	body := `{"model":"vendor/cheap","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	if !srv.proxyModel(rec, req, Principal{Kind: db.KeyKindAgent}, gateway.ProtocolOpenAIChat, []byte(body), openAIFail(rec)) {
		t.Fatal("proxyModel did not take over")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	rows := st.ListUsage(db.UsageFilter{}, 10)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ProviderID != "vendor" || got.CredentialID != cred.ID || got.UpstreamModel != "cheap" || got.Model != "vendor/cheap" {
		t.Fatalf("usage identity = %+v", got)
	}
	if got.InputTokens != 100 || got.OutputTokens != 20 || got.CacheReadTokens != 40 {
		t.Fatalf("usage tokens = %+v", got)
	}
	if got.Status != http.StatusOK || got.Source != "local" {
		t.Fatalf("usage meta = %+v", got)
	}
	// 100/1e6*2 + 20/1e6*10 + 40/1e6*0.2 = 0.000408
	if diff := got.CostUSD - 0.000408; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want 0.000408", got.CostUSD)
	}

	// 聚合维度：按渠道与模型都能查到。
	sum := st.SummarizeUsage(db.UsageFilter{From: time.Now().Add(-time.Hour).Unix()})
	if sum.Totals.Requests != 1 || len(sum.ByCredential) != 1 || len(sum.ByModel) != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.ByCredential[0].CredentialID != cred.ID {
		t.Fatalf("by_credential = %+v", sum.ByCredential)
	}
}
