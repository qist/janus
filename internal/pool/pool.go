// Package pool manages several credentials of the same model. It exists for one
// reason: when a key runs out of quota the next key is used, without changing
// the model, so the session and its prompt cache survive.
//
// Selection is deliberately not load-balanced across healthy accounts: switching
// costs a cold cache, and that usually costs more than balancing gains.
package pool

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the quota state of one credential.
type Status string

const (
	StatusUnknown     Status = "unknown"
	StatusOK          Status = "ok"
	StatusExhausted   Status = "exhausted"
	StatusRateLimited Status = "rate_limited"
	StatusInvalid     Status = "invalid"
)

// DefaultCooldown is applied to a plain 429 without a Retry-After hint.
const DefaultCooldown int64 = 60

// Account is one credential plus its quota state. APIKey is memory only and
// must never be persisted or logged.
type Account struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	UpstreamID string `json:"upstream_id,omitempty"`
	APIKey     string `json:"-"`
	Priority   int    `json:"priority"`
	Enabled    bool   `json:"enabled"`
	Status     Status `json:"status"`
	Remaining  int64  `json:"remaining"`
	Total      int64  `json:"total"`
	Unit       string `json:"unit,omitempty"` // credit|token|usd
	// Model support; empty means "every model this provider serves".
	Models []string `json:"models,omitempty"`

	ResetsAt      int64  `json:"resets_at,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastCheckedAt int64  `json:"last_checked_at,omitempty"`
	LastUsedAt    int64  `json:"last_used_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// Ratio is remaining/total in [0,1]. Unknown quota counts as fully available:
// the pool should try rather than starve on missing data.
func (a Account) Ratio() float64 {
	if a.Total > 0 {
		ratio := float64(a.Remaining) / float64(a.Total)
		if ratio < 0 {
			return 0
		}
		if ratio > 1 {
			return 1
		}
		return ratio
	}
	if a.Remaining > 0 {
		return 1
	}
	return 1
}

// Supports reports whether the account serves model.
func (a Account) Supports(model string) bool {
	if len(a.Models) == 0 {
		return true
	}
	for _, m := range a.Models {
		if m == model || m == "*" {
			return true
		}
	}
	return false
}

// FailureKind is the classified upstream failure.
type FailureKind string

const (
	FailureQuota     FailureKind = "quota"
	FailureRateLimit FailureKind = "rate_limit"
	FailureInvalid   FailureKind = "invalid"
	FailureInfra     FailureKind = "infra"
	FailureClient    FailureKind = "client"
)

