package pool

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func newClock(ts *int64) func() int64 { return func() int64 { return *ts } }

func mustAdd(t *testing.T, p *Pool, account Account) {
	t.Helper()
	if err := p.Add(account); err != nil {
		t.Fatalf("Add(%s): %v", account.ID, err)
	}
}

func TestAddValidation(t *testing.T) {
	p := New(nil)
	if err := p.Add(Account{ProviderID: "x"}); err == nil {
		t.Fatal("accepted an account without an id")
	}
	if err := p.Add(Account{ID: "a"}); err == nil {
		t.Fatal("accepted an account without a provider")
	}
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})
	if err := p.Add(Account{ID: "a", ProviderID: "p"}); err == nil {
		t.Fatal("accepted a duplicate id")
	}
	account, ok := p.Get("a")
	if !ok || !account.Enabled {
		t.Fatalf("account = %+v ok=%v", account, ok)
	}
	if account.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown", account.Status)
	}
}

func TestSelectionOrder(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))

	mustAdd(t, p, Account{ID: "low", ProviderID: "p", Priority: 5})
	mustAdd(t, p, Account{ID: "scarce", ProviderID: "p", Priority: 1, Remaining: 10, Total: 100})
	mustAdd(t, p, Account{ID: "rich", ProviderID: "p", Priority: 1, Remaining: 90, Total: 100})

	account, err := p.Next("p", "m")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if account.ID != "rich" {
		t.Fatalf("first = %s, want rich", account.ID)
	}

	account, err = p.Pick("p", "m", "low", nil)
	if err != nil {
		t.Fatalf("Pick(pinned): %v", err)
	}
	if account.ID != "low" {
		t.Fatalf("pinned = %s, want low", account.ID)
	}
}

func TestSelectionTieBreakers(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))

	// Same priority and ratio: sooner reset wins.
	mustAdd(t, p, Account{ID: "late", ProviderID: "p", ResetsAt: 5000})
	mustAdd(t, p, Account{ID: "early", ProviderID: "p", ResetsAt: 2000})
	account, _ := p.Next("p", "m")
	if account.ID != "early" {
		t.Fatalf("reset tie-break = %s, want early", account.ID)
	}

	// Same everything: least recently used wins.
	q := New(newClock(&now))
	mustAdd(t, q, Account{ID: "recent", ProviderID: "p", LastUsedAt: 900})
	mustAdd(t, q, Account{ID: "stale", ProviderID: "p", LastUsedAt: 100})
	account, _ = q.Next("p", "m")
	if account.ID != "stale" {
		t.Fatalf("last-used tie-break = %s, want stale", account.ID)
	}
}

func TestFiltersSkipUnusableAccounts(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))

	mustAdd(t, p, Account{ID: "disabled", ProviderID: "p"})
	p.SetEnabled("disabled", false)
	mustAdd(t, p, Account{ID: "invalid", ProviderID: "p", Status: StatusInvalid})
	mustAdd(t, p, Account{ID: "cooling", ProviderID: "p", Status: StatusRateLimited, CooldownUntil: 2000})
	mustAdd(t, p, Account{ID: "empty", ProviderID: "p", Status: StatusExhausted, ResetsAt: 5000})
	mustAdd(t, p, Account{ID: "other-provider", ProviderID: "q"})
	mustAdd(t, p, Account{ID: "other-model", ProviderID: "p", Models: []string{"m2"}})
	mustAdd(t, p, Account{ID: "good", ProviderID: "p", Models: []string{"m1"}})

	account, err := p.Next("p", "m1")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if account.ID != "good" {
		t.Fatalf("selected %s, want good", account.ID)
	}
}

func TestAllowedWhitelist(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})
	mustAdd(t, p, Account{ID: "b", ProviderID: "p"})

	account, err := p.Pick("p", "m", "", []string{"b"})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if account.ID != "b" {
		t.Fatalf("selected %s, want b", account.ID)
	}

	if _, err := p.Pick("p", "m", "", []string{"c"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

func TestAllExhaustedReportsEarliestResume(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p", Status: StatusExhausted, ResetsAt: 3000})
	mustAdd(t, p, Account{ID: "b", ProviderID: "p", Status: StatusExhausted, ResetsAt: 2000})

	_, err := p.Next("p", "m")
	var exhausted *AllExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("err = %v, want AllExhaustedError", err)
	}
	if exhausted.ResumeAt != 2000 {
		t.Fatalf("resume = %d, want 2000", exhausted.ResumeAt)
	}
}

func TestQuotaFailureMarksExhausted(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p", Remaining: 100, Total: 100})

	failure := ClassifyHTTP(429, `{"error":{"code":"insufficient_quota"}}`, 0)
	if failure.Kind != FailureQuota || !failure.RetrySwitch {
		t.Fatalf("failure = %+v", failure)
	}
	p.ReportFailure("a", failure)

	account, _ := p.Get("a")
	if account.Status != StatusExhausted || account.Remaining != 0 {
		t.Fatalf("account = %+v", account)
	}
	if _, err := p.Next("p", "m"); err == nil {
		t.Fatal("exhausted account was selected")
	}
	if events := p.Events(); len(events) != 1 || events[0].Type != EventExhausted {
		t.Fatalf("events = %+v", events)
	}
}

func TestRateLimitCooldownExpires(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})

	p.ReportFailure("a", ClassifyHTTP(429, "slow down", 0))
	account, _ := p.Get("a")
	if account.Status != StatusRateLimited || account.CooldownUntil != 1060 {
		t.Fatalf("account = %+v", account)
	}
	if _, err := p.Next("p", "m"); err == nil {
		t.Fatal("cooling account selected")
	}

	now = 1061
	if _, err := p.Next("p", "m"); err != nil {
		t.Fatalf("account still cooling: %v", err)
	}
}

