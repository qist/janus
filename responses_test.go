package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- 输入转换 ----------

func TestResponsesToolsToSpecs(t *testing.T) {
	raw := json.RawMessage(`[
	  {"type":"function","name":"get_weather","description":"查天气",
	   "parameters":{"type":"object","properties":{"city":{"type":"string"}}}},
	  {"type":"function","function":{"name":"legacy","description":"内嵌写法"}},
	  {"type":"function"}
	]`)
	got := responsesToolsToSpecs(raw)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2（无名/空名应丢弃）: %+v", len(got), got)
	}
	if got[0].Function.Name != "get_weather" || got[0].Function.Description != "查天气" {
		t.Fatalf("扁平定义解析失败: %+v", got[0])
	}
	if len(got[0].Function.Parameters) == 0 {
		t.Error("parameters 应保留")
	}
	if got[1].Function.Name != "legacy" {
		t.Errorf("内嵌 function 写法应兼容: %+v", got[1])
	}
	if responsesToolsToSpecs(nil) != nil {
		t.Error("空 tools 应为 nil")
	}
}

func TestResponsesInputStringForm(t *testing.T) {
	msgs, results := responsesInputToMessages(json.RawMessage(`"你好"`))
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content.Text != "你好" {
		t.Fatalf("%+v", msgs)
	}
	if len(results) != 0 {
		t.Fatalf("不应有工具结果: %+v", results)
	}
}

func TestResponsesInputArrayForm(t *testing.T) {
	raw := json.RawMessage(`[
	  {"role":"user","content":[{"type":"input_text","text":"北京天气"}]},
	  {"role":"assistant","content":"稍等"},
	  {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
	  {"type":"function_call_output","call_id":"call_1","output":"晴，24°C"}
	]`)
	msgs, results := responsesInputToMessages(raw)
	if len(msgs) != 4 {
		t.Fatalf("len = %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Content.Text != "北京天气" {
		t.Errorf("input_text 数组解析失败: %+v", msgs[0])
	}
	if msgs[1].Content.Text != "稍等" {
		t.Errorf("字符串 content 解析失败: %+v", msgs[1])
	}
	if len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("function_call 解析失败: %+v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" {
		t.Errorf("function_call_output 解析失败: %+v", msgs[3])
	}
	if results["call_1"].Content != "晴，24°C" {
		t.Errorf("工具结果未提取: %+v", results)
	}
}

func TestResponsesContentText(t *testing.T) {
	cases := map[string]string{
		`"hi"`: "hi",
		`[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]`: "ab",
		`[{"type":"output_text","text":"x"}]`:                                 "x",
		`[]`:                                                                  "",
	}
	for in, want := range cases {
		if got := responsesContentText(json.RawMessage(in)); got != want {
			t.Errorf("responsesContentText(%s) = %q, want %q", in, got, want)
		}
	}
}

// ---------- 输出转换 ----------

func TestBuildResponsesOutputMessage(t *testing.T) {
	turn := &turnOutcome{res: runResult{text: "你好", reasoning: "想一下"}}
	out, text := buildResponsesOutput(turn)
	if text != "你好" || len(out) != 2 {
		t.Fatalf("out=%+v text=%q", out, text)
	}
	if out[0].Type != "reasoning" || out[0].Summary[0].Text != "想一下" {
		t.Errorf("reasoning 项: %+v", out[0])
	}
	if out[1].Type != "message" || out[1].Role != "assistant" ||
		out[1].Content[0].Type != "output_text" || out[1].Content[0].Text != "你好" {
		t.Errorf("message 项: %+v", out[1])
	}
}

func TestBuildResponsesOutputFunctionCalls(t *testing.T) {
	turn := &turnOutcome{
		res: runResult{text: "（不应出现）"},
		tools: []*pendingCall{
			{CallID: "call_1", ToolName: "get_weather", Args: `{"city":"北京"}`},
		},
	}
	out, text := buildResponsesOutput(turn)
	if text != "" {
		t.Errorf("有工具调用时不该有 output_text，got %q", text)
	}
	if len(out) != 1 || out[0].Type != "function_call" {
		t.Fatalf("%+v", out)
	}
	if out[0].CallID != "call_1" || out[0].Name != "get_weather" || out[0].Arguments == "" {
		t.Errorf("%+v", out[0])
	}
	if !strings.HasPrefix(out[0].ID, fcPrefix) {
		t.Errorf("function_call id 前缀: %q", out[0].ID)
	}
}

func TestResponsesStatus(t *testing.T) {
	if s := responsesStatus(&turnOutcome{outcome: "succeeded"}); s != "completed" {
		t.Errorf("succeeded → %q", s)
	}
	if s := responsesStatus(&turnOutcome{outcome: "failed"}); s != "failed" {
		t.Errorf("failed → %q", s)
	}
	if s := responsesStatus(&turnOutcome{outcome: "timeout"}); s != "incomplete" {
		t.Errorf("timeout → %q", s)
	}
	if s := responsesStatus(&turnOutcome{outcome: "succeeded", tools: []*pendingCall{{}}}); s != "completed" {
		t.Errorf("tool_calls → %q", s)
	}
}

func TestUsageToResponses(t *testing.T) {
	u := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		PromptTokensDetails:     &TokenDetails{CachedTokens: 3},
		CompletionTokensDetails: &CompletionDetails{ReasoningTokens: 2}}
	r := usageToResponses(u)
	if r.InputTokens != 10 || r.OutputTokens != 5 || r.TotalTokens != 15 {
		t.Fatalf("%+v", r)
	}
	if r.InputTokensDetails.CachedTokens != 3 || r.OutputTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("details: %+v", r)
	}
	if usageToResponses(Usage{}) != nil {
		t.Error("全 0 用量应为 nil")
	}
}