// Failure is what a failed call teaches the pool.
type Failure struct {
	Kind        FailureKind `json:"kind"`
	Cooldown    int64       `json:"cooldown,omitempty"`
	ResetsAt    int64       `json:"resets_at,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	RetrySwitch bool        `json:"retry_switch"` // another account may succeed
}

// ClassifyHTTP maps an upstream status and body to a failure. 4xx other than
// 401/403/402/429 is a client bug, not an account problem, so it does not
// trigger a switch.
func ClassifyHTTP(status int, body string, retryAfter time.Duration) Failure {
	reason := excerpt(body)
	cooldown := int64(retryAfter / time.Second)
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}

	switch status {
	case 401, 403:
		return Failure{Kind: FailureInvalid, Reason: reason, RetrySwitch: true}
	case 402:
		return Failure{Kind: FailureQuota, Reason: reason, RetrySwitch: true}
	case 429:
		lower := strings.ToLower(body)
		for _, marker := range []string{"insufficient_quota", "insufficient quota", "quota", "balance", "credit"} {
			if strings.Contains(lower, marker) {
				return Failure{Kind: FailureQuota, Reason: reason, RetrySwitch: true}
			}
		}
		return Failure{Kind: FailureRateLimit, Cooldown: cooldown, Reason: reason, RetrySwitch: true}
	}
	if status >= 500 || status == 0 {
		return Failure{Kind: FailureInfra, Cooldown: 30, Reason: reason}
	}
	return Failure{Kind: FailureClient, Reason: reason}
}

// Event is one append-only account event, used for reconciliation.
type Event struct {
	Type      string `json:"type"`
	AccountID string `json:"account_id"`
	At        int64  `json:"at"`
	Detail    string `json:"detail,omitempty"`
}

const (
	EventSelected  = "selected"
	EventExhausted = "exhausted"
	EventRateLimit = "rate_limited"
	EventInvalid   = "invalid"
	EventReset     = "reset"
	EventProbeFail = "probe_failed"
)

// ErrNoAccount means no candidate matched the request at all.
var ErrNoAccount = errors.New("no account available")

// AllExhaustedError means every candidate is out of quota; ResumeAt is the
// earliest moment something may recover.
type AllExhaustedError struct {
	ResumeAt int64
}

func (e *AllExhaustedError) Error() string {
	return fmt.Sprintf("all accounts exhausted (resume at %d)", e.ResumeAt)
}

// Pool is an in-memory account set. Persist Account rows when moving to
// multi-node; selection order is defined here.
type Pool struct {
	mu       sync.Mutex
	accounts map[string]*Account
	now      func() int64
	events   []Event
}

// New returns an empty pool.
func New(now func() int64) *Pool {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Pool{accounts: make(map[string]*Account), now: now}
}

// Add inserts an account. Enabled defaults to true so a caller cannot
// accidentally create a permanently unusable pool entry.
func (p *Pool) Add(account Account) error {
	if strings.TrimSpace(account.ID) == "" {
		return errors.New("account id required")
	}
	if strings.TrimSpace(account.ProviderID) == "" {
		return errors.New("account provider required")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.accounts[account.ID]; ok {
		return fmt.Errorf("account %q already exists", account.ID)
	}
	if account.Status == "" {
		account.Status = StatusUnknown
	}
	account.Enabled = true
	p.accounts[account.ID] = &account
	return nil
}

// Len reports the number of accounts.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// Get returns a copy of one account.
func (p *Pool) Get(id string) (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return Account{}, false
	}
	return *account, true
}

// SetEnabled turns an account on or off without deleting its state.
func (p *Pool) SetEnabled(id string, enabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	account.Enabled = enabled
	return true
}

// Next picks the best usable account for provider/model and records a selection
// event. It returns AllExhaustedError when candidates exist but none can serve
// right now, and ErrNoAccount when nothing matches at all.
func (p *Pool) Next(provider, model string) (Account, error) {
	return p.Pick(provider, model, "", nil)
}

// Pick is Next with an optional pinned account and credential whitelist.
func (p *Pool) Pick(provider, model, pinned string, allowed []string) (Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	candidates := p.candidatesLocked(provider, model, pinned, allowed, now)
	if len(candidates) > 0 {
		chosen := candidates[0]
		chosen.LastUsedAt = now
		entry := p.accounts[chosen.ID]
		entry.LastUsedAt = now
		entry.LastCheckedAt = now
		if entry.Status == StatusExhausted {
			// The reset time passed; treat it as usable again.
			entry.Status = StatusOK
			entry.ResetsAt = 0
			p.events = append(p.events, Event{Type: EventReset, AccountID: entry.ID, At: now})
		}
		if pinned != "" && chosen.ID == pinned {
			p.events = append(p.events, Event{Type: EventSelected, AccountID: entry.ID, At: now, Detail: "pinned"})
		} else {
			p.events = append(p.events, Event{Type: EventSelected, AccountID: entry.ID, At: now})
		}
		return *entry, nil
	}

	resume := p.nextResumeLocked(provider, model, allowed, now)
	if resume > 0 {
		return Account{}, &AllExhaustedError{ResumeAt: resume}
	}
	return Account{}, ErrNoAccount
}

func (p *Pool) candidatesLocked(provider, model, pinned string, allowed []string, now int64) []*Account {
	var out []*Account
	for _, account := range p.accounts {
		if !account.Enabled {
			continue
		}
		if provider != "" && account.ProviderID != provider {
			continue
		}
		if !account.Supports(model) {
			continue
		}
		if len(allowed) > 0 && !contains(allowed, account.ID) {
			continue
		}
		switch account.Status {
		case StatusInvalid:
			continue
		case StatusExhausted:
			// A zero ResetsAt means the reset time is unknown; the account
			// stays out until an operator resets it or a success is reported.
			if account.ResetsAt == 0 || account.ResetsAt > now {
				continue
			}
		}
		if account.CooldownUntil > now {
			continue
		}
		out = append(out, account)
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// 1. A pinned account wins while it is still usable.
		if pinned != "" {
			if a.ID == pinned && b.ID != pinned {
				return true
			}
			if b.ID == pinned && a.ID != pinned {
				return false
			}
		}
		// 2. Priority ascending.
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		// 3. More remaining quota first.
		if a.Ratio() != b.Ratio() {
			return a.Ratio() > b.Ratio()
		}
		// 4. Sooner reset first: it frees quota earlier.
		if a.ResetsAt != b.ResetsAt {
			if a.ResetsAt == 0 {
				return false
			}
			if b.ResetsAt == 0 {
				return true
			}
			return a.ResetsAt < b.ResetsAt
		}
		// 5. Least recently used first.
		if a.LastUsedAt != b.LastUsedAt {
			return a.LastUsedAt < b.LastUsedAt
		}
		return a.ID < b.ID
	})
	return out
}

func (p *Pool) nextResumeLocked(provider, model string, allowed []string, now int64) int64 {
	var resume int64
	for _, account := range p.accounts {
		if !account.Enabled || account.Status == StatusInvalid {
			continue
		}
		if provider != "" && account.ProviderID != provider {
			continue
		}
		if !account.Supports(model) {
			continue
		}
		if len(allowed) > 0 && !contains(allowed, account.ID) {
			continue
		}
		at := account.CooldownUntil
		if account.Status == StatusExhausted && account.ResetsAt > at {
			at = account.ResetsAt
		}
		if at <= now {
			continue
		}
		if resume == 0 || at < resume {
			resume = at
		}
	}
	return resume
}

// ReportSuccess marks an account healthy and records the time of use.
func (p *Pool) ReportSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	account, ok := p.accounts[id]
	if !ok {
		return
	}
	now := p.now()
	account.Status = StatusOK
	account.CooldownUntil = 0
	account.LastCheckedAt = now
	account.LastUsedAt = now
	account.LastError = ""
}

// ReportFailure applies a classified failure to the account.
func (p *Pool) ReportFailure(id string, failure Failure) {
	p.mu.Lock()
	defer p.mu.Unlock()

	account, ok := p.accounts[id]
	if !ok {
		return
	}
	now := p.now()
	account.LastCheckedAt = now
	account.LastError = truncate(failure.Reason, 512)
	if account.LastError == "" {
		account.LastError = string(failure.Kind)
	}

	switch failure.Kind {
	case FailureQuota:
		account.Status = StatusExhausted
		account.Remaining = 0
		account.ResetsAt = failure.ResetsAt
		p.events = append(p.events, Event{Type: EventExhausted, AccountID: id, At: now, Detail: account.LastError})
	case FailureRateLimit:
		account.Status = StatusRateLimited
		cooldown := failure.Cooldown
		if cooldown <= 0 {
			cooldown = DefaultCooldown
		}
		account.CooldownUntil = now + cooldown
		p.events = append(p.events, Event{Type: EventRateLimit, AccountID: id, At: now, Detail: account.LastError})
	case FailureInvalid:
		account.Status = StatusInvalid
		account.Enabled = false
		p.events = append(p.events, Event{Type: EventInvalid, AccountID: id, At: now, Detail: account.LastError})
	case FailureInfra:
		cooldown := failure.Cooldown
		if cooldown <= 0 {
			cooldown = 30
		}
		account.CooldownUntil = now + cooldown
		p.events = append(p.events, Event{Type: EventProbeFail, AccountID: id, At: now, Detail: account.LastError})
	}
}

// Reset manually marks an account usable again, e.g. after topping up.
func (p *Pool) Reset(id string, remaining int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	account, ok := p.accounts[id]
	if !ok {
		return false
	}
	account.Status = StatusOK
	account.Remaining = remaining
	account.ResetsAt = 0
	account.CooldownUntil = 0
	account.LastError = ""
	account.Enabled = true
	p.events = append(p.events, Event{Type: EventReset, AccountID: id, At: p.now(), Detail: "manual reset"})
	return true
}

// Events returns a copy of the append-only event log.
func (p *Pool) Events() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Event, len(p.events))
	copy(out, p.events)
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func excerpt(body string) string {
	body = strings.TrimSpace(body)
	if len(body) > 300 {
		return body[:300] + "..."
	}
	return body
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