func TestInvalidAuthDisablesAccount(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})

	failure := ClassifyHTTP(401, "bad key", 0)
	if failure.Kind != FailureInvalid || !failure.RetrySwitch {
		t.Fatalf("failure = %+v", failure)
	}
	p.ReportFailure("a", failure)

	account, _ := p.Get("a")
	if account.Status != StatusInvalid || account.Enabled {
		t.Fatalf("account = %+v", account)
	}
	if _, err := p.Next("p", "m"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

func TestInfraFailureIsShortCooldownWithoutSwitch(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})

	failure := ClassifyHTTP(503, "upstream down", 0)
	if failure.Kind != FailureInfra || failure.RetrySwitch {
		t.Fatalf("failure = %+v", failure)
	}
	p.ReportFailure("a", failure)

	account, _ := p.Get("a")
	if account.Status != StatusUnknown || account.CooldownUntil != 1030 {
		t.Fatalf("account = %+v", account)
	}
}

func TestClientErrorDoesNotChangeAccount(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})

	failure := ClassifyHTTP(400, "bad request", 0)
	if failure.Kind != FailureClient || failure.RetrySwitch {
		t.Fatalf("failure = %+v", failure)
	}
	p.ReportFailure("a", failure)

	account, _ := p.Get("a")
	if account.Status != StatusUnknown || account.CooldownUntil != 0 {
		t.Fatalf("account = %+v", account)
	}
}

func TestExhaustedAccountRecoversAfterReset(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p", Status: StatusExhausted, ResetsAt: 2000})
	p.SetEnabled("a", true)

	now = 1999
	if _, err := p.Next("p", "m"); err == nil {
		t.Fatal("account used before its reset time")
	}

	now = 2001
	account, err := p.Next("p", "m")
	if err != nil {
		t.Fatalf("Next after reset: %v", err)
	}
	if account.Status != StatusOK || account.ResetsAt != 0 {
		t.Fatalf("account = %+v", account)
	}
	found := false
	for _, event := range p.Events() {
		if event.Type == EventReset {
			found = true
		}
	}
	if !found {
		t.Fatalf("no reset event: %+v", p.Events())
	}
}

func TestManualReset(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p", Status: StatusInvalid})
	p.SetEnabled("a", false)

	if !p.Reset("a", 500) {
		t.Fatal("Reset reported false")
	}
	account, _ := p.Get("a")
	if account.Status != StatusOK || account.Remaining != 500 || !account.Enabled {
		t.Fatalf("account = %+v", account)
	}
	if !p.Reset("missing", 0) {
		// Reset on a missing id must report false.
	} else {
		t.Fatal("Reset(missing) reported true")
	}
}

func TestReportSuccessClearsErrorState(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p", Status: StatusRateLimited, CooldownUntil: 2000, LastError: "429"})

	p.ReportSuccess("a")
	account, _ := p.Get("a")
	if account.Status != StatusOK || account.CooldownUntil != 0 || account.LastError != "" {
		t.Fatalf("account = %+v", account)
	}
	if account.LastUsedAt != now {
		t.Fatalf("last used = %d", account.LastUsedAt)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	now := int64(1000)
	p := New(newClock(&now))
	mustAdd(t, p, Account{ID: "a", ProviderID: "p"})
	p.Next("p", "m")
	p.ReportFailure("a", ClassifyHTTP(500, "boom", 0))

	events := p.Events()
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != EventSelected || events[1].Type != EventProbeFail {
		t.Fatalf("events = %+v", events)
	}

	// Mutating the returned slice must not touch the log.
	events[0].Type = "hacked"
	if p.Events()[0].Type != EventSelected {
		t.Fatal("event log was mutated through the returned slice")
	}
}

func TestRatio(t *testing.T) {
	cases := []struct {
		account Account
		want    float64
	}{
		{Account{}, 1},
		{Account{Remaining: 5}, 1},
		{Account{Remaining: 5, Total: 10}, 0.5},
		{Account{Remaining: 0, Total: 10}, 0},
		{Account{Remaining: 20, Total: 10}, 1},
	}
	for _, tc := range cases {
		if got := tc.account.Ratio(); got != tc.want {
			t.Fatalf("%+v ratio = %v, want %v", tc.account, got, tc.want)
		}
	}
}

func TestAccountDoesNotSerializeAPIKey(t *testing.T) {
	raw, err := json.Marshal(Account{ID: "a", ProviderID: "p", APIKey: "sk-secret"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if contains([]string{string(raw)}, "sk-secret") || string(raw) == "" {
		t.Fatalf("api key leaked into JSON: %s", raw)
	}
}

func TestSupports(t *testing.T) {
	if !(Account{}).Supports("anything") {
		t.Fatal("empty model list should match everything")
	}
	if !(Account{Models: []string{"*"}}).Supports("x") {
		t.Fatal("wildcard did not match")
	}
	if (Account{Models: []string{"m1"}}).Supports("m2") {
		t.Fatal("unlisted model matched")
	}
}

func TestRetryAfterHint(t *testing.T) {
	failure := ClassifyHTTP(429, "rate limited", 120*time.Second)
	if failure.Cooldown != 120 {
		t.Fatalf("cooldown = %d", failure.Cooldown)
	}
}
