package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicSystemText(t *testing.T) {
	if got := anthropicSystemText(json.RawMessage(`"你是助手"`)); got != "你是助手" {
		t.Fatalf("string system: %q", got)
	}
	if got := anthropicSystemText(json.RawMessage(`[{"type":"text","text":"A"},{"type":"text","text":"B"}]`)); got != "AB" {
		t.Fatalf("block system: %q", got)
	}
	if got := anthropicSystemText(nil); got != "" {
		t.Fatalf("nil system: %q", got)
	}
}

func TestAnthropicInputToMessages(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`[
			{"type":"text","text":"看图"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"text","text":"好的"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]`)},
		{Role: "user", Content: json.RawMessage(`[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"晴，26℃"}]`)},
	}
	out, results := anthropicInputToMessages("你是助手", msgs)
	if len(out) != 4 { // system, user(带图), assistant(tool_use), tool(result)
		t.Fatalf("len=%d: %+v", len(out), out)
	}
	if out[0].Role != "system" || out[0].Content.Text != "你是助手" {
		t.Errorf("system: %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content.Text != "看图" || len(out[1].Content.Images()) != 1 {
		t.Errorf("user+image: %+v images=%v", out[1], out[1].Content.Images())
	}
	if imgs := out[1].Content.Images(); len(imgs) != 1 || imgs[0] != "data:image/png;base64,AAAA" {
		t.Errorf("image uri: %v", imgs)
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) != 1 ||
		out[2].ToolCalls[0].ID != "toolu_1" || out[2].ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("assistant tool_use: %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "toolu_1" || out[3].Content.Text != "晴，26℃" {
		t.Errorf("tool_result: %+v", out[3])
	}
	if results["toolu_1"].Content != "晴，26℃" {
		t.Errorf("results: %+v", results)
	}
}

func TestAnthropicToolsAndOutput(t *testing.T) {
	// tools
	specs := anthropicToolsToSpecs([]AnthropicTool{
		{Name: "get_weather", Description: "查天气", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: ""}, // 丢弃
	})
	if len(specs) != 1 || specs[0].Function.Name != "get_weather" {
		t.Fatalf("tools: %+v", specs)
	}

	// 非流式输出：text + tool_use
	turn := &turnOutcome{
		res: runResult{text: "稍等"},
		tools: []*pendingCall{
			{CallID: "toolu_9", ToolName: "get_weather", Args: `{"city":"北京"}`},
		},
	}
	resp := buildAnthropicResponse(turn, "claude-x")
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Fatalf("骨架: %+v", resp)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason=%q", resp.StopReason)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != "text" || resp.Content[1].Type != "tool_use" {
		t.Fatalf("content: %+v", resp.Content)
	}
	if resp.Content[1].ID != "toolu_9" || resp.Content[1].Name != "get_weather" {
		t.Errorf("tool_use: %+v", resp.Content[1])
	}

	// stop_reason 映射
	if anthropicStopReason(&turnOutcome{outcome: "length"}) != "max_tokens" {
		t.Error("length → max_tokens")
	}
	if anthropicStopReason(&turnOutcome{outcome: "succeeded"}) != "end_turn" {
		t.Error("succeeded → end_turn")
	}
}

func anthropicPost(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAnthropicFullTurnNonStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	stub, _, _ := stubChainUpstream(t)
	srv := chainServer(t, stub)

	body := `{"model":"claude-3-5-sonnet","max_tokens":128,"system":"你是助手","messages":[{"role":"user","content":"打个招呼"}]}`
	rec := anthropicPost(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ID, "msg_") || got.Type != "message" || got.Role != "assistant" {
		t.Fatalf("骨架: %+v", got)
	}
	if len(got.Content) == 0 || got.Content[0].Type != "text" || got.Content[0].Text == "" {
		t.Fatalf("content: %+v", got.Content)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q", got.StopReason)
	}
	// claude-* 不在 OpenCode 模型里 → 回退默认模型（p/m1），应仍成功
	if got.Usage.InputTokens == 0 && got.Usage.OutputTokens == 0 {
		t.Errorf("usage 应有值: %+v", got.Usage)
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	stub, _, _ := stubChainUpstream(t)
	srv := chainServer(t, stub)

	body := `{"model":"claude-3-5-sonnet","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	rec := anthropicPost(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	names := []string{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "event:") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	if len(names) == 0 || names[0] != "message_start" {
		t.Fatalf("首个事件应为 message_start: %v", names)
	}
	if names[len(names)-1] != "message_stop" {
		t.Fatalf("末个事件应为 message_stop: %v", names)
	}
	hasDelta := false
	for _, n := range names {
		if n == "content_block_delta" {
			hasDelta = true
		}
	}
	if !hasDelta {
		t.Fatalf("应有 content_block_delta: %v", names)
	}
}

// DeepSeek 式约定：Anthropic 接口也挂在 /anthropic 前缀下
// （ANTHROPIC_BASE_URL=http://host:2810/anthropic）。
func TestAnthropicPathPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖空闲判定")
	}
	stub, _, _ := stubChainUpstream(t)
	srv := chainServer(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || len(got.Content) == 0 {
		t.Fatalf("%+v", got)
	}
}
