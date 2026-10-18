package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCanTranslate(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{ProtocolAnthropic, ProtocolOpenAIChat, true},
		{ProtocolOpenAIChat, ProtocolAnthropic, true},
		{ProtocolOpenAIChat, ProtocolOpenAIResponses, false},
		{ProtocolOpenAIResponses, ProtocolAnthropic, false},
		{ProtocolAnthropic, ProtocolAnthropic, false},
	}
	for _, c := range cases {
		if got := CanTranslate(c.from, c.to); got != c.want {
			t.Errorf("CanTranslate(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTranslateRequestAnthropicToOpenAI(t *testing.T) {
	body := []byte(`{
		"model":"claude-x","max_tokens":64,"temperature":0.2,"top_p":0.9,
		"stop_sequences":["END"],
		"system":[{"type":"text","text":"sys-a"},{"type":"text","text":" sys-b"}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"text","text":"sure"},
				{"type":"tool_use","id":"tu1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"res"}]}]}
		],
		"tools":[
			{"name":"lookup","description":"d","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"tool_choice":{"type":"auto"}
	}`)
	out, err := TranslateRequest(ProtocolAnthropic, ProtocolOpenAIChat, body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model      string           `json:"model"`
		MaxTokens  int              `json:"max_tokens"`
		Messages   []map[string]any `json:"messages"`
		Tools      []map[string]any `json:"tools"`
		ToolChoice any              `json:"tool_choice"`
		Stop       []string         `json:"stop"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-x" || got.MaxTokens != 64 {
		t.Errorf("model/max_tokens = %q/%d", got.Model, got.MaxTokens)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "END" {
		t.Errorf("stop = %+v", got.Stop)
	}
	if got.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v", got.ToolChoice)
	}
	// system 合并成一条 message，后接 user / assistant / tool
	if len(got.Messages) != 4 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if got.Messages[0]["role"] != "system" || got.Messages[0]["content"] != "sys-a sys-b" {
		t.Errorf("system = %+v", got.Messages[0])
	}
	if got.Messages[2]["role"] != "assistant" {
		t.Errorf("assistant role = %v", got.Messages[2]["role"])
	}
	calls, _ := got.Messages[2]["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v", calls)
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "tu1" || fn["name"] != "lookup" || fn["arguments"] != `{"q":"x"}` {
		t.Errorf("tool call = %+v", call)
	}
	if got.Messages[3]["role"] != "tool" || got.Messages[3]["tool_call_id"] != "tu1" ||
		got.Messages[3]["content"] != "res" {
		t.Errorf("tool result = %+v", got.Messages[3])
	}
	// 服务端工具（web_search）没有 OpenAI 对应物，跳过。
	if len(got.Tools) != 1 || got.Tools[0]["type"] != "function" {
		t.Errorf("tools = %+v", got.Tools)
	}
}

func TestTranslateRequestOpenAIToAnthropic(t *testing.T) {
	body := []byte(`{
		"model":"gpt-x","max_completion_tokens":24,"stop":"END",
		"messages":[
			{"role":"developer","content":"dev"},
			{"role":"system","content":"sys"},
			{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},
			{"role":"assistant","content":"sure","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	out, err := TranslateRequest(ProtocolOpenAIChat, ProtocolAnthropic, body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model         string           `json:"model"`
		System        string           `json:"system"`
		MaxTokens     int              `json:"max_tokens"`
		StopSequences []string         `json:"stop_sequences"`
		Messages      []map[string]any `json:"messages"`
		Tools         []map[string]any `json:"tools"`
		ToolChoice    map[string]any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens != 24 {
		t.Errorf("max_tokens = %d, want max_completion_tokens fallback", got.MaxTokens)
	}
	if got.System != "dev\nsys" {
		t.Errorf("system = %q", got.System)
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "END" {
		t.Errorf("stop_sequences = %+v", got.StopSequences)
	}
	if got.ToolChoice["type"] != "tool" || got.ToolChoice["name"] != "f" {
		t.Errorf("tool_choice = %+v", got.ToolChoice)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	userBlocks, _ := got.Messages[0]["content"].([]any)
	if len(userBlocks) != 2 {
		t.Fatalf("user blocks = %+v", userBlocks)
	}
	img, _ := userBlocks[1].(map[string]any)
	src, _ := img["source"].(map[string]any)
	if img["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Errorf("image block = %+v", img)
	}
	asstBlocks, _ := got.Messages[1]["content"].([]any)
	if len(asstBlocks) != 2 {
		t.Fatalf("assistant blocks = %+v", asstBlocks)
	}
	toolUse, _ := asstBlocks[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "c1" {
		t.Errorf("tool_use = %+v", toolUse)
	}
	resultBlocks, _ := got.Messages[2]["content"].([]any)
	res, _ := resultBlocks[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "c1" || res["content"] != "ok" {
		t.Errorf("tool_result = %+v", res)
	}
}

func TestTranslateResponseBothWays(t *testing.T) {
	anth := []byte(`{"id":"msg_1","model":"claude-x","content":[
		{"type":"text","text":"hello"},
		{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":5}}`)
	oai, err := TranslateResponse(ProtocolAnthropic, ProtocolOpenAIChat, anth, "vendor/claude-x")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(oai, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "vendor/claude-x" || got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("openai response = %+v", got)
	}
	if got.Choices[0].Message.Content != "hello" || got.Usage.TotalTokens != 8 {
		t.Errorf("openai content/usage = %+v / %+v", got.Choices[0].Message, got.Usage)
	}

	back, err := TranslateResponse(ProtocolOpenAIChat, ProtocolAnthropic, oai, "")
	if err != nil {
		t.Fatal(err)
	}
	var anthOut struct {
		Type      string `json:"type"`
		Stop      string `json:"stop_reason"`
		Content   []map[string]any
		Usage     map[string]int `json:"usage"`
		StopSeq   any            `json:"stop_sequence"`
		ModelText string         `json:"model"`
	}
	if err := json.Unmarshal(back, &anthOut); err != nil {
		t.Fatal(err)
	}
	if anthOut.Type != "message" || anthOut.Stop != "tool_use" || len(anthOut.Content) != 2 {
		t.Errorf("anthropic response = %+v", anthOut)
	}
}

// 流式：Anthropic 事件 → OpenAI chunk。
func TestTranslateStreamAnthropicToOpenAI(t *testing.T) {
	src := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := TranslateStream(&out, strings.NewReader(src), ProtocolAnthropic, ProtocolOpenAIChat, "vendor/claude-x"); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("missing text delta: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", body)
	}
	if !strings.Contains(body, `"model":"vendor/claude-x"`) {
		t.Errorf("model echo missing: %s", body)
	}
}

// 流式：OpenAI chunk → Anthropic 事件，工具调用要拆成 tool_use 块。
func TestTranslateStreamOpenAIToAnthropic(t *testing.T) {
	src := strings.Join([]string{
		`data: {"id":"chatcmpl-1","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{\"q\":"}}]}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := TranslateStream(&out, strings.NewReader(src), ProtocolOpenAIChat, ProtocolAnthropic, "vendor/gpt-x"); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{
		"event: message_start",
		`"text":"hi","type":"text_delta"`,
		`"id":"c1"`,
		`"name":"f"`,
		`"type":"tool_use"`,
		`"partial_json":"{\"q\":"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in stream:\n%s", want, body)
		}
	}
}

func TestTranslateError(t *testing.T) {
	oai := TranslateError(ProtocolOpenAIChat, 401, []byte(`{"error":{"message":"bad key"}}`))
	if !strings.Contains(string(oai), `"message":"bad key"`) {
		t.Errorf("openai error = %s", oai)
	}
	anth := TranslateError(ProtocolAnthropic, 429, []byte(`{"error":{"message":"slow down"}}`))
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(anth, &v); err != nil {
		t.Fatal(err)
	}
	if v.Type != "error" || v.Error.Type != "rate_limit_error" || v.Error.Message != "slow down" {
		t.Errorf("anthropic error = %+v", v)
	}
}
