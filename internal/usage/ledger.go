// Package usage is the local ledger for model spend. Provider usage APIs are
// authoritative when available, but a local append-only record is what makes
// reconciliation, per-key budgets and incident diagnosis possible.
package usage

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Entry is one completed model call. Failed calls are recorded too (Status is
// then zero or the upstream status) so error rates and latency are visible
// without a second table.
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
	// Model is the name the caller asked for (the exposed route).
	Model string `json:"model"`
	// UpstreamModel is what the provider reported or was sent.
	UpstreamModel string `json:"upstream_model,omitempty"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// CachedTokens are prompt tokens served from a provider cache.
	CachedTokens int `json:"cached_tokens,omitempty"`
	// CostUSD is filled by the caller when a price table is known.
	CostUSD float64 `json:"cost_usd,omitempty"`

	// Status is the HTTP status returned to the caller; 0 means the call never
	// produced a response (transport error, timeout).
	Status int `json:"status,omitempty"`
	// ErrorCode is the stable janus code, e.g. upstream_error.
	ErrorCode string `json:"error_code,omitempty"`
	Stream    bool   `json:"stream,omitempty"`
	// DurationMS is the wall time of the upstream call.
	DurationMS int64 `json:"duration_ms,omitempty"`

	CreatedAt int64 `json:"created_at"`
}

// Tokens is the billable token count.
func (e Entry) Tokens() int { return e.PromptTokens + e.CompletionTokens }

// Failed reports whether the call ended in an error.
func (e Entry) Failed() bool { return e.ErrorCode != "" || (e.Status != 0 && e.Status >= 400) }

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
	Failed           int     `json:"failed"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// DurationMS is the summed upstream time; average = DurationMS/Entries.
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// Tokens is the billable token total.
func (t Totals) Tokens() int { return t.PromptTokens + t.CompletionTokens }

// Group is one row of a breakdown (by model, provider or key).
type Group struct {
	Name             string  `json:"name"`
	Entries          int     `json:"entries"`
	Failed           int     `json:"failed"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	DurationMS       int64   `json:"duration_ms,omitempty"`
}

// Bucket is one point of the trend series. Start is the window start in Unix
// seconds.
type Bucket struct {
	Start            int64   `json:"start"`
	Entries          int     `json:"entries"`
	Failed           int     `json:"failed"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Bucket sizes accepted by Stats.
const (
	BucketHour = "hour"
	BucketDay  = "day"
)

// StatsQuery selects the aggregate view.
type StatsQuery struct {
	Filter Filter
	// Bucket is "hour" or "day"; empty picks hour for ranges up to 3 days.
	Bucket string
	// RangeStart/RangeEnd, when set, force the trend window so empty buckets
	// can be rendered. They are Unix seconds.
	RangeStart int64
	RangeEnd   int64
	// TopN caps each breakdown; zero uses 10, negative means unlimited.
	TopN int
}

// Stats is the dashboard payload: one totals line plus the dimensions the
// console actually renders. Aggregation happens in the ledger so no caller has
// to walk the whole table.
type Stats struct {
	Totals     Totals   `json:"totals"`
	ByModel    []Group  `json:"by_model"`
	ByProvider []Group  `json:"by_provider"`
	ByKey      []Group  `json:"by_key"`
	Trend      []Bucket `json:"trend"`
	Bucket     string   `json:"bucket"`
}

// Ledger persists usage entries. Append-only: entries are never updated.
type Ledger interface {
	Append(ctx context.Context, entry Entry) error
	Totals(ctx context.Context, filter Filter) (Totals, error)
	Stats(ctx context.Context, query StatsQuery) (Stats, error)
	// Recent returns the newest entries first.
	Recent(ctx context.Context, filter Filter, limit int) ([]Entry, error)
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
		if filter.Match(entry) {
			addTotals(&out, entry)
		}
	}
	return out, nil
}

// Stats implements Ledger.
func (l *MemoryLedger) Stats(_ context.Context, query StatsQuery) (Stats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket := NormalizeBucket(query.Bucket, query.Filter)
	out := Stats{
		ByModel:    []Group{},
		ByProvider: []Group{},
		ByKey:      []Group{},
		Trend:      []Bucket{},
		Bucket:     bucket,
	}
	byModel := map[string]*Group{}
	byProvider := map[string]*Group{}
	byKey := map[string]*Group{}
	trend := map[int64]*Bucket{}
	size := BucketSeconds(bucket)

	for _, entry := range l.entries {
		if !query.Filter.Match(entry) {
			continue
		}
		addTotals(&out.Totals, entry)
		addGroup(byModel, entry.Model, entry)
		addGroup(byProvider, entry.ProviderID, entry)
		addGroup(byKey, entry.KeyID, entry)

		start := entry.CreatedAt / size * size
		point := trend[start]
		if point == nil {
			point = &Bucket{Start: start}
			trend[start] = point
		}
		addBucket(point, entry)
	}

	out.ByModel = topGroups(byModel, query.TopN)
	out.ByProvider = topGroups(byProvider, query.TopN)
	out.ByKey = topGroups(byKey, query.TopN)
	for _, point := range trend {
		out.Trend = append(out.Trend, *point)
	}
	sort.Slice(out.Trend, func(i, j int) bool { return out.Trend[i].Start < out.Trend[j].Start })
	return out, nil
}

// Recent implements Ledger.
func (l *MemoryLedger) Recent(_ context.Context, filter Filter, limit int) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	limit = normalizeLimit(limit)
	out := make([]Entry, 0, limit)
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		if filter.Match(l.entries[i]) {
			out = append(out, l.entries[i])
		}
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

func addTotals(out *Totals, entry Entry) {
	out.Entries++
	if entry.Failed() {
		out.Failed++
	}
	out.PromptTokens += entry.PromptTokens
	out.CompletionTokens += entry.CompletionTokens
	out.CachedTokens += entry.CachedTokens
	out.CostUSD += entry.CostUSD
	out.DurationMS += entry.DurationMS
}

func addGroup(groups map[string]*Group, name string, entry Entry) {
	if name == "" {
		name = "unknown"
	}
	group := groups[name]
	if group == nil {
		group = &Group{Name: name}
		groups[name] = group
	}
	group.Entries++
	if entry.Failed() {
		group.Failed++
	}
	group.PromptTokens += entry.PromptTokens
	group.CompletionTokens += entry.CompletionTokens
	group.CachedTokens += entry.CachedTokens
	group.CostUSD += entry.CostUSD
	group.DurationMS += entry.DurationMS
}

func addBucket(point *Bucket, entry Entry) {
	point.Entries++
	if entry.Failed() {
		point.Failed++
	}
	point.PromptTokens += entry.PromptTokens
	point.CompletionTokens += entry.CompletionTokens
	point.CachedTokens += entry.CachedTokens
	point.CostUSD += entry.CostUSD
}

func topGroups(groups map[string]*Group, limit int) []Group {
	out := make([]Group, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Entries != out[j].Entries {
			return out[i].Entries > out[j].Entries
		}
		return out[i].Name < out[j].Name
	})
	limit = normalizeTopN(limit)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

func normalizeTopN(limit int) int {
	if limit == 0 {
		return 10
	}
	if limit < 0 {
		return 0
	}
	return limit
}

// NormalizeBucket picks a valid bucket, defaulting by range length.
func NormalizeBucket(bucket string, filter Filter) string {
	switch bucket {
	case BucketHour, BucketDay:
		return bucket
	case "":
		// A three-day window is the point where hourly points stop being
		// readable; before that hours are the useful resolution.
		if filter.Since > 0 && filter.Until > 0 && filter.Until-filter.Since > 3*86400 {
			return BucketDay
		}
		return BucketHour
	default:
		return BucketHour
	}
}

// BucketSeconds is the width of one trend point.
func BucketSeconds(bucket string) int64 {
	if bucket == BucketDay {
		return 86400
	}
	return 3600
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
