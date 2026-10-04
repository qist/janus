package main

import (
	"strings"
	"testing"
)

// ---------- max_tokens 近似预算 ----------

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Errorf("空串应为 0，got %d", got)
	}
	// 纯 ASCII ≈ 4 字符/token
	ascii := strings.Repeat("a", 400)
	if got := estimateTokens(ascii); got < 90 || got > 110 {
		t.Errorf("400 个 ASCII 字符应约 100 token，got %d", got)
	}
	// 中文 ≈ 1.3 字符/token
	cjk := strings.Repeat("中", 130)
	if got := estimateTokens(cjk); got < 90 || got > 110 {
		t.Errorf("130 个汉字应约 100 token，got %d", got)
	}
	// 非空短串至少算 1
	if got := estimateTokens("a"); got < 1 {
		t.Errorf("短串至少 1，got %d", got)
	}
}

func TestEffectiveMaxTokens(t *testing.T) {
	a, b := 100, 200
	if got := effectiveMaxTokens(&a, nil); got != 100 {
		t.Errorf("max_tokens: %d", got)
	}
	// max_completion_tokens 优先
	if got := effectiveMaxTokens(&a, &b); got != 200 {
		t.Errorf("max_completion_tokens 应优先: %d", got)
	}
	zero := 0
	if got := effectiveMaxTokens(&zero, nil); got != 0 {
		t.Errorf("0 视为未设置: %d", got)
	}
	if got := effectiveMaxTokens(nil, nil); got != 0 {
		t.Errorf("都未设置应为 0: %d", got)
	}
}

func TestTokenBudget(t *testing.T) {
	if newTokenBudget(0) != nil {
		t.Fatal("limit<=0 应返回 nil")
	}
	var nilBudget *tokenBudget
	if !nilBudget.allow("任意内容") {
		t.Error("nil 预算应永远放行")
	}

	b := newTokenBudget(10) // 约 40 个 ASCII 字符
	if !b.allow(strings.Repeat("a", 20)) {
		t.Fatal("前 20 字符应放行")
	}
	if b.truncated {
		t.Fatal("还没超限")
	}
	if b.allow(strings.Repeat("a", 100)) {
		t.Fatal("超出后应拒绝")
	}
	if !b.truncated {
		t.Fatal("应标记截断")
	}
	// 截断后继续拒绝
	if b.allow("x") {
		t.Fatal("截断后应一直拒绝")
	}
}

func TestTruncateToBudget(t *testing.T) {
	s := strings.Repeat("a", 400) // ≈100 token
	got, trunc := truncateToBudget(s, 50)
	if !trunc {
		t.Fatal("应发生截断")
	}
	if len(got) == 0 || len(got) >= len(s) {
		t.Fatalf("截断长度异常: %d", len(got))
	}
	if estimateTokens(got) > 50 {
		t.Errorf("截断后仍超预算: %d", estimateTokens(got))
	}
	// 不超预算时原样返回
	short := "你好"
	if got, trunc := truncateToBudget(short, 100); trunc || got != short {
		t.Errorf("不该截断: %q %v", got, trunc)
	}
	// limit<=0 表示不限
	if got, trunc := truncateToBudget(s, 0); trunc || got != s {
		t.Error("limit<=0 应原样返回")
	}
}

// 端到端：max_tokens 截断后 finish_reason 必须变成 length。
func TestExecutorBudgetSetsLength(t *testing.T) {
	ex := newExecutor(&Server{log: NewLogger("error")}, "s", "m", false)
	ex.budget = newTokenBudget(5) // 约 20 个 ASCII 字符

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
	ex := newExecutor(&Server{log: NewLogger("error")}, "s", "m", false)
	_ = ex.addText(strings.Repeat("x", 10000))
	res := ex.snapshot()
	if res.truncated || res.finish == "length" {
		t.Fatalf("无预算不该截断: %+v", res)
	}
}
