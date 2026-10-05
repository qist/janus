package main

import "testing"

func TestRequestLogHitRate(t *testing.T) {
	raw := requestLogRaw{InputTokens: 100, CacheReadTokens: 900, OutputTokens: 5, StartedAt: 1700000000000}
	raw.RequestedModel = "mimo-v2.6-flash"
	r := raw.toRequestLog()
	if r.PromptTokens != 1000 || r.InputTokens != 100 || r.CacheRead != 900 {
		t.Fatalf("tokens: %+v", r)
	}
	if r.CacheHitRate < 0.899 || r.CacheHitRate > 0.901 {
		t.Fatalf("hit rate = %v, want ~0.9", r.CacheHitRate)
	}
	if r.Model != "mimo-v2.6-flash" {
		t.Fatalf("model = %q", r.Model)
	}
}

func TestRequestLogNoCache(t *testing.T) {
	raw := requestLogRaw{InputTokens: 500}
	r := raw.toRequestLog()
	if r.PromptTokens != 500 || r.CacheHitRate != 0 {
		t.Fatalf("%+v", r)
	}
}
