package worksvc

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"janus/internal/capsule"
	"janus/internal/contextpack"
	"janus/internal/work"
)

type memCapsules struct {
	mu    sync.Mutex
	items map[string]*capsule.Capsule
}

func newMemCapsules() *memCapsules {
	return &memCapsules{items: make(map[string]*capsule.Capsule)}
}

func cloneCapsule(t *testing.T, c *capsule.Capsule) *capsule.Capsule {
	t.Helper()
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := capsule.Unmarshal(data, nil)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return out
}

func (m *memCapsules) SaveCapsule(_ context.Context, c *capsule.Capsule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[c.ID] = c
	return nil
}

func (m *memCapsules) GetCapsule(_ context.Context, id string) (*capsule.Capsule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.items[id]
	if !ok {
		return nil, capsule.ErrNotFound
	}
	return c, nil
}

func (m *memCapsules) ListCapsules(_ context.Context, workItemID string) ([]*capsule.Capsule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*capsule.Capsule
	for _, c := range m.items {
		if c.WorkItemID == workItemID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version > out[j].Version
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func setup(t *testing.T) (*Service, *work.Board, *memCapsules, *int64) {
	t.Helper()
	ctx := context.Background()
	now := int64(1_700_000_000)

	board := work.NewBoard(work.NewMemoryRepo(), func() int64 { return now })
	repo := newMemCapsules()
	svc := &Service{
		Board:    board,
		Capsules: repo,
		Facts: func(context.Context, string) (contextpack.WorkspaceFacts, error) {
			return contextpack.WorkspaceFacts{Build: "go build ./...", Test: "go test ./..."}, nil
		},
		Now: func() int64 { return now },
	}

	if err := board.Add(ctx, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "fix claim race",
		Brief:       "two agents claim at once",
		Contract: work.Contract{
			Goal:      "one claim wins",
			Paths:     []string{"internal/work"},
			CapsuleID: "cap-1",
		},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return svc, board, repo, &now
}

func TestClaimBuildsPack(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setup(t)

	result, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if result.Claim.LeaseEpoch != 1 || result.Capsule == nil {
		t.Fatalf("result = %+v", result)
	}
	if result.Capsule.Goal != "one claim wins" {
		t.Fatalf("capsule goal = %q", result.Capsule.Goal)
	}
	for _, want := range []string{
		"work w1: fix claim race",
		"goal: one claim wins",
		"paths: internal/work",
		"capsule cap-1",
		"goal: one claim wins",
		"build: go build ./...",
	} {
		if !contains(result.Pack.Text, want) {
			t.Fatalf("pack missing %q:\n%s", want, result.Pack.Text)
		}
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	if result.Pack.Tokens > result.Pack.Budget {
		t.Fatalf("tokens %d > budget %d", result.Pack.Tokens, result.Pack.Budget)
	}
}

func TestGetReturnsClaimCapsuleAndReviews(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setup(t)

	if _, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	view, err := svc.Get(ctx, "w1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Claim == nil || view.Claim.AgentID != "worker-1" {
		t.Fatalf("claim = %+v", view.Claim)
	}
	if view.Capsule == nil || view.Capsule.WorkItemID != "w1" {
		t.Fatalf("capsule = %+v", view.Capsule)
	}
	if len(view.Reviews) != 0 {
		t.Fatalf("reviews = %+v", view.Reviews)
	}
}

func TestSubmitRecordsEvidenceInCapsule(t *testing.T) {
	ctx := context.Background()
	svc, _, repo, _ := setup(t)

	c, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := svc.Submit(ctx, "w1", "worker-1", c.Claim.LeaseEpoch, "k1", work.Evidence{
		Artifacts: []string{"artifact:1"},
		DiffHash:  "sha-diff",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	caps, err := repo.GetCapsule(ctx, "cap-1")
	if err != nil {
		t.Fatalf("GetCapsule: %v", err)
	}
	if len(caps.Artifacts) != 2 {
		t.Fatalf("artifacts = %+v, want 2", caps.Artifacts)
	}
	if caps.Artifacts[1].Kind != "diff" || caps.Artifacts[1].SHA256 != "sha-diff" {
		t.Fatalf("diff ref = %+v", caps.Artifacts[1])
	}
}

func TestSubmitFailureDoesNotTouchCapsule(t *testing.T) {
	ctx := context.Background()
	svc, _, repo, _ := setup(t)

	err := svc.Submit(ctx, "w1", "worker-1", 99, "k1", work.Evidence{Artifacts: []string{"a1"}})
	if !errors.Is(err, work.ErrLeaseExpired) {
		t.Fatalf("Submit = %v, want ErrLeaseExpired", err)
	}
	if _, err := repo.GetCapsule(ctx, "cap-1"); !errors.Is(err, capsule.ErrNotFound) {
		t.Fatalf("capsule written after failed submit: %v", err)
	}
}

func TestVerdictRecordsFailureForNextClaimer(t *testing.T) {
	ctx := context.Background()
	svc, _, repo, _ := setup(t)

	w, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if err := svc.Submit(ctx, "w1", "worker-1", w.Claim.LeaseEpoch, "k1", work.Evidence{
		Artifacts: []string{"artifact:1"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	v, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "verifier-1"}, work.RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	if err := svc.Verdict(ctx, "w1", "verifier-1", v.Claim.LeaseEpoch, work.VerdictReject, "unit failed", []work.CheckResult{
		{Name: "unit", ExitCode: 1, OutputTail: "FAIL: claim race"},
	}, ""); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	caps, err := repo.GetCapsule(ctx, "cap-1")
	if err != nil {
		t.Fatalf("GetCapsule: %v", err)
	}
	if len(caps.Failures) != 1 {
		t.Fatalf("failures = %+v, want 1", caps.Failures)
	}
	failure := caps.Failures[0]
	if failure.Attempt != 1 || failure.Summary != "unit failed" {
		t.Fatalf("failure = %+v", failure)
	}
	if !contains(failure.Reason, "unit exit=1") {
		t.Fatalf("reason = %q", failure.Reason)
	}

	pack, warnings, err := svc.Handoff(ctx, "w1")
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if !contains(pack.Text, "failures:") || !contains(pack.Text, "unit failed") {
		t.Fatalf("pack missing failure:\n%s", pack.Text)
	}
}

func TestApproveDoesNotRecordFailure(t *testing.T) {
	ctx := context.Background()
	svc, _, repo, _ := setup(t)

	w, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if err := svc.Submit(ctx, "w1", "worker-1", w.Claim.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	v, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "verifier-1"}, work.RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	if err := svc.Verdict(ctx, "w1", "verifier-1", v.Claim.LeaseEpoch, work.VerdictApprove, "ok", nil, ""); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	caps, err := repo.GetCapsule(ctx, "cap-1")
	if err != nil {
		t.Fatalf("GetCapsule: %v", err)
	}
	if len(caps.Failures) != 0 {
		t.Fatalf("failures = %+v, want none", caps.Failures)
	}
}

func TestFactsErrorBecomesWarning(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setup(t)
	svc.Facts = func(context.Context, string) (contextpack.WorkspaceFacts, error) {
		return contextpack.WorkspaceFacts{}, errors.New("facts backend down")
	}

	result, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(result.Warnings) != 1 || !contains(result.Warnings[0], "facts") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	if result.Pack.Text == "" {
		t.Fatal("pack should still be built without facts")
	}
}

func TestClaimWithoutCapsuleStore(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := setup(t)
	svc.Capsules = nil

	result, err := svc.Claim(ctx, "w1", work.Claimant{AgentID: "worker-1"}, work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if result.Capsule != nil {
		t.Fatalf("capsule = %+v, want nil", result.Capsule)
	}
	if !contains(result.Pack.Text, "work w1") {
		t.Fatalf("pack = %q", result.Pack.Text)
	}
}

func TestKindFor(t *testing.T) {
	cases := []struct {
		paths []string
		want  capsule.Kind
	}{
		{[]string{"docs/REFACTOR.md"}, capsule.KindDocs},
		{[]string{"tests/e2e"}, capsule.KindTests},
		{[]string{"deploy/Dockerfile"}, capsule.KindInfra},
		{[]string{".github/workflows/ci.yml"}, capsule.KindInfra},
		{[]string{"internal/auth/capability.go"}, capsule.KindSecurity},
		{[]string{"internal/work/board.go"}, capsule.KindDefault},
		{nil, capsule.KindDefault},
	}
	for _, tc := range cases {
		item := work.WorkItem{Contract: work.Contract{Paths: tc.paths}}
		if got := KindFor(item); got != tc.want {
			t.Fatalf("KindFor(%v) = %q, want %q", tc.paths, got, tc.want)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
