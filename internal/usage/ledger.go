// Package usage is the local ledger for model spend. Provider usage APIs are
// authoritative when available, but a local append-only record is what makes
// reconciliation, per-key budgets and incident diagnosis possible.
package usage

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Entry is one completed model call.
type Entry struct {
	ID string `json:"id"`
	// RequestID ties the entry to the request that caused it.
	RequestID string `json:"request_id,omitempty"`
	// KeyID is the API key / tenant that pays for the call.
	KeyID string `json:"key_id,omitempty"`
	// GroupID groups several keys under one team budget.
	GroupID string `json:"group_id,omitempty"`
	// WorkItemID links spend to a work item when there is one.
	WorkItemID string `json:"work_item_id,omitempty"`

	ProviderID   string `json:"provider_id"`
	CredentialID string `json:"credential_id,omitempty"`
	Model        string `json:"model"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// CachedTokens are prompt tokens served from a provider cache.
	CachedTokens int `json:"cached_tokens,omitempty"`
	// CostUSD is filled by the caller when a price table is known.
	CostUSD float64 `json:"cost_usd,omitempty"`

	CreatedAt int64 `json:"created_at"`
}

// Tokens is the billable token count.
func (e Entry) Tokens() int { return e.PromptTokens + e.CompletionTokens }

// Filter selects entries. Empty fields match everything; Since/Until are
// inclusive Unix seconds and zero means unbounded.
type Filter struct {
	KeyID        string
	GroupID      string
	WorkItemID   string
	CredentialID string
	ProviderID   string
	Model        string
	Since        int64
	Until        int64
}

// Match reports whether e belongs in the filter.
func (f Filter) Match(e Entry) bool {
	switch {
	case f.KeyID != "" && e.KeyID != f.KeyID:
		return false
	case f.GroupID != "" && e.GroupID != f.GroupID:
		return false
	case f.WorkItemID != "" && e.WorkItemID != f.WorkItemID:
		return false
	case f.CredentialID != "" && e.CredentialID != f.CredentialID:
		return false
	case f.ProviderID != "" && e.ProviderID != f.ProviderID:
		return false
	case f.Model != "" && e.Model != f.Model:
		return false
	case f.Since > 0 && e.CreatedAt < f.Since:
		return false
	case f.Until > 0 && e.CreatedAt > f.Until:
		return false
	}
	return true
}

// Totals is the aggregate view used by budgets and dashboards.
type Totals struct {
	Entries          int     `json:"entries"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Tokens is the billable token total.
func (t Totals) Tokens() int { return t.PromptTokens + t.CompletionTokens }

// Ledger persists usage entries. Append-only: entries are never updated.
type Ledger interface {
	Append(ctx context.Context, entry Entry) error
	Totals(ctx context.Context, filter Filter) (Totals, error)
}

// MemoryLedger is the in-process ledger used by tests and single-node dev.
type MemoryLedger struct {
	mu      sync.Mutex
	entries []Entry
	nextID  uint64
	now     func() int64
}

// NewMemoryLedger returns an empty ledger. now may be nil.
func NewMemoryLedger(now func() int64) *MemoryLedger {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &MemoryLedger{now: now}
}

// Append implements Ledger.
func (l *MemoryLedger) Append(_ context.Context, entry Entry) error {
	if entry.Model == "" {
		return errors.New("usage entry requires a model")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.nextID++
	if entry.ID == "" {
		entry.ID = "usg_" + itoa(l.nextID)
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = l.now()
	}
	l.entries = append(l.entries, entry)
	return nil
}

// Totals implements Ledger.
func (l *MemoryLedger) Totals(_ context.Context, filter Filter) (Totals, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out Totals
	for _, entry := range l.entries {
		if !filter.Match(entry) {
			continue
		}
		out.Entries++
		out.PromptTokens += entry.PromptTokens
		out.CompletionTokens += entry.CompletionTokens
		out.CachedTokens += entry.CachedTokens
		out.CostUSD += entry.CostUSD
	}
	return out, nil
}

// Entries returns a copy of the recorded entries, oldest first.
func (l *MemoryLedger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Len reports how many entries were recorded.
func (l *MemoryLedger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
