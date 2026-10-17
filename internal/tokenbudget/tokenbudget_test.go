package tokenbudget

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("空串应为 0，got %d", got)
	}
	// 纯 ASCII ≈ 4 字符/token
	ascii := strings.Repeat("a", 400)
	if got := EstimateTokens(ascii); got < 90 || got > 110 {
		t.Errorf("400 个 ASCII 字符应约 100 token，got %d", got)
	}
	// 中文 ≈ 1.3 字符/token
	cjk := strings.Repeat("中", 130)
	if got := EstimateTokens(cjk); got < 90 || got > 110 {
		t.Errorf("130 个汉字应约 100 token，got %d", got)
	}
	// 非空短串至少算 1
	if got := EstimateTokens("a"); got < 1 {
		t.Errorf("短串至少 1，got %d", got)
	}
}

func TestEffectiveMaxTokens(t *testing.T) {
	a, b := 100, 200
	if got := EffectiveMaxTokens(&a, nil); got != 100 {
		t.Errorf("max_tokens: %d", got)
	}
	// max_completion_tokens 优先
	if got := EffectiveMaxTokens(&a, &b); got != 200 {
		t.Errorf("max_completion_tokens 应优先: %d", got)
	}
	zero := 0
	if got := EffectiveMaxTokens(&zero, nil); got != 0 {
		t.Errorf("0 视为未设置: %d", got)
	}
	if got := EffectiveMaxTokens(nil, nil); got != 0 {
		t.Errorf("都未设置应为 0: %d", got)
	}
}

func TestTokenBudget(t *testing.T) {
	if NewBudget(0) != nil {
		t.Fatal("limit<=0 应返回 nil")
	}
	var nilBudget *Budget
	if !nilBudget.Allow("任意内容") {
		t.Error("nil 预算应永远放行")
	}

	b := NewBudget(10) // 约 40 个 ASCII 字符
	if !b.Allow(strings.Repeat("a", 20)) {
		t.Fatal("前 20 字符应放行")
	}
	if b.Truncated() {
		t.Fatal("还没超限")
	}
	if b.Allow(strings.Repeat("a", 100)) {
		t.Fatal("超出后应拒绝")
	}
	if !b.Truncated() {
		t.Fatal("应标记截断")
	}
	// 截断后继续拒绝
	if b.Allow("x") {
		t.Fatal("截断后应一直拒绝")
	}
}

func TestTruncateToBudget(t *testing.T) {
	s := strings.Repeat("a", 400) // ≈100 token
	got, trunc := TruncateToBudget(s, 50)
	if !trunc {
		t.Fatal("应发生截断")
	}
	if len(got) == 0 || len(got) >= len(s) {
		t.Fatalf("截断长度异常: %d", len(got))
	}
	if EstimateTokens(got) > 50 {
		t.Errorf("截断后仍超预算: %d", EstimateTokens(got))
	}
	// 不超预算时原样返回
	short := "你好"
	if got, trunc := TruncateToBudget(short, 100); trunc || got != short {
		t.Errorf("不该截断: %q %v", got, trunc)
	}
	// limit<=0 表示不限
	if got, trunc := TruncateToBudget(s, 0); trunc || got != s {
		t.Error("limit<=0 应原样返回")
	}
}
