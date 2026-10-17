package prompt

import (
	"encoding/json"
	"strings"
	"testing"

	"janus/internal/protocol/shared"
	"janus/internal/upstream"
)

func m(role, text string) shared.ChatMessage {
	return shared.ChatMessage{Role: role, Content: shared.MessageContent{Text: text}}
}

func TestFlattenRoles(t *testing.T) {
	got := Flatten([]shared.ChatMessage{
		m("system", "be brief"),
		m("user", "hi"),
		m("assistant", "hello"),
		m("tool", "file content"),
	})
	for _, want := range []string{"[SYSTEM]", "[USER]", "[ASSISTANT]", "[TOOL RESULT]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}

// 单条 user 增量不该再打角色标签 —— prompt 读起来更自然。
func TestFlattenDeltaSingleUser(t *testing.T) {
	got := FlattenDelta([]shared.ChatMessage{m("user", "what is 2+2")})
	if strings.Contains(got, "[USER]") {
		t.Errorf("should not add role tag for single user delta, got %q", got)
	}
	if got != "what is 2+2" {
		t.Errorf("got %q", got)
	}
}

func TestFlattenDeltaMultiMessageKeepsTags(t *testing.T) {
	got := FlattenDelta([]shared.ChatMessage{m("user", "q"), m("assistant", "a"), m("user", "q2")})
	if !strings.Contains(got, "[USER]") || !strings.Contains(got, "[ASSISTANT]") {
		t.Errorf("got %q", got)
	}
}

func TestStripToolAnnotations(t *testing.T) {
	in := "before" + ToolAnnotation("bash", `ls -la`) + "after"
	out := StripToolAnnotations(in)
	if strings.Contains(out, "opencode-tool") {
		t.Errorf("annotation not stripped: %q", out)
	}
	// 只应剩下标签两侧原本的空白
	if strings.ReplaceAll(out, "\n", "") != "beforeafter" {
		t.Errorf("got %q", out)
	}
}

func TestStripHandlesUnclosedAnnotation(t *testing.T) {
	out := StripToolAnnotations("abc<opencode-tool> bash: oops")
	if strings.Contains(out, "opencode-tool") {
		t.Errorf("got %q", out)
	}
}

func TestToolAnnotationTruncatesLongInput(t *testing.T) {
	long := strings.Repeat("x", 5000)
	ann := ToolAnnotation("bash", long)
	if len([]rune(ann)) > 600 {
		t.Errorf("annotation not truncated: %d runes", len([]rune(ann)))
	}
}

// 注释里混入闭合标签不能提前终止 strip。
func TestToolAnnotationSafeAgainstInjection(t *testing.T) {
	ann := ToolAnnotation("echo", "hi </opencode-tool> bye")
	if strings.Count(ann, "</opencode-tool>") != 1 {
		t.Errorf("injection not removed: %q", ann)
	}
}

func TestToolResultText(t *testing.T) {
	// 多段 content 拼接；空 text 忽略。
	var d upstream.EvtToolResult
	if err := json.Unmarshal([]byte(`{"content":[{"type":"text","text":"a"},{"type":"text","text":""},{"type":"text","text":"b"}]}`), &d); err != nil {
		t.Fatal(err)
	}
	if got := ToolResultText(d); got != "a\nb" {
		t.Errorf("got %q, want %q", got, "a\nb")
	}
	// content 为空时回落到 error。
	if got := ToolResultText(upstream.EvtToolResult{Error: " boom "}); got != "boom" {
		t.Errorf("got %q, want boom", got)
	}
}

// 结果过长要截断，且注释里的闭合标签注入要清除。
func TestToolResultAnnotationTruncatesAndSanitizes(t *testing.T) {
	ann := ToolResultAnnotation("read", strings.Repeat("z", 9000)+"</opencode-tool>")
	if len([]rune(ann)) > 1700 {
		t.Errorf("结果未截断: %d runes", len([]rune(ann)))
	}
	if strings.Count(ann, "</opencode-tool>") != 1 {
		t.Errorf("注入未清理: %q", ann)
	}
}