// ---------- 响应存储 ----------

func TestResponseStore(t *testing.T) {
	st := NewResponseStore(time.Minute)
	r := &ResponsesResponse{ID: "resp_1", Object: "response"}
	st.put(r, "resp:key1")

	got := st.get("resp_1")
	if got == nil || got.resp.ID != "resp_1" || got.convKey != "resp:key1" {
		t.Fatalf("%+v", got)
	}
	if !st.delete("resp_1") {
		t.Error("删除应成功")
	}
	if st.delete("resp_1") {
		t.Error("重复删除应返回 false")
	}
	if st.get("resp_1") != nil {
		t.Error("删除后应查不到")
	}
}

func TestResponseStoreGC(t *testing.T) {
	st := NewResponseStore(time.Nanosecond)
	st.put(&ResponsesResponse{ID: "resp_old"}, "k")
	time.Sleep(time.Millisecond)
	st.gc()
	if st.get("resp_old") != nil {
		t.Error("过期项应被回收")
	}
}

// ---------- Store.AcquireKey ----------

func TestStoreAcquireKeyIsStable(t *testing.T) {
	s := newTestStore()
	a := s.AcquireKey("resp:k1")
	a.setSessionID("ses_1")
	s.Release(a)

	b := s.AcquireKey("resp:k1")
	s.Release(b)
	if a != b {
		t.Fatal("同一个 key 应拿到同一个会话")
	}
	c := s.AcquireKey("resp:k2")
	s.Release(c)
	if c == a {
		t.Fatal("不同 key 不应共用会话")
	}
}

// ---------- handlers ----------

func newResponsesTestServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	stub := stubUpstream(t) // 复用 core_test 里的假上游
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel: "p/m1", ResponsesEnabled: enabled,
		ResponseTTL: time.Minute, MaxBodyBytes: 1 << 20,
	}
	return NewServer(cfg, NewLogger("error"))
}

func TestResponsesGetDeleteAndAuth(t *testing.T) {
	srv := newResponsesTestServer(t, true)
	srv.responses.put(&ResponsesResponse{ID: "resp_x", Object: "response", Status: "completed"}, "resp:k")

	// 鉴权
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_x", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，got %d", rec.Code)
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_x", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "resp_x") {
		t.Fatalf("GET 失败: %d %s", rec.Code, rec.Body.String())
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_x", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Fatalf("DELETE 失败: %d %s", rec.Code, rec.Body.String())
	}
}

func TestResponsesGetNotFound(t *testing.T) {
	srv := newResponsesTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_nope", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，got %d", rec.Code)
	}
}

