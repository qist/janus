package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- 工具注册表 ----------

func TestMCPServerNameSanitized(t *testing.T) {
	cases := map[string]string{
		"f:5ae60f80f4ecf220": "ob-f-5ae60f80f4ecf220",
		"x:my session":       "ob-x-my-session",
		"plain":              "ob-plain",
	}
	for in, want := range cases {
		if got := mcpServerName(in); got != want {
			t.Errorf("mcpServerName(%q) = %q, want %q", in, got, want)
		}
	}
	// 必须每次稳定（janitor 要靠它反查）
	if mcpServerName("f:abc") != mcpServerName("f:abc") {
		t.Error("名字不稳定")
	}
}

func TestToolSessionNameRoundTrip(t *testing.T) {
	s := &toolSession{key: "f:abc123"}
	mcp := s.mcpToolName("get_weather")
	if !strings.HasPrefix(mcp, mcpToolPrefix+"f:abc123_") {
		t.Fatalf("暴露名 = %q", mcp)
	}
	orig, ok := s.originalName(mcp)
	if !ok || orig != "get_weather" {
		t.Fatalf("还原 = %q ok=%v", orig, ok)
	}
	// 别的会话的名字不能还原
	other := &toolSession{key: "f:zzz"}
	if _, ok := other.originalName(mcp); ok {
		t.Error("不应还原其他会话的工具名")
	}
	// 客户端工具名里带下划线也要能正确还原
	mcp2 := s.mcpToolName("my_tool_v2")
	if orig, ok := s.originalName(mcp2); !ok || orig != "my_tool_v2" {
		t.Fatalf("带下划线的名字还原失败: %q %v", orig, ok)
	}
}

func TestToolBridgeRegisterLookupUnregister(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), time.Minute, 0, nil)
	tools := []ToolSpec{{Type: "function", Function: funcSpec("a", "desc")}}

	s1 := b.Register("k1", tools)
	if s1 == nil || s1.token == "" {
		t.Fatal("注册失败")
	}
	if b.Get(s1.token) != s1 || b.ByKey("k1") != s1 {
		t.Fatal("查不到")
	}
	// 重复注册复用同一个 session，但工具集更新
	s2 := b.Register("k1", []ToolSpec{{Type: "function", Function: funcSpec("b", "d2")}})
	if s2 != s1 {
		t.Fatal("同 key 应复用会话")
	}
	if got := s1.snapshotTools(); len(got) != 1 || got[0].Function.Name != "b" {
		t.Fatalf("工具集未更新: %+v", got)
	}

	b.Unregister("k1")
	if b.Get(s1.token) != nil || b.ByKey("k1") != nil {
		t.Fatal("注销后仍能查到")
	}
}

