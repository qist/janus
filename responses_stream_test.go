package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func collectResponsesEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	var etype string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			etype = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if raw == "" || raw == "[DONE]" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("bad data: %s", raw)
			}
			m["_event"] = etype
			out = append(out, m)
		}
	}
	return out
}

func names(evs []map[string]any) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		s, _ := e["_event"].(string)
		out = append(out, s)
	}
	return out
}

// 文本回合：reasoning + message 的完整生命周期，且事件里的 item id 与最终 output 一致。
func TestResponsesStreamTextLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	ss, err := newResponsesSSE(rec)
	if err != nil {
		t.Fatal(err)
	}
	st := newResponsesStream(ss)
	st.reasoning("想一下")
	st.text("你好")
	st.closeAll()

	evs := collectResponsesEvents(t, rec.Body.String())
	got := names(evs)
	want := []string{
		"response.output_item.added", // reasoning idx0
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added", // message idx1
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列不符:\n got=%v\nwant=%v", got, want)
	}
	if len(st.items) != 2 || st.items[0].Type != "reasoning" || st.items[1].Type != "message" {
		t.Fatalf("items: %+v", st.items)
	}
	if st.items[1].Content[0].Text != "你好" || st.items[1].Status != "completed" {
		t.Fatalf("message item: %+v", st.items[1])
	}
	// 事件里引用的 item id 必须等于最终 output 的 id
	addedIDs := []string{}
	for _, e := range evs {
		if e["_event"] == "response.output_item.added" {
			it, _ := e["item"].(map[string]any)
			addedIDs = append(addedIDs, it["id"].(string))
		}
	}
	if len(addedIDs) != 2 || addedIDs[0] != st.items[0].ID || addedIDs[1] != st.items[1].ID {
		t.Fatalf("item id 与最终 output 不一致: events=%v items=%s/%s",
			addedIDs, st.items[0].ID, st.items[1].ID)
	}
}

// 工具回合：reasoning + function_call（index 递增，delta/done 齐全）。
func TestResponsesStreamFunctionCallLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	ss, _ := newResponsesSSE(rec)
	st := newResponsesStream(ss)
	st.reasoning("需要查天气")
	st.functionCalls([]*pendingCall{
		{CallID: "call_1", ToolName: "get_weather", Args: `{"city":"北京"}`},
		{CallID: "call_2", ToolName: "get_time", Args: `{"tz":"CST"}`},
	})
	st.closeAll()

	evs := collectResponsesEvents(t, rec.Body.String())
	got := names(evs)
	// reasoning 项占 index 0，两个 function_call 应为 index 1、2
	idxs := []int{}
	for _, e := range evs {
		if e["_event"] == "response.output_item.added" {
			if it, ok := e["item"].(map[string]any); ok && it["type"] == "function_call" {
				idxs = append(idxs, int(e["output_index"].(float64)))
			}
		}
	}
	if len(idxs) != 2 || idxs[0] != 1 || idxs[1] != 2 {
		t.Fatalf("function_call output_index 应递增 1,2: %v (%v)", idxs, got)
	}
	if strings.Count(strings.Join(got, ","), "response.function_call_arguments.delta") != 2 ||
		strings.Count(strings.Join(got, ","), "response.function_call_arguments.done") != 2 {
		t.Fatalf("delta/done 数量不对: %v", got)
	}
	if len(st.items) != 3 || st.items[1].Type != "function_call" || st.items[2].CallID != "call_2" {
		t.Fatalf("items: %+v", st.items)
	}
}
