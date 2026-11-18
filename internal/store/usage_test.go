package store

import (
	"context"
	"path/filepath"
	"testing"

	"janus/internal/usage"
)

func openUsageStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestUsageLedgerPersistsAndAggregates(t *testing.T) {
	st := openUsageStore(t)
	ctx := context.Background()

	entries := []usage.Entry{
		{KeyID: "k1", ProviderID: "p1", Model: "m1", PromptTokens: 10, CompletionTokens: 5, Status: 200, DurationMS: 100, CreatedAt: 1000},
		{KeyID: "k1", ProviderID: "p1", Model: "m2", PromptTokens: 20, CompletionTokens: 10, Status: 200, DurationMS: 300, CreatedAt: 3700},
		{KeyID: "k2", ProviderID: "p2", Model: "m1", PromptTokens: 0, CompletionTokens: 0, Status: 502, ErrorCode: "upstream_error", CreatedAt: 4000},
	}
	for _, entry := range entries {
		if err := st.Append(ctx, entry); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	totals, err := st.Totals(ctx, usage.Filter{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.Entries != 3 || totals.Failed != 1 {
		t.Fatalf("totals = %+v", totals)
	}
	if totals.Tokens() != 45 || totals.DurationMS != 400 {
		t.Fatalf("totals = %+v", totals)
	}

	stats, err := st.Stats(ctx, usage.StatsQuery{Filter: usage.Filter{}, Bucket: usage.BucketHour})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats.ByModel) != 2 || len(stats.ByProvider) != 2 || len(stats.ByKey) != 2 {
		t.Fatalf("breakdown sizes: models=%d providers=%d keys=%d",
			len(stats.ByModel), len(stats.ByProvider), len(stats.ByKey))
	}
	if len(stats.Trend) != 2 {
		t.Fatalf("trend points = %d, want 2: %+v", len(stats.Trend), stats.Trend)
	}
	if stats.Trend[0].Start != 0 || stats.Trend[1].Start != 3600 {
		t.Fatalf("unexpected trend buckets: %+v", stats.Trend)
	}
	if stats.Trend[1].Entries != 2 {
		t.Fatalf("second bucket = %+v", stats.Trend[1])
	}

	recent, err := st.Recent(ctx, usage.Filter{ProviderID: "p1"}, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Model != "m2" {
		t.Fatalf("recent = %+v", recent)
	}
}

func TestUsageTotalsRespectWindow(t *testing.T) {
	st := openUsageStore(t)
	ctx := context.Background()
	if err := st.Append(ctx, usage.Entry{Model: "m", PromptTokens: 10, Status: 200, CreatedAt: 500}); err != nil {
		t.Fatal(err)
	}
	totals, err := st.Totals(ctx, usage.Filter{Since: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Entries != 0 {
		t.Fatalf("entries outside the window counted: %+v", totals)
	}
}
