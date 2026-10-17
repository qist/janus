package api

import (
	"strings"
	"testing"

	"janus/internal/logx"
	"janus/internal/tokenbudget"
)

// 端到端：max_tokens 截断后 finish_reason 必须变成 length。
func TestExecutorBudgetSetsLength(t *testing.T) {
	ex := newExecutor(&Server{log: logx.NewLogger("error")}, "s", "m", false)
	ex.budget = tokenbudget.NewBudget(5) // 约 20 个 ASCII 字符

	_ = ex.addText(strings.Repeat("a", 10))
	_ = ex.addText(strings.Repeat("b", 100)) // 触发截断

	res := ex.snapshot()
	if !res.truncated {
		t.Fatal("应标记截断")
	}
	if res.finish != "length" {
		t.Fatalf("finish 应为 length，got %q", res.finish)
	}
	if strings.Contains(res.text, "b") {
		t.Fatal("截断后的内容不该进入正文")
	}
	if len(res.text) != 10 {
		t.Fatalf("正文长度 = %d, want 10", len(res.text))
	}
}

func TestExecutorNoBudgetNoTruncation(t *testing.T) {
	ex := newExecutor(&Server{log: logx.NewLogger("error")}, "s", "m", false)
	_ = ex.addText(strings.Repeat("x", 10000))
	res := ex.snapshot()
	if res.truncated || res.finish == "length" {
		t.Fatalf("无预算不该截断: %+v", res)
	}
}
