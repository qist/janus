package api

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesSpecRequired 是 OpenAI OpenAPI 规范里各流式事件的必填字段
// （来源：openai-openapi openapi.yaml，Response*Event schema 的 required）。
// 只列 janus 会发出的事件；新增事件时同步补这里。
var responsesSpecRequired = map[string][]string{
	"response.created":                       {"response", "sequence_number", "type"},
	"response.in_progress":                   {"response", "sequence_number", "type"},
	"response.completed":                     {"response", "sequence_number", "type"},
	"response.failed":                        {"response", "sequence_number", "type"},
	"response.output_item.added":             {"output_index", "item", "sequence_number", "type"},
	"response.output_item.done":              {"output_index", "item", "sequence_number", "type"},
	"response.content_part.added":            {"item_id", "output_index", "content_index", "part", "sequence_number", "type"},
	"response.content_part.done":             {"item_id", "output_index", "content_index", "part", "sequence_number", "type"},
	"response.output_text.delta":             {"item_id", "output_index", "content_index", "delta", "logprobs", "sequence_number", "type"},
	"response.output_text.done":              {"item_id", "output_index", "content_index", "text", "logprobs", "sequence_number", "type"},
	"response.reasoning_summary_part.added":  {"item_id", "output_index", "summary_index", "part", "sequence_number", "type"},
	"response.reasoning_summary_part.done":   {"item_id", "output_index", "summary_index", "part", "sequence_number", "type"},
	"response.reasoning_summary_text.delta":  {"item_id", "output_index", "summary_index", "delta", "sequence_number", "type"},
	"response.reasoning_summary_text.done":   {"item_id", "output_index", "summary_index", "text", "sequence_number", "type"},
	"response.function_call_arguments.delta": {"item_id", "output_index", "delta", "sequence_number", "type"},
	"response.function_call_arguments.done":  {"item_id", "output_index", "arguments", "sequence_number", "type"},
}

// parseSSE 从 SSE 文本里取出每个事件类型的首个 data 对象。
func parseSSE(t *testing.T, raw string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			continue
		}
		typ, _ := obj["type"].(string)
		if typ == "" {
			continue
		}
		if _, ok := out[typ]; !ok {
			out[typ] = obj
		}
	}
	return out
}

// emitSampleStream 走一遍完整的事件序列：created → in_progress →
// reasoning → text → function_call → completed。
func emitSampleStream(t *testing.T, withReasoning bool) string {
	t.Helper()
	rec := httptest.NewRecorder()
	ss, err := newResponsesSSE(rec)
	if err != nil {
		t.Fatal(err)
	}
	st := newResponsesStream(ss)

	skel := &ResponsesResponse{ID: "resp_1", Object: "response", Status: "in_progress",
		Model: "janus", Output: []ResponseItem{}, Tools: []ResponseTool{}}
	st.event("response.created", map[string]any{"response": skel})
	inProg := *skel
	st.event("response.in_progress", map[string]any{"response": &inProg})

	if withReasoning {
		st.reasoning("思考中")
	}
	st.text("你好")
	st.closeAll()
	st.functionCalls([]*pendingCall{{CallID: "call_1", ToolName: "get_weather", Args: `{"city":"BJ"}`}})

	final := *skel
	final.Status = "completed"
	final.Output = st.items
	final.Usage = &ResponsesUsage{} // 生产路径经 usageOrZero 恒带 usage
	st.event("response.completed", map[string]any{"response": &final})
	ss.markClosed()

	return rec.Body.String()
}

// response.completed 事件里的 response 必带 usage：官方流恒带（工具调用轮也带，
// 至少 input tokens），缺失会被严格解析器（实测 Trae）判整个事件非法 →
// 静默丢弃 → 客户端不执行 function_call。
func TestResponsesSSECompletedCarriesUsage(t *testing.T) {
	raw := emitSampleStream(t, false)
	events := parseSSE(t, raw)
	comp, ok := events["response.completed"]
	if !ok {
		t.Fatal("无 response.completed 事件")
	}
	resp, ok := comp["response"].(map[string]any)
	if !ok {
		t.Fatalf("response 字段类型异常: %T", comp["response"])
	}
	if _, present := resp["usage"]; !present {
		t.Error("response.completed 的 response 缺 usage 字段")
	}
}

// TestResponsesSSEEventFieldsMatchSpec 锁死所有 SSE 事件的必填字段，
// 防止再次出现客户端（Trae 等）因缺 item_id / logprobs 判定"空流"的回归。
func TestResponsesSSEEventFieldsMatchSpec(t *testing.T) {
	for _, withReasoning := range []bool{true, false} {
		raw := emitSampleStream(t, withReasoning)
		events := parseSSE(t, raw)
		if len(events) == 0 {
			t.Fatalf("reasoning=%v 未解析到任何事件", withReasoning)
		}
		for typ, required := range responsesSpecRequired {
			obj, ok := events[typ]
			if !ok {
				continue // 该事件本轮未发出
			}
			var missing []string
			for _, k := range required {
				if _, present := obj[k]; !present {
					missing = append(missing, k)
				}
			}
			if len(missing) > 0 {
				t.Errorf("reasoning=%v %s 缺必填字段 %v；实际字段 %v",
					withReasoning, typ, missing, keysOf(obj))
			}
		}
	}
}

// TestResponsesSSENoNullOrphanFields 断言事件不输出 null 型可选字段。
func TestResponsesSSENoNullOrphanFields(t *testing.T) {
	raw := emitSampleStream(t, true)
	for _, bad := range []string{
		`"output":null`, `"tools":null`, `"previous_response_id":null`,
		`"instructions":null`, `"error":null`, `"usage":null`,
	} {
		if strings.Contains(raw, bad) {
			t.Errorf("SSE 不应包含 %s", bad)
		}
	}
}

// TestResponsesSSESequenceMonotonic 断言 sequence_number 自 0 起严格递增。
func TestResponsesSSESequenceMonotonic(t *testing.T) {
	raw := emitSampleStream(t, true)
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	want := 0
	got := 0
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			continue
		}
		if _, ok := obj["type"]; !ok {
			continue
		}
		seq, ok := obj["sequence_number"].(float64)
		if !ok {
			t.Fatalf("第 %d 个事件缺 sequence_number: %v", want, obj)
		}
		if int(seq) != want {
			t.Fatalf("sequence_number 应为 %d，实际 %v（type=%v）", want, seq, obj["type"])
		}
		want++
		got++
	}
	if got == 0 {
		t.Fatal("未解析到事件")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
