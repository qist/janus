package work

import (
	"context"
	"errors"
	"testing"
)

type fakeRunner struct {
	got     []Check
	results []CheckResult
	err     error
}

func (f *fakeRunner) Run(_ context.Context, checks []Check) ([]CheckResult, error) {
	f.got = checks
	return f.results, f.err
}

func reviewSetup(t *testing.T) (*Board, *MemoryRepo, string) {
	t.Helper()
	ctx := context.Background()
	repo := NewMemoryRepo()
	b := NewBoard(repo, func() int64 { return 1_700_000_000 })

	if err := b.Add(ctx, WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "w1",
		Contract: Contract{
			Goal:   "ship",
			Checks: []Check{{Name: "unit", Command: "go test ./...", TimeoutSeconds: 60}},
		},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	w, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if err := b.Submit(ctx, "w1", w.LeaseEpoch, "submit-1", Evidence{
		Artifacts: []string{"artifact:diff"},
		DiffHash:  "sha256:diff",
		Checks:    []CheckResult{{Name: "unit", Command: "go test ./...", ExitCode: 0}},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return b, repo, "w1"
}

func TestApproveRequiresVerifierChecks(t *testing.T) {
	ctx := context.Background()
	b, repo, id := reviewSetup(t)

	v, err := b.Claim(ctx, id, "verifier-1", RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}

	if err := b.Verdict(ctx, id, v.LeaseEpoch, VerdictApprove, "looks fine"); !errors.Is(err, ErrVerifyRequired) {
		t.Fatalf("Verdict without checks = %v, want ErrVerifyRequired", err)
	}
	if err := b.VerdictWithChecks(ctx, id, v.LeaseEpoch, VerdictApprove, "looks fine", []CheckResult{
		{Name: "unit", Command: "go test ./...", ExitCode: 1},
	}); !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("Verdict with failing check = %v, want ErrCheckFailed", err)
	}

	if err := b.VerdictWithChecks(ctx, id, v.LeaseEpoch, VerdictApprove, "unit passed", []CheckResult{
		{Name: "unit", Command: "go test ./...", ExitCode: 0},
	}); err != nil {
		t.Fatalf("VerdictWithChecks: %v", err)
	}

	item, _ := b.Get(ctx, id)
	if item.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", item.Status)
	}

	reviews, err := repo.ListReviews(ctx, id)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("reviews = %+v, want 1", reviews)
	}
	review := reviews[0]
	if review.Verdict != VerdictApprove || review.ReviewerID != "verifier-1" {
		t.Fatalf("review = %+v", review)
	}
	if len(review.Checks) != 1 || review.Checks[0].ExitCode != 0 {
		t.Fatalf("review checks = %+v", review.Checks)
	}
}

func TestVerifyRunsContractChecks(t *testing.T) {
	ctx := context.Background()
	b, _, id := reviewSetup(t)

	v, err := b.Claim(ctx, id, "verifier-1", RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}

	runner := &fakeRunner{results: []CheckResult{{Name: "unit", ExitCode: 0}}}
	results, err := b.Verify(ctx, id, v.LeaseEpoch, runner)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(runner.got) != 1 || runner.got[0].Name != "unit" {
		t.Fatalf("runner got %+v, want the contract checks", runner.got)
	}
	if len(results) != 1 || results[0].ExitCode != 0 {
		t.Fatalf("results = %+v", results)
	}

	// The worker role cannot run verification checks.
	if _, err := b.Verify(ctx, id, v.LeaseEpoch+1, runner); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale epoch = %v, want ErrStaleEpoch", err)
	}
	if _, err := b.Verify(ctx, id, v.LeaseEpoch, nil); err == nil {
		t.Fatal("Verify accepted a nil runner")
	}
}

func TestVerifyRejectsSelfReview(t *testing.T) {
	ctx := context.Background()
	b, _, id := reviewSetup(t)

	v, err := b.Claim(ctx, id, "worker-1", RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	if _, err := b.Verify(ctx, id, v.LeaseEpoch, &fakeRunner{}); !errors.Is(err, ErrSelfReview) {
		t.Fatalf("Verify = %v, want ErrSelfReview", err)
	}
}

func TestRejectRecordsReviewWithChecks(t *testing.T) {
	ctx := context.Background()
	b, repo, id := reviewSetup(t)

	v, err := b.Claim(ctx, id, "verifier-1", RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	checks := []CheckResult{{Name: "unit", ExitCode: 1, OutputTail: "FAIL"}}
	if err := b.VerdictWithChecks(ctx, id, v.LeaseEpoch, VerdictReject, "unit fails", checks); err != nil {
		t.Fatalf("VerdictWithChecks: %v", err)
	}

	item, _ := b.Get(ctx, id)
	if item.Status != StatusOpen || item.Attempts != 1 {
		t.Fatalf("item = %+v, want open with 1 attempt", item)
	}

	reviews, _ := repo.ListReviews(ctx, id)
	if len(reviews) != 1 || reviews[0].Verdict != VerdictReject || len(reviews[0].Checks) != 1 {
		t.Fatalf("reviews = %+v", reviews)
	}
}

func TestReviewRecordedForItemsWithoutChecks(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepo()
	b := NewBoard(repo, func() int64 { return 1_700_000_000 })
	addItem(t, b, "w1")

	finishItem(t, b, "w1")

	reviews, err := repo.ListReviews(ctx, "w1")
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Verdict != VerdictApprove || reviews[0].ReviewerID != "verifier-1" {
		t.Fatalf("reviews = %+v", reviews)
	}
}
