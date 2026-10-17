package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// sseWriter 捕获：capOn 时 writeChunk/toolCalls/finish/done 全部留底。
func TestSSEWriterCapture(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	sw.capOn = true
	_ = sw.begin()
	_ = sw.toolCalls([]*pendingCall{{
		CallID:   "call_cap1",
		ToolName: "Read",
		MCPName:  "ob_s_x_Read",
		Args:     `{"p":1}`,
	}})
	_ = sw.finish("tool_calls", nil, false)

	cap := sw.captureBytes()
	if cap == nil {
		t.Fatal("capOn 时应有捕获")
	}
	for _, want := range []string{`"role":"assistant"`, "call_cap1", `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(string(cap), want) {
			t.Fatalf("捕获缺 %q:\n%s", want, cap)
		}
	}
	if !strings.HasPrefix(string(cap), "data: ") {
		t.Fatalf("捕获应以 data: 开头:\n%s", cap)
	}

	// 未开启时捕获为 nil
	rec2 := httptest.NewRecorder()
	sw2, _ := newSSE(rec2, "m")
	sw2.capOn = false
	_ = sw2.begin()
	if sw2.captureBytes() != nil {
		t.Fatal("capOn=false 不应有捕获")
	}
}
