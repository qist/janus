package usage

import (
	"context"
	"testing"
)

func TestAppendAssignsIDAndTime(t *testing.T) {
	ledger := NewMemoryLedger(func() int64 { return 1000 })
	if err := ledger.Append(context.Background(), Entry{Model: "m", PromptTokens: 10, CompletionTokens: 5}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries := ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].ID == "" || entries[0].CreatedAt != 1000 {
		t.Fatalf("entry = %+v", entries[0])
	}
	if entries[0].Tokens() != 15 {
		t.Fatalf("tokens = %d", entries[0].Tokens())
	}
}

func TestAppendRequiresModel(t *testing.T) {
	ledger := NewMemoryLedger(nil)
	if err := ledger.Append(context.Background(), Entry{}); err == nil {
		t.Fatal("accepted an entry without a model")
	}
}

func TestTotalsAndFilters(t *testing.T) {
	ledger := NewMemoryLedger(func() int64 { return 1000 })
	ctx := context.Background()

	entries := []Entry{
		{Model: "m", KeyID: "k1", GroupID: "g1", WorkItemID: "w1", ProviderID: "p1", CredentialID: "a", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.01, CreatedAt: 100},
		{Model: "m", KeyID: "k1", GroupID: "g1", WorkItemID: "w2", ProviderID: "p1", CredentialID: "b", PromptTokens: 20, CompletionTokens: 10, CostUSD: 0.02, CreatedAt: 200},
		{Model: "m2", KeyID: "k2", GroupID: "g2", WorkItemID: "w1", ProviderID: "p2", CredentialID: "c", PromptTokens: 5, CompletionTokens: 5, CostUSD: 0.03, CreatedAt: 300},
	}
	for _, entry := range entries {
		if err := ledger.Append(ctx, entry); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	all, err := ledger.Totals(ctx, Filter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if all.Entries != 3 || all.Tokens() != 55 || all.CostUSD != 0.06 {
		t.Fatalf("all = %+v", all)
	}

	byKey, _ := ledger.Totals(ctx, Filter{KeyID: "k1"})
	if byKey.Entries != 2 || byKey.Tokens() != 45 {
		t.Fatalf("byKey = %+v", byKey)
	}

	byGroup, _ := ledger.Totals(ctx, Filter{GroupID: "g2"})
	if byGroup.Entries != 1 || byGroup.CostUSD != 0.03 {
		t.Fatalf("byGroup = %+v", byGroup)
	}

	byItem, _ := ledger.Totals(ctx, Filter{WorkItemID: "w1"})
	if byItem.Entries != 2 || byItem.Tokens() != 25 {
		t.Fatalf("byItem = %+v", byItem)
	}

	byCredential, _ := ledger.Totals(ctx, Filter{CredentialID: "b"})
	if byCredential.Entries != 1 || byCredential.PromptTokens != 20 {
		t.Fatalf("byCredential = %+v", byCredential)
	}

	window, _ := ledger.Totals(ctx, Filter{Since: 150, Until: 250})
	if window.Entries != 1 || window.CostUSD != 0.02 {
		t.Fatalf("window = %+v", window)
	}

	none, _ := ledger.Totals(ctx, Filter{KeyID: "missing"})
	if none.Entries != 0 || none.Tokens() != 0 {
		t.Fatalf("none = %+v", none)
	}
}

func TestEntriesAreCopied(t *testing.T) {
	ledger := NewMemoryLedger(nil)
	if err := ledger.Append(context.Background(), Entry{Model: "m"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries := ledger.Entries()
	entries[0].Model = "hacked"
	if ledger.Entries()[0].Model != "m" {
		t.Fatal("ledger entry mutated through the returned slice")
	}
}

func TestEmptyLedgerTotals(t *testing.T) {
	ledger := NewMemoryLedger(nil)
	totals, err := ledger.Totals(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if totals != (Totals{}) {
		t.Fatalf("totals = %+v", totals)
	}
	if ledger.Len() != 0 {
		t.Fatalf("len = %d", ledger.Len())
	}
}
