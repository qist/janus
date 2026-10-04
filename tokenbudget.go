package main

import (
	"math"
	"strings"
)

// tokenbudget.go —— max_tokens / max_completion_tokens 的尽力映射。
//
// 上游（OpenCode）的 prompt 接口没有任何输出上限参数，agent 的 request.body
// 虽然能带 provider 级覆盖，但只能在配置里静态设定，无法按请求传入。
//
// 所以桥侧尽力做到「标准语义」：
//   - 客户端给了 max_tokens / max_completion_tokens 时，按近似 token 数截断输出
//   - 截断时 finish_reason = "length"（与 OpenAI 一致）
//
// 局限（已写进文档）：计数是**估算**，不是精确 tokenizer。
// 中文 ≈ 1.3 字符/token，ASCII ≈ 4 字符/token。会与真实值有偏差，
// 因此只用于"别超出太多"的软限制，不保证精确等于客户端请求的上限。

// estimateTokens 近似估算一段文本的 token 数。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var ascii, nonASCII int
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	est := float64(ascii)/4.0 + float64(nonASCII)/1.3
	if est < 1 {
		est = 1
	}
	return int(math.Ceil(est))
}

// effectiveMaxTokens 取客户端请求的输出上限。
// max_completion_tokens 是新字段，优先于旧的 max_tokens（与 OpenAI 一致）。
func effectiveMaxTokens(maxTokens, maxCompletionTokens *int) int {
	if maxCompletionTokens != nil && *maxCompletionTokens > 0 {
		return *maxCompletionTokens
	}
	if maxTokens != nil && *maxTokens > 0 {
		return *maxTokens
	}
	return 0
}

// tokenBudget 是一个近似的输出预算，超出后拒绝继续追加正文。
type tokenBudget struct {
	limit     int
	used      int
	truncated bool
}

func newTokenBudget(limit int) *tokenBudget {
	if limit <= 0 {
		return nil
	}
	return &tokenBudget{limit: limit}
}

// allow 判断这段增量能否纳入预算。
// 返回 false 表示已超限，调用方应丢弃该增量并结束正文输出。
func (b *tokenBudget) allow(s string) bool {
	if b == nil || s == "" {
		// 没有预算（客户端未指定 max_tokens）→ 一律放行
		return true
	}
	if b.truncated {
		return false
	}
	b.used += estimateTokens(s)
	if b.used > b.limit {
		b.truncated = true
		return false
	}
	return true
}

// truncate 按预算截断一段完整文本（非流式用）。
// 返回截断后的文本与是否发生截断。
func truncateToBudget(s string, limit int) (string, bool) {
	if limit <= 0 || s == "" {
		return s, false
	}
	if estimateTokens(s) <= limit {
		return s, false
	}
	// 逐段（按 rune）累加，找到最接近上限的位置
	var sb strings.Builder
	used := 0
	for _, r := range s {
		cost := estimateTokens(string(r))
		if used+cost > limit {
			return sb.String(), true
		}
		used += cost
		sb.WriteRune(r)
	}
	return sb.String(), false
}