func TestToolSessionParkTakeAndWatch(t *testing.T) {
	s := &toolSession{key: "k", pending: map[string]*pendingCall{}}
	watch := s.watch()
	defer s.unwatch(watch)

	if s.hasPending() {
		t.Fatal("初始不该有 pending")
	}
	p := s.park("call_1", "get_weather", s.mcpToolName("get_weather"), `{"city":"北京"}`)

	select {
	case <-watch:
	case <-time.After(time.Second):
		t.Fatal("park 后应通知 watch")
	}
	if !s.hasPending() {
		t.Fatal("应有一个 pending")
	}

	got := s.takePending()
	if len(got) != 1 || got[0].CallID != "call_1" {
		t.Fatalf("takePending = %+v", got)
	}
	if s.hasPending() {
		t.Fatal("take 之后应为空")
	}
	// takePending 只是取出，不应应答调用
	select {
	case r := <-p.result:
		t.Fatalf("takePending 不该应答: %+v", r)
	default:
	}

	p.complete(ToolResult{Content: "晴"})
	select {
	case r := <-p.result:
		if r.Content != "晴" {
			t.Fatalf("result = %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("complete 后应能拿到结果")
	}
}

func TestToolSessionCloseFailsPending(t *testing.T) {
	s := &toolSession{key: "k", pending: map[string]*pendingCall{}}
	p := s.park("call_1", "t", s.mcpToolName("t"), "{}")
	s.close()
	select {
	case r := <-p.result:
		if !r.IsError {
			t.Fatalf("关闭时应以错误收尾: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("close 后应立刻拿到结果")
	}
	// 关闭后再 park 也应立刻失败
	p2 := s.park("call_2", "t", s.mcpToolName("t"), "{}")
	select {
	case r := <-p2.result:
		if !r.IsError {
			t.Fatalf("已关闭会话不该接受新调用: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("已关闭会话的 park 应立刻失败")
	}
}

func TestWaitResultTimeout(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), 50*time.Millisecond, 0, nil)
	p := &pendingCall{CallID: "c", result: make(chan ToolResult, 1)}
	r := b.waitResult(context.Background(), p)
	if !r.IsError || !strings.Contains(r.Content, "did not return") {
		t.Fatalf("超时应报错: %+v", r)
	}
}

func TestWaitForTwoTier(t *testing.T) {
	b := NewToolBridge(NewLogger("error"), 5*time.Minute, 90*time.Second,
		map[string]bool{"grep": true, "read": true})

	// 只读/编辑类 → 短等待（大小写不敏感）
	for _, name := range []string{"Grep", "READ"} {
		if got := b.waitFor(&pendingCall{ToolName: name}); got != 90*time.Second {
			t.Fatalf("%s wait = %s, want 90s", name, got)
		}
	}
	// 执行类 / 未知工具 → 长等待（避免误杀长任务）
	for _, name := range []string{"RunCommand", "SomeNewTool"} {
		if got := b.waitFor(&pendingCall{ToolName: name}); got != 5*time.Minute {
			t.Fatalf("%s wait = %s, want 5m", name, got)
		}
	}
}

func TestWaitForFastDisabled(t *testing.T) {
	// waitFast >= wait → 不启用短等待，全部走长等待
	b := NewToolBridge(NewLogger("error"), time.Minute, time.Minute, map[string]bool{"grep": true})
	if got := b.waitFor(&pendingCall{ToolName: "Grep"}); got != time.Minute {
		t.Fatalf("禁用短等待后 Grep wait = %s, want 1m", got)
	}
}

func TestParseToolSet(t *testing.T) {
	got := parseToolSet("Grep, Read ,grep,,RunCommand")
	if !got["grep"] || !got["read"] || !got["runcommand"] {
		t.Fatalf("解析结果缺项: %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("去重/去空失败: %+v", got)
	}
	for _, off := range []string{"", "none", "off", "-"} {
		if s := parseToolSet(off); len(s) != 0 {
			t.Fatalf("parseToolSet(%q) 应为空: %+v", off, s)
		}
	}
}

func TestNormalizeTools(t *testing.T) {
	in := []ToolSpec{
		{Type: "function", Function: funcSpec("b", "")},
		{Type: "function", Function: funcSpec("a", "")},
		{Type: "function", Function: funcSpec("b", "dup")}, // 重名丢弃
		{Type: "function", Function: funcSpec("", "noname")},
	}
	out := normalizeTools(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Function.Name != "a" || out[1].Function.Name != "b" {
		t.Fatalf("未排序去重: %+v", out)
	}
}

func TestToolsFingerprintChanges(t *testing.T) {
	a := []ToolSpec{{Type: "function", Function: funcSpec("x", "1")}}
	b := []ToolSpec{{Type: "function", Function: funcSpec("x", "2")}}
	if toolsFingerprint(a) == toolsFingerprint(b) {
		t.Error("描述变化应导致指纹变化")
	}
	if toolsFingerprint(a) != toolsFingerprint([]ToolSpec{{Type: "function", Function: funcSpec("x", "1")}}) {
		t.Error("相同工具集指纹应一致")
	}
}

func TestToolResultsFromMessages(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: MessageContent{Text: "hi"}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1"}}},
		{Role: "tool", ToolCallID: "call_1", Content: MessageContent{Text: "晴，24°C"}},
	}
	got := toolResultsFromMessages(msgs)
	if got["call_1"].Content != "晴，24°C" {
		t.Fatalf("%+v", got)
	}
	if !hasToolResults(msgs) {
		t.Error("hasToolResults 应为 true")
	}
	if hasToolResults([]ChatMessage{{Role: "user"}}) {
		t.Error("没有 tool 消息时应为 false")
	}
}

// 一轮里并行返回多个工具结果时，必须按 tool_call_id 精确匹配。
func TestToolResultsFromMessagesParallel(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "tool", ToolCallID: "call_a", Content: MessageContent{Text: "A"}},
		{Role: "tool", ToolCallID: "call_b", Content: MessageContent{Text: "B"}},
		{Role: "tool", ToolCallID: "call_c", Content: MessageContent{Text: "C"}},
	}
	got := toolResultsFromMessages(msgs)
	for id, want := range map[string]string{"call_a": "A", "call_b": "B", "call_c": "C"} {
		if got[id].Content != want {
			t.Errorf("%s = %q, want %q", id, got[id].Content, want)
		}
	}
	if len(got) != 3 {
		t.Fatalf("应恰好 3 条结果，got %d", len(got))
	}
}

// ---------- MCP 协议 ----------

func newMCPTestServer(t *testing.T) (*Server, *toolSession) {
	t.Helper()
	cfg := Config{
		Upstream: "http://127.0.0.1:1", Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: "http://127.0.0.1:1",
		ToolCalling: true, ToolCallWait: 2 * time.Second,
	}
	srv := NewServer(cfg, NewLogger("error"))
	tools := []ToolSpec{{
		Type:     "function",
		Function: funcSpec("get_weather", "查天气"),
	}}
	sess := srv.tools.Register("f:test", tools)
	return srv, sess
}

func mcpPost(t *testing.T, srv *Server, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp/"+token, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestMCPInitialize(t *testing.T) {
	srv, sess := newMCPTestServer(t)
	rec := mcpPost(t, srv, sess.token,
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Mcp-Session-Id"); got != sess.token {
		t.Errorf("Mcp-Session-Id = %q", got)
	}
	var out struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Result.ProtocolVersion != "2025-11-25" {
		t.Errorf("应回客户端请求的版本，got %q", out.Result.ProtocolVersion)
	}
	if len(out.Result.Capabilities.Tools) == 0 {
		t.Error("应声明 tools 能力")
	}
}

func TestMCPNotificationAccepted(t *testing.T) {
	srv, sess := newMCPTestServer(t)
	rec := mcpPost(t, srv, sess.token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("通知应回 202，got %d", rec.Code)
	}
}

func TestMCPToolsListUsesNamespacedNames(t *testing.T) {
	srv, sess := newMCPTestServer(t)
	rec := mcpPost(t, srv, sess.token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	body := rec.Body.String()
	want := sess.mcpToolName("get_weather")
	if !strings.Contains(body, want) {
		t.Fatalf("tools/list 应包含命名空间后的名字 %q:\n%s", want, body)
	}
	if !strings.Contains(body, `"inputSchema"`) {
		t.Errorf("应带 inputSchema:\n%s", body)
	}
}

func TestMCPToolsCallParksThenReturnsClientResult(t *testing.T) {
	srv, sess := newMCPTestServer(t)

	// 真实流程里执行器会先注册 watch（表示该会话有在飞请求）；否则会被
	// "防串会话"守卫立即拒绝。
	watch := sess.watch()
	defer sess.unwatch(watch)

	type res struct {
		rec *httptest.ResponseRecorder
	}
	done := make(chan res, 1)
	go func() {
		r := mcpPost(t, srv, sess.token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+
			sess.mcpToolName("get_weather")+`","arguments":{"city":"北京"}}}`)
		done <- res{r}
	}()

	// 等 agent 侧挂起
	var pending []*pendingCall
	deadline := time.After(2 * time.Second)
	for pending == nil {
		select {
		case <-deadline:
			t.Fatal("tools/call 没有挂起")
		case <-time.After(5 * time.Millisecond):
			pending = sess.takePending()
		}
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	p := pending[0]
	if p.ToolName != "get_weather" {
		t.Errorf("应还原成客户端原始工具名，got %q", p.ToolName)
	}
	if !strings.Contains(p.Args, "北京") {
		t.Errorf("args = %q", p.Args)
	}
	if !strings.HasPrefix(p.CallID, "call_") {
		t.Errorf("callID = %q", p.CallID)
	}

	// 客户端回填结果 → MCP 应答
	p.complete(ToolResult{Content: "晴，24°C"})

	select {
	case r := <-done:
		if r.rec.Code != http.StatusOK {
			t.Fatalf("status = %d", r.rec.Code)
		}
		var out struct {
			Result struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(r.rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Result.IsError || len(out.Result.Content) != 1 || out.Result.Content[0].Text != "晴，24°C" {
			t.Fatalf("MCP 应答 = %+v", out.Result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("回填结果后 MCP 请求没有返回")
	}
}

func TestMCPUnknownTool(t *testing.T) {
	srv, sess := newMCPTestServer(t)
	rec := mcpPost(t, srv, sess.token,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ob_nope","arguments":{}}}`)
	if !strings.Contains(rec.Body.String(), "unknown tool") {
		t.Fatalf("应报 unknown tool:\n%s", rec.Body.String())
	}
}

func TestMCPUnknownToken(t *testing.T) {
	srv, _ := newMCPTestServer(t)
	rec := mcpPost(t, srv, "deadbeef", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("未知 token 应给可恢复响应(200)，got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tools":[]`) {
		t.Fatalf("未知 token 的 tools/list 应返回空工具表:\n%s", rec.Body.String())
	}
}

func TestMCPGetReturns405(t *testing.T) {
	srv, sess := newMCPTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp/"+sess.token, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，got %d", rec.Code)
	}
}

// ---------- OpenAI 侧的工具输出格式 ----------

func TestBuildToolCallResponse(t *testing.T) {
	pending := []*pendingCall{
		{CallID: "call_1", ToolName: "get_weather", Args: `{"city":"北京"}`},
		{CallID: "call_2", ToolName: "get_time", Args: `{"tz":"CST"}`},
	}
	resp := buildToolCallResponse("chatcmpl-x", "opencode-go/gpt-6-luna", pending)
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", resp.Choices[0].FinishReason)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 2 || calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" {
		t.Fatalf("%+v", calls)
	}
	if calls[0].Index != nil {
		t.Error("非流式响应的 tool_calls 不该带 index")
	}
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"finish_reason":"tool_calls"`) {
		t.Errorf("JSON: %s", b)
	}
}

func TestSSEToolCallsFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = sw.begin()
	_ = sw.toolCalls([]*pendingCall{
		{CallID: "call_1", ToolName: "get_weather", Args: `{"city":"北京"}`},
		{CallID: "call_2", ToolName: "get_time", Args: `{}`},
	})
	_ = sw.finish("tool_calls", &Usage{}, false)

	body := rec.Body.String()
	// 流式 delta 必须带 index（并发工具调用靠它对应序号）
	if !strings.Contains(body, `"index":0`) || !strings.Contains(body, `"index":1`) {
		t.Errorf("tool_calls delta 应带 index:\n%s", body)
	}
	if !strings.Contains(body, `"name":"get_weather"`) || !strings.Contains(body, `"id":"call_1"`) {
		t.Errorf("缺少工具名/id:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("缺少 finish_reason:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺少 [DONE]:\n%s", body)
	}
}

// 一轮返回 3 个并行工具调用时，流式 delta 的 index 必须严格 0/1/2，
// 且 id/name 一一对应（对应 "一次 response 里 text + tool_call A/B/C"）。
func TestSSEToolCallsThreeIndexes(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = sw.begin()
	_ = sw.toolCalls([]*pendingCall{
		{CallID: "call_a", ToolName: "tool_a", Args: `{}`},
		{CallID: "call_b", ToolName: "tool_b", Args: `{}`},
		{CallID: "call_c", ToolName: "tool_c", Args: `{}`},
	})
	_ = sw.finish("tool_calls", &Usage{}, false)

	body := rec.Body.String()
	for i, tc := range [][2]string{{"call_a", "tool_a"}, {"call_b", "tool_b"}, {"call_c", "tool_c"}} {
		if !strings.Contains(body, `"index":`+strconv.Itoa(i)) {
			t.Errorf("缺少 index %d:\n%s", i, body)
		}
		if !strings.Contains(body, `"id":"`+tc[0]+`"`) || !strings.Contains(body, `"name":"`+tc[1]+`"`) {
			t.Errorf("缺少 %s/%s:\n%s", tc[0], tc[1], body)
		}
	}
}

// ---------- 工具注册的健壮性 ----------

// stubUpstreamForTools 记录 MCP PUT 次数，并可指定其返回码。
func stubUpstreamForTools(t *testing.T, mcpStatus int) (*httptest.Server, *int32) {
	t.Helper()
	var puts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/experimental/mcp/") && r.Method == http.MethodPut:
			atomic.AddInt32(&puts, 1)
			w.WriteHeader(mcpStatus)
			if mcpStatus >= 400 {
				return
			}
			return
		case strings.HasSuffix(r.URL.Path, "/api/model"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","modelID":"m1","providerID":"p","name":"M1","enabled":true,"limit":{"context":1000,"output":100}}]}`))
		case strings.HasSuffix(r.URL.Path, "/api/model/default"):
			_, _ = w.Write([]byte(`{"data":{"id":"m1","providerID":"p","enabled":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"nope"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &puts
}

func newToolTestServer(t *testing.T, mcpStatus int, reregister time.Duration, softFail bool) (*Server, *int32) {
	t.Helper()
	stub, puts := stubUpstreamForTools(t, mcpStatus)
	cfg := Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		DefaultModel:   "p/m1",
		ToolCalling:    true,
		ToolSoftFail:   softFail,
		ToolReregister: reregister,
		ToolCallWait:   time.Minute,
	}
	return NewServer(cfg, NewLogger("error")), puts
}

func toolSpecs() []ToolSpec {
	return []ToolSpec{{Type: "function", Function: funcSpec("get_weather", "查天气")}}
}

// 注册成功后在 TTL 内不重复注册；TTL 过期后主动续注册（上游重启会丢注册）。
func TestEnsureToolsReregistersAfterTTL(t *testing.T) {
	srv, puts := newToolTestServer(t, http.StatusNoContent, time.Hour, false)
	conv := &Conversation{Key: "f:k"}
	ctx := context.Background()

	if err := srv.ensureTools(ctx, conv, "/tmp", toolSpecs()); err != nil {
		t.Fatal(err)
	}
	if err := srv.ensureTools(ctx, conv, "/tmp", toolSpecs()); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(puts); n != 1 {
		t.Fatalf("TTL 内应只注册一次，实际 %d 次", n)
	}

	// 工具集变化 → 立即重注册
	if err := srv.ensureTools(ctx, conv, "/tmp", []ToolSpec{
		{Type: "function", Function: funcSpec("get_time", "查时间")},
	}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(puts); n != 2 {
		t.Fatalf("工具集变化应重注册，实际 %d 次", n)
	}

	// TTL 过期 → 续注册
	srv.cfg.ToolReregister = time.Nanosecond
	time.Sleep(time.Millisecond)
	if err := srv.ensureTools(ctx, conv, "/tmp", []ToolSpec{
		{Type: "function", Function: funcSpec("get_time", "查时间")},
	}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(puts); n != 3 {
		t.Fatalf("TTL 过期应续注册，实际 %d 次", n)
	}
}

func TestEnsureToolsPropagatesError(t *testing.T) {
	srv, _ := newToolTestServer(t, http.StatusInternalServerError, time.Hour, false)
	conv := &Conversation{Key: "f:k"}
	err := srv.ensureTools(context.Background(), conv, "/tmp", toolSpecs())
	if err == nil {
		t.Fatal("上游 500 时 ensureTools 应报错")
	}
	if !strings.Contains(err.Error(), "register MCP server") || !strings.Contains(err.Error(), "/tmp") {
		t.Fatalf("错误信息应带上服务器名与目录: %v", err)
	}
	if conv.mcpName != "" || conv.toolSess != nil {
		t.Error("注册失败时不该留下半截状态")
	}
}

// 客户端声明了 tools 却注册失败：必须显式报错，而不是静默降级成无工具。
func TestChatFailsExplicitlyWhenToolRegistrationFails(t *testing.T) {
	srv, _ := newToolTestServer(t, http.StatusInternalServerError, time.Hour, false)

	body := `{"model":"default","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
	          "messages":[{"role":"user","content":"北京天气"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tool_registration_failed") {
		t.Fatalf("应返回 tool_registration_failed:\n%s", rec.Body.String())
	}
	// 错误信息要能指导用户
	if !strings.Contains(rec.Body.String(), "BRIDGE_TOOL_SOFT_FAIL") {
		t.Errorf("错误信息应提示降级开关:\n%s", rec.Body.String())
	}
}

// soft-fail 时不应因为注册失败而报 tool_registration_failed。
func TestChatSoftFailContinuesWithoutTools(t *testing.T) {
	srv, _ := newToolTestServer(t, http.StatusInternalServerError, time.Hour, true)

	body := `{"model":"default","tools":[{"type":"function","function":{"name":"get_weather"}}],
	          "messages":[{"role":"user","content":"北京天气"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "tool_registration_failed") {
		t.Fatalf("soft-fail 不该报 tool_registration_failed:\n%s", rec.Body.String())
	}
}

func TestRemoveToolsClearsState(t *testing.T) {
	srv, _ := newToolTestServer(t, http.StatusNoContent, time.Hour, false)
	conv := &Conversation{Key: "f:k"}
	ctx := context.Background()
	if err := srv.ensureTools(ctx, conv, "/tmp", toolSpecs()); err != nil {
		t.Fatal(err)
	}
	if conv.mcpName == "" || srv.tools.ByKey("f:k") == nil {
		t.Fatal("注册后应有状态")
	}
	srv.removeTools(ctx, conv, "/tmp")
	if conv.mcpName != "" || conv.toolSess != nil || srv.tools.ByKey("f:k") != nil {
		t.Error("removeTools 未清理干净")
	}
	if !conv.toolsRegAt.IsZero() {
		t.Error("注册时间未清零")
	}
}

// ---------- 辅助 ----------

func funcSpec(name, desc string) ToolFunction {
	return ToolFunction{Name: name, Description: desc}
}

// 客户端回填的结果要能按顺序带出工具名（供 id 对不上时按名字兜底）；
// 且只应包含「当前这一轮」的结果，不能命中历史里的旧结果。
func TestOrderedToolResults(t *testing.T) {
	msgs := []ChatMessage{
		// 旧的一轮（同名 RunCommand，绝不能被匹配到）
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "old_1", Function: &FunctionCall{Name: "RunCommand"}}}},
		{Role: "tool", ToolCallID: "old_1", Content: MessageContent{Text: "OLD"}},
		// 当前这一轮
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_a", Function: &FunctionCall{Name: "RunCommand"}},
			{ID: "call_b", Function: &FunctionCall{Name: "Grep"}},
		}},
		{Role: "tool", ToolCallID: "call_a", Content: MessageContent{Text: "ok-a"}},
		{Role: "tool", ToolCallID: "call_b", Content: MessageContent{Text: "ok-b"}},
	}
	items := orderedToolResults(msgs)
	if len(items) != 2 {
		t.Fatalf("len=%d（应只含当前一轮）", len(items))
	}
	if items[0].Name != "RunCommand" || items[0].Result.Content != "ok-a" {
		t.Fatalf("item0=%+v", items[0])
	}
	if items[1].Name != "Grep" || items[1].Result.Content != "ok-b" {
		t.Fatalf("item1=%+v", items[1])
	}
}

// 工具调用超时后（waitResult 超时、agent 已释放）必须被 livePending 过滤掉，
// 否则客户端晚到的回填会触发"幽灵 resume"——在已中断的会话上空转后报错。
func TestLivePendingFiltersStale(t *testing.T) {
	p1 := &pendingCall{CallID: "call_1", ToolName: "SearchReplace", result: make(chan ToolResult, 1)}
	p2 := &pendingCall{CallID: "call_2", ToolName: "Grep", result: make(chan ToolResult, 1)}
	p3 := &pendingCall{CallID: "call_3", ToolName: "Grep", result: make(chan ToolResult, 1)}
	p1.markStale()
	p3.markStale()

	live := livePending([]*pendingCall{p1, p2, p3})
	if len(live) != 1 || live[0] != p2 {
		t.Fatalf("livePending 应只保留未超时的调用，got %+v", live)
	}
	// 全部超时时返回空 → 调用方走"按新轮次处理"而不是 resume
	if len(livePending([]*pendingCall{p1, p3})) != 0 {
		t.Fatal("全超时应返回空")
	}
}

// 三条路径（Chat / Anthropic / Responses）统一走 assignToolResults 的三级对齐：
// ① 精确 id → ② 工具名 → ③ 顺序。这里覆盖"客户端换成自己生成的 id"的场景。
func TestAssignToolResultsAlignment(t *testing.T) {
	logger := NewLogger("error")
	pending := []*pendingCall{
		{CallID: "call_1", ToolName: "SearchReplace"},
		{CallID: "call_2", ToolName: "Grep"},
	}
	// 客户端全部换成自己的 id（client_x），精确匹配全落空
	results := map[string]ToolResult{
		"client_1": {Content: "改了"},
		"client_2": {Content: "找到了"},
	}
	// items 带工具名（② 按名命中）
	items := []toolResultItem{
		{ID: "client_1", Name: "SearchReplace", Result: results["client_1"]},
		{ID: "client_2", Name: "Grep", Result: results["client_2"]},
	}
	assigned := assignToolResults(pending, results, items, logger)
	if len(assigned) != 2 {
		t.Fatalf("按名兜底应全部命中，got %+v", assigned)
	}
	if assigned["call_1"].Content != "改了" || assigned["call_2"].Content != "找到了" {
		t.Errorf("对齐结果错：%+v", assigned)
	}
	if logger == nil {
		t.Fatal("noop")
	}
}

// 名字也拿不到时按顺序兜底（③）。
func TestAssignToolResultsOrderFallback(t *testing.T) {
	logger := NewLogger("error")
	pending := []*pendingCall{
		{CallID: "call_1", ToolName: "SearchReplace"},
		{CallID: "call_2", ToolName: "Grep"},
	}
	results := map[string]ToolResult{"client_1": {Content: "A"}, "client_2": {Content: "B"}}
	// items 里没有工具名（Name 为空）→ 顺序兜底
	items := []toolResultItem{
		{ID: "client_1", Name: "", Result: results["client_1"]},
		{ID: "client_2", Name: "", Result: results["client_2"]},
	}
	assigned := assignToolResults(pending, results, items, logger)
	if assigned["call_1"].Content != "A" || assigned["call_2"].Content != "B" {
		t.Errorf("顺序兜底错：%+v", assigned)
	}
}

// RunCommand 这类「异步命令」工具必须补配套的状态查询工具，
// 否则 agent 发起 build 后看不到结果、卡死（客户端本地有但 tools[] 不声明）。
func TestAugmentCompanionTools(t *testing.T) {
	names := func(ts []ToolSpec) map[string]bool {
		out := map[string]bool{}
		for _, s := range ts {
			out[s.Function.Name] = true
		}
		return out
	}
	count := func(ts []ToolSpec, name string) int {
		n := 0
		for _, s := range ts {
			if s.Function.Name == name {
				n++
			}
		}
		return n
	}

	base := []ToolSpec{
		{Type: "function", Function: ToolFunction{Name: "Grep"}},
		{Type: "function", Function: ToolFunction{Name: "RunCommand"}},
	}
	out := augmentCompanionTools(base)
	got := names(out)
	if !got["Grep"] || !got["RunCommand"] {
		t.Fatalf("原工具丢失: %+v", got)
	}
	if !got["CheckCommandStatus"] || !got["check_command_status"] {
		t.Fatalf("应补两个配套状态查询工具，got %+v", got)
	}
	if count(out, "CheckCommandStatus") != 1 || count(out, "check_command_status") != 1 {
		t.Fatalf("配套工具重复: %+v", out)
	}

	// 配套工具要能被 agent 正确使用：入参会声明 command_id
	for _, name := range []string{"CheckCommandStatus", "check_command_status"} {
		found := false
		for _, s := range out {
			if s.Function.Name != name {
				continue
			}
			found = true
			var schema struct {
				Required []string                   `json:"required"`
				Props    map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(s.Function.Parameters, &schema); err != nil {
				t.Fatalf("%s schema 不是合法 JSON: %v", name, err)
			}
			if len(schema.Required) != 1 || schema.Required[0] != "command_id" {
				t.Errorf("%s 未要求 command_id: %s", name, s.Function.Parameters)
			}
			if _, ok := schema.Props["command_id"]; !ok {
				t.Errorf("%s schema 缺 command_id 属性", name)
			}
		}
		if !found {
			t.Fatalf("缺 %s", name)
		}
	}

	// 没触发工具 → 原样返回
	if got := augmentCompanionTools([]ToolSpec{{Function: ToolFunction{Name: "Grep"}}}); len(got) != 1 {
		t.Fatalf("无 RunCommand 不应补工具: %+v", got)
	}

	// 下划线命名也能触发
	if got := names(augmentCompanionTools([]ToolSpec{{Function: ToolFunction{Name: "run_command"}}})); !got["CheckCommandStatus"] {
		t.Fatalf("run_command 未触发补全: %+v", got)
	}

	// 客户端自己声明了同名工具 → 不重复
	decl := []ToolSpec{
		{Function: ToolFunction{Name: "RunCommand"}},
		{Function: ToolFunction{Name: "CheckCommandStatus"}},
	}
	out = augmentCompanionTools(decl)
	if count(out, "CheckCommandStatus") != 1 {
		t.Fatalf("客户端已声明的不应重复: %+v", out)
	}
	if !names(out)["check_command_status"] {
		t.Fatalf("仍应补另一个名字: %+v", out)
	}

	// 补全结果必须稳定（指纹=>每次重新注册，工具列表不能抖）
	a := augmentCompanionTools(base)
	if toolsFingerprint(a) != toolsFingerprint(augmentCompanionTools(base)) {
		t.Fatal("配套工具集不稳定")
	}
	if !normSorted(a) {
		t.Fatal("配套工具未排序")
	}
}

func normSorted(ts []ToolSpec) bool {
	for i := 1; i < len(ts); i++ {
		if ts[i].Function.Name < ts[i-1].Function.Name {
			return false
		}
	}
	return true
}

// 触发工具的描述里写明了配套状态查询工具名 → 原样识别，不靠猜也不补默认名。
func TestAugmentCompanionToolsDerivesFromDescription(t *testing.T) {
	base := []ToolSpec{{Function: ToolFunction{
		Name:        "RunCommand",
		Description: "运行命令。长任务请使用 get_command_status 工具轮询执行结果。",
	}}}
	out := augmentCompanionTools(base)
	got := map[string]bool{}
	for _, s := range out {
		got[s.Function.Name] = true
	}
	if !got["get_command_status"] {
		t.Fatalf("应从描述识别出 get_command_status，got %+v", got)
	}
	if got["CheckCommandStatus"] || got["check_command_status"] {
		t.Fatalf("描述已给出明确名字时不应再补默认名: %+v", got)
	}
}

// 配套状态查询工具失败时，桥要附上"换名重试"提示，agent 不用反复搜索卡住。
func TestCompanionErrorHint(t *testing.T) {
	if h := companionErrorHint("check_command_status"); h == "" || !strings.Contains(h, "CheckCommandStatus") {
		t.Fatalf("配套名应返回换名提示: %q", h)
	}
	if h := companionErrorHint("Grep"); h != "" {
		t.Fatalf("非配套名不应返回提示: %q", h)
	}
	// 大小写不敏感
	if companionErrorHint("CHECK_COMMAND_STATUS") == "" {
		t.Fatal("配套名判断应大小写不敏感")
	}
}
