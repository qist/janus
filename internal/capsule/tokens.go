package capsule

// EstimateTokens is a deterministic, model-agnostic size estimate used to
// enforce capsule budgets without calling a tokenizer. Wide runes (CJK and
// similar) count as one token each; other bytes count four to a token. It
// deliberately rounds up so a budget pass never under-counts a prompt.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}

	wide, narrow := 0, 0
	for _, r := range s {
		if r > 0x2e80 {
			wide++
			continue
		}
		narrow++
	}
	return wide + (narrow+3)/4
}