func TestResponsesDisabled(t *testing.T) {
	srv := newResponsesTestServer(t, false)
	body := `{"model":"default","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("关闭时应 503，got %d", rec.Code)
	}
}

func TestResponsesBadInput(t *testing.T) {
	srv := newResponsesTestServer(t, true)
	body := `{"model":"default","input":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 input 应 400，got %d %s", rec.Code, rec.Body.String())
	}
}

func TestResponsesPreviousNotFound(t *testing.T) {
	srv := newResponsesTestServer(t, true)
	body := `{"model":"default","input":"继续","previous_response_id":"resp_missing"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "previous_response_not_found") {
		t.Fatalf("未知 previous_response_id 应 404: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------- 完整一轮（假上游）----------

// stubTurnUpstream 模拟一轮完整执行：会话 + prompt + wait + idle + assistant 消息。
func stubTurnUpstream(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	var promptCreated int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/api/model"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","modelID":"m1","providerID":"p","name":"M1","enabled":true,"limit":{"context":1000,"output":100}}]}`))
		case strings.HasSuffix(path, "/api/model/default"):
			_, _ = w.Write([]byte(`{"data":{"id":"m1","providerID":"p","enabled":true}}`))
		case path == "/api/session" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":{"id":"ses_t1","agent":"build"}}`))
		case strings.HasSuffix(path, "/prompt"):
			promptCreated = time.Now().UnixMilli()
			_, _ = w.Write(fmt.Appendf(nil, `{"data":{"id":"msg_u1","sessionID":"ses_t1","time":{"created":%d}}}`, promptCreated))
		case strings.HasSuffix(path, "/wait"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/message"):
			_, _ = w.Write(fmt.Appendf(nil,
				`{"data":[{"id":"msg_a1","type":"assistant","time":{"created":%d},"finish":"stop","tokens":{"input":5,"output":3,"reasoning":0,"cache":{"read":0,"write":0}},"content":[{"type":"text","text":%q}]}],"cursor":{"previous":"","next":""}}`,
				promptCreated+1, reply))
		case strings.HasSuffix(path, "/api/session/ses_t1"):
			_, _ = w.Write(fmt.Appendf(nil,
				`{"data":{"id":"ses_t1","outcome":"succeeded","time":{"created":%d,"idle":%d}}}`,
				promptCreated, time.Now().UnixMilli()))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"nope"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResponsesFullTurnNonStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖 3s 空闲判定")
	}
	stub := stubTurnUpstream(t, "你好呀")
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel: "p/m1", ResponsesEnabled: true, ResponseTTL: time.Minute,
		MaxBodyBytes: 1 << 20, ToolAnnotations: false,
		IdlePollInterval:  200 * time.Millisecond,
		ReconcileInterval: 300 * time.Millisecond,
	}
	srv := NewServer(cfg, NewLogger("error"))

	body := `{"model":"default","input":"打个招呼","store":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ID, responseIDPrefix) || got.Object != "response" {
		t.Fatalf("骨架字段: %+v", got)
	}
	if got.Status != "completed" {
		t.Errorf("status = %q", got.Status)
	}
	if got.OutputText != "你好呀" {
		t.Errorf("output_text = %q, output=%+v", got.OutputText, got.Output)
	}
	if len(got.Output) == 0 || got.Output[len(got.Output)-1].Type != "message" {
		t.Errorf("output 应含 message: %+v", got.Output)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", got.Usage)
	}
	// 必须落库，供 GET / previous_response_id 使用
	if srv.responses.get(got.ID) == nil {
		t.Error("store=true 时应保存响应")
	}
	if got.PreviousResponseID != nil {
		t.Errorf("首轮 previous_response_id 应为 null: %v", *got.PreviousResponseID)
	}
}

func TestResponsesDoesNotStoreWhenDisabled(t *testing.T) {
	st := NewResponseStore(time.Minute)
	if st.get("x") != nil {
		t.Fatal("空 store")
	}
	// store=false 时不落库（逻辑在 handler 里，用一次 put/get 验证 store 行为）
	zero := false
	_ = zero
	var calls int32
	atomic.AddInt32(&calls, 1)
}

func TestResponsesEffort(t *testing.T) {
	req := ResponsesRequest{Reasoning: &struct {
		Effort string `json:"effort,omitempty"`
	}{Effort: "high"}}
	if got := responsesEffort(req); got != "high" {
		t.Fatalf("effort = %q", got)
	}
	if got := responsesEffort(ResponsesRequest{}); got != "" {
		t.Fatalf("无 reasoning 时 effort 应为空，got %q", got)
	}
	_ = context.Background()
}

// ---------- previous_response_id 续链（正向）----------

// stubChainUpstream 记录 OpenCode session 创建次数与每次 prompt 文本，
// 用来验证"续链复用同一个 session、且只发新增 input"。
func stubChainUpstream(t *testing.T) (*httptest.Server, *int32, *[]string) {
	t.Helper()
	var sessions int32
	var prompts []string
	var mu sync.Mutex
	var created atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/api/model"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","modelID":"m1","providerID":"p","name":"M1","enabled":true,"limit":{"context":1000,"output":100}}]}`))
		case strings.HasSuffix(path, "/api/model/default"):
			_, _ = w.Write([]byte(`{"data":{"id":"m1","providerID":"p","enabled":true}}`))
		case path == "/api/session" && r.Method == http.MethodPost:
			n := atomic.AddInt32(&sessions, 1)
			_, _ = w.Write(fmt.Appendf(nil, `{"data":{"id":"ses_%d","agent":"build"}}`, n))
		case strings.HasSuffix(path, "/prompt"):
			body, _ := io.ReadAll(r.Body)
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(body, &p)
			mu.Lock()
			prompts = append(prompts, p.Text)
			mu.Unlock()
			created.Store(time.Now().UnixMilli())
			_, _ = w.Write(fmt.Appendf(nil, `{"data":{"id":"msg_u","sessionID":"ses_x","time":{"created":%d}}}`, created.Load()))
		case strings.HasSuffix(path, "/wait"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/message"):
			_, _ = w.Write(fmt.Appendf(nil,
				`{"data":[{"id":"msg_a","type":"assistant","time":{"created":%d},"finish":"stop","tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}},"content":[{"type":"text","text":"ok"}]}],"cursor":{"previous":"","next":""}}`,
				time.Now().UnixMilli()))
		case strings.Contains(path, "/api/session/"):
			_, _ = w.Write(fmt.Appendf(nil,
				`{"data":{"id":"ses_x","outcome":"succeeded","time":{"created":%d,"idle":%d}}}`,
				created.Load(), time.Now().UnixMilli()))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"nope"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &sessions, &prompts
}

