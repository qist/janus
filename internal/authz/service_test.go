package authz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"janus/internal/auditor"
	"janus/internal/auth"
	"janus/internal/policy"
	"janus/internal/work"
)

type fakeAuditor struct {
	name     string
	decision auditor.Decision
	err      error
	calls    int
}

func (f *fakeAuditor) Name() string { return f.name }

func (f *fakeAuditor) Review(context.Context, auditor.Input) (auditor.Decision, error) {
	f.calls++
	return f.decision, f.err
}

func allowAuditor(name string) *fakeAuditor {
	return &fakeAuditor{name: name, decision: auditor.Decision{Verdict: auditor.VerdictAllow, Confidence: 0.95}}
}

func newAuthority(t *testing.T, now *int64) *auth.Authority {
	t.Helper()
	authority, err := auth.NewAuthority([]byte("0123456789abcdef0123456789abcdef"), func() int64 { return *now })
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	return authority
}

func contract(risk work.Risk, paths ...string) work.Contract {
	return work.Contract{Goal: "ship", Risk: risk, Paths: paths}
}

func sensitiveRequest() Request {
	return Request{
		WorkItemID: "w1",
		LeaseEpoch: 7,
		AgentID:    "agent-worker",
		Action:     policy.Action{Kind: "fs.write", Targets: []string{"internal/work/x.go"}},
		Contract:   contract(work.RiskMedium, "internal/work"),
	}
}

func TestOrdinaryActionNeedsNoToken(t *testing.T) {
	now := int64(1_700_000_000)
	fake := allowAuditor("a")
	svc := New(auditor.NewGate(fake), newAuthority(t, &now))
	svc.Now = func() int64 { return now }

	out, err := svc.Authorize(context.Background(), Request{
		WorkItemID: "w1",
		Action:     policy.Action{Command: "go test ./..."},
		Contract:   contract(work.RiskMedium, "internal/work"),
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.Verdict != auditor.VerdictAllow || out.Token != "" {
		t.Fatalf("outcome = %+v", out)
	}
	if fake.calls != 0 {
		t.Fatalf("auditor called for an ordinary action (%d)", fake.calls)
	}
}

func TestSensitiveAllowIssuesSingleUseToken(t *testing.T) {
	now := int64(1_700_000_000)
	authority := newAuthority(t, &now)
	svc := New(auditor.NewGate(allowAuditor("a")), authority)
	svc.Now = func() int64 { return now }

	req := sensitiveRequest()
	out, err := svc.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.Verdict != auditor.VerdictAllow {
		t.Fatalf("verdict = %s (%s)", out.Verdict, out.Reason)
	}
	if out.Token == "" {
		t.Fatal("no token issued for an allowed sensitive action")
	}
	if out.ExpiresAt != now+DefaultTTL {
		t.Fatalf("expires_at = %d, want %d", out.ExpiresAt, now+DefaultTTL)
	}

	claims, err := authority.Verify(out.Token, CapabilityAction(req.Action))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.WorkItemID != "w1" || claims.LeaseEpoch != 7 {
		t.Fatalf("claims = %+v", claims)
	}
	if len(claims.Paths) != 1 || claims.Paths[0] != "internal/work/x.go" {
		t.Fatalf("paths = %v", claims.Paths)
	}

	// The nonce is burned on use: replaying the same token must fail.
	if _, err := authority.Verify(out.Token, CapabilityAction(req.Action)); !errors.Is(err, auth.ErrReplay) {
		t.Fatalf("replay = %v, want ErrReplay", err)
	}
}

func TestPolicyDenyIssuesNothing(t *testing.T) {
	now := int64(1_700_000_000)
	fake := allowAuditor("a")
	svc := New(auditor.NewGate(fake), newAuthority(t, &now))

	out, err := svc.Authorize(context.Background(), Request{
		WorkItemID: "w1",
		LeaseEpoch: 7,
		Action:     policy.Action{Command: "rm -rf /"},
		Contract:   contract(work.RiskMedium),
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.Verdict != auditor.VerdictDeny || out.Token != "" {
		t.Fatalf("outcome = %+v", out)
	}
	if fake.calls != 0 {
		t.Fatalf("auditor called for a hard denial (%d)", fake.calls)
	}
}

func TestSensitiveActionRequiresLease(t *testing.T) {
	now := int64(1_700_000_000)
	svc := New(auditor.NewGate(allowAuditor("a")), newAuthority(t, &now))

	req := sensitiveRequest()
	req.LeaseEpoch = 0
	out, err := svc.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.Verdict != auditor.VerdictDeny || out.Token != "" {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.Contains(out.Reason, "lease epoch") {
		t.Fatalf("reason = %q", out.Reason)
	}
}

func TestHumanRequiredIssuesNoToken(t *testing.T) {
	now := int64(1_700_000_000)
	svc := New(auditor.NewGate(allowAuditor("a"), allowAuditor("b")), newAuthority(t, &now))

	out, err := svc.Authorize(context.Background(), Request{
		WorkItemID: "w1",
		LeaseEpoch: 7,
		Action:     policy.Action{Command: "git push --force origin main"},
		Contract:   contract(work.RiskMedium),
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.Verdict != auditor.VerdictNeedsHuman || out.Token != "" {
		t.Fatalf("outcome = %+v", out)
	}
	if !out.Plan.Human {
		t.Fatalf("plan = %+v", out.Plan)
	}
}

func TestMissingAuthorityDenies(t *testing.T) {
	svc := New(auditor.NewGate(allowAuditor("a")), nil)

	out, err := svc.Authorize(context.Background(), sensitiveRequest())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.Verdict != auditor.VerdictDeny || out.Token != "" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestCachedDecisionStillMintsFreshToken(t *testing.T) {
	now := int64(1_700_000_000)
	authority := newAuthority(t, &now)
	fake := allowAuditor("a")
	gate := auditor.NewGate(fake)
	gate.Cache = auditor.NewMemoryCache(60, func() int64 { return now })

	svc := New(gate, authority)
	svc.Now = func() int64 { return now }

	first, err := svc.Authorize(context.Background(), sensitiveRequest())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Authorize(context.Background(), sensitiveRequest())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Cached {
		t.Fatal("second decision was not served from the cache")
	}
	if second.Token == "" || second.Token == first.Token {
		t.Fatal("cached decision must still mint a fresh single-use token")
	}
	if fake.calls != 1 {
		t.Fatalf("auditor calls = %d, want 1", fake.calls)
	}
}

func TestTokenTTLIsCapped(t *testing.T) {
	now := int64(1_700_000_000)
	svc := New(auditor.NewGate(allowAuditor("a")), newAuthority(t, &now))
	svc.TTL = 10_000
	svc.Now = func() int64 { return now }

	out, err := svc.Authorize(context.Background(), sensitiveRequest())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if out.ExpiresAt != now+auth.MaxTTL {
		t.Fatalf("expires_at = %d, want %d", out.ExpiresAt, now+auth.MaxTTL)
	}
}

func TestEmptyWorkItemRejected(t *testing.T) {
	svc := New(auditor.NewGate(allowAuditor("a")), nil)
	if _, err := svc.Authorize(context.Background(), Request{}); err == nil {
		t.Fatal("expected an error for a request without a work item")
	}
}