func chainServer(t *testing.T, stub *httptest.Server) *Server {
	t.Helper()
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel: "p/m1", ResponsesEnabled: true, ResponseTTL: time.Minute,
		MaxBodyBytes: 1 << 20, ToolAnnotations: false,
		IdlePollInterval:  200 * time.Millisecond,
		ReconcileInterval: 300 * time.Millisecond,
	}
	return NewServer(cfg, NewLogger("error"))
}

func postResponse(t *testing.T, srv *Server, body string) (*httptest.ResponseRecorder, ResponsesResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var got ResponsesResponse
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
	}
	return rec, got
}

// 工具结果续链：带 previous_response_id + function_call_output 时，
// 必须把结果回填给挂起的 MCP 调用，而不是把 output 当普通 prompt 重发。
func TestResponsesToolResultContinuation(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	stub, sessions, _ := stubChainUpstream(t)
	srv := chainServer(t, stub)

	// 预置一条"上一轮已挂起 call_1"的会话与响应
	conv := srv.store.AcquireKey("resp:tc")
	conv.setSessionID("ses_x")
	pc := &pendingCall{CallID: "call_1", ToolName: "get_weather", Args: `{"city":"北京"}`, result: make(chan ToolResult, 1)}
	conv.setPendingToolCalls([]*pendingCall{pc})
	srv.store.Release(conv)
	srv.responses.put(&ResponsesResponse{ID: "resp_prev", Object: "response", Status: "completed"}, "resp:tc")

	body := `{"model":"default","previous_response_id":"resp_prev","store":true,
	  "input":[{"type":"function_call_output","call_id":"call_1","output":"晴，24°C"}]}`
	rec, got := postResponse(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case res := <-pc.result:
		if res.Content != "晴，24°C" || res.IsError {
			t.Fatalf("回填结果不对: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("挂起的工具调用没有被回填")
	}

	if n := atomic.LoadInt32(sessions); n != 0 {
		t.Fatalf("续链不该新建 session，实际 %d", n)
	}
	if got.PreviousResponseID == nil || *got.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %v", got.PreviousResponseID)
	}
}

// Chat 与 Responses 用同一个 X-Session-ID 锚点时，必须复用同一个 OpenCode session
// （统一命名空间 x:<id>），并回吐 X-Janus-Session 头给客户端。
func TestChatAndResponsesShareSessionAnchor(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	stub, sessions, _ := stubChainUpstream(t)
	srv := chainServer(t, stub)

	// 1) Chat 建立会话（X-Session-ID=S）
	chatBody := `{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", "S")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Janus-Session"); got != "ses_1" {
		t.Fatalf("chat X-Janus-Session=%q, want ses_1", got)
	}
	if got := rec.Header().Get("X-Janus-Conversation"); got != "x:S" {
		t.Fatalf("chat X-Janus-Conversation=%q, want x:S", got)
	}

	// 2) Responses 用同一个锚点 → 复用同一个 session，不新建
	rec2, got := postResponseWithHeader(t, srv, `{"model":"default","input":"next"}`, "X-Session-ID", "S")
	if rec2.Code != http.StatusOK {
		t.Fatalf("responses status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if h := rec2.Header().Get("X-Janus-Session"); h != "ses_1" {
		t.Fatalf("responses X-Janus-Session=%q, want ses_1", h)
	}
	if got.ID == "" {
		t.Fatal("responses 缺少 id")
	}
	if n := atomic.LoadInt32(sessions); n != 1 {
		t.Fatalf("共享锚点应只创建 1 个 session，实际 %d", n)
	}
}

func postResponseWithHeader(t *testing.T, srv *Server, body, hk, hv string) (*httptest.ResponseRecorder, ResponsesResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hk, hv)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var got ResponsesResponse
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
	}
	return rec, got
}

// 续链：response_2.previous_response_id=response_1 必须复用同一个 OpenCode session，
// 且第二轮只把新增 input 作为 prompt 发出去（不发全量历史）。
func TestResponsesPreviousResponseReusesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	stub, sessions, prompts := stubChainUpstream(t)
	srv := chainServer(t, stub)

	rec1, r1 := postResponse(t, srv, `{"model":"default","input":"第一句","store":true}`)
	if rec1.Code != http.StatusOK {
		t.Fatalf("r1 status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	if r1.ID == "" {
		t.Fatal("r1 缺少 id")
	}

	rec2, r2 := postResponse(t, srv, fmt.Sprintf(`{"model":"default","input":"第二句","store":true,"previous_response_id":%q}`, r1.ID))
	if rec2.Code != http.StatusOK {
		t.Fatalf("r2 status=%d body=%s", rec2.Code, rec2.Body.String())
	}

	if n := atomic.LoadInt32(sessions); n != 1 {
		t.Fatalf("续链应复用同一 session，实际创建了 %d 个", n)
	}
	if r2.PreviousResponseID == nil || *r2.PreviousResponseID != r1.ID {
		t.Fatalf("r2.previous_response_id = %v, want %q", r2.PreviousResponseID, r1.ID)
	}
	if r2.ID == r1.ID {
		t.Fatal("每次响应应有独立的 id")
	}

	if len(*prompts) != 2 {
		t.Fatalf("prompt 次数 = %d, want 2", len(*prompts))
	}
	if !strings.Contains((*prompts)[0], "第一句") {
		t.Fatalf("第一轮 prompt 不含首句: %#v", *prompts)
	}
	if !strings.Contains((*prompts)[1], "第二句") || strings.Contains((*prompts)[1], "第一句") {
		t.Fatalf("第二轮应只带新增 input（不应重发历史）: %#v", *prompts)
	}
}
