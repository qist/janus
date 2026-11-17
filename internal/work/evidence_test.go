package work

import (
	"context"
	"errors"
	"testing"
)

func evidenceItem(t *testing.T, b *Board, id string) {
	t.Helper()
	item := WorkItem{
		ID:          id,
		WorkspaceID: "ws",
		Title:       id,
		Contract: Contract{
			Goal: "ship",
			Checks: []Check{
				{Name: "unit", Command: "go test ./...", TimeoutSeconds: 60, ExpectExit: 0},
			},
		},
	}
	if err := b.Add(context.Background(), item); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestSubmitRequiresEvidence(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addItem(t, b, "w1")

	c := claimWorker(t, b, "w1")
	err := b.Submit(ctx, "w1", c.LeaseEpoch, "k1", Evidence{})
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("Submit = %v, want ErrEvidenceMissing", err)
	}

	item, _ := b.Get(ctx, "w1")
	if item.Status != StatusRunning {
		t.Fatalf("status = %s, want running", item.Status)
	}
}

func TestSubmitRequiresContractCheckResults(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	evidenceItem(t, b, "w1")

	c := claimWorker(t, b, "w1")
	err := b.Submit(ctx, "w1", c.LeaseEpoch, "k1", Evidence{
		Artifacts: []string{"artifact:diff"},
		DiffHash:  "sha256:diff",
	})
	if !errors.Is(err, ErrCheckNotRun) {
		t.Fatalf("Submit = %v, want ErrCheckNotRun", err)
	}

	err = b.Submit(ctx, "w1", c.LeaseEpoch, "k2", Evidence{
		Artifacts: []string{"artifact:diff"},
		DiffHash:  "sha256:diff",
		Checks: []CheckResult{
			{Name: "unit", Command: "go test ./...", ExitCode: 1},
		},
	})
	if !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("Submit = %v, want ErrCheckFailed", err)
	}

	err = b.Submit(ctx, "w1", c.LeaseEpoch, "k3", Evidence{
		Artifacts: []string{"artifact:diff"},
		DiffHash:  "sha256:diff",
		Checks: []CheckResult{
			{Name: "unit", Command: "go test ./...", ExitCode: 0},
		},
	})
	if err != nil {
		t.Fatalf("Submit with full evidence: %v", err)
	}

	item, _ := b.Get(ctx, "w1")
	if item.Status != StatusReview || item.SubmittedBy != "worker-1" {
		t.Fatalf("item = %+v, want review submitted by worker-1", item)
	}
}

func TestExpectedNonZeroExitPasses(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	if err := b.Add(ctx, WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "w1",
		Contract: Contract{
			Goal:   "negative test",
			Checks: []Check{{Name: "reject", Command: "exit 1", TimeoutSeconds: 10, ExpectExit: 1}},
		},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	c := claimWorker(t, b, "w1")
	if err := b.Submit(ctx, "w1", c.LeaseEpoch, "k1", Evidence{
		Artifacts: []string{"artifact:diff"},
		Checks:    []CheckResult{{Name: "reject", Command: "exit 1", ExitCode: 1}},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func TestSelfReviewRejected(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addItem(t, b, "w1")

	w := claimWorker(t, b, "w1")
	if err := b.Submit(ctx, "w1", w.LeaseEpoch, "k1", submitEvidence()); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The same agent tries to verify its own work under the verifier role.
	v, err := b.Claim(ctx, "w1", "worker-1", RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	err = b.Verdict(ctx, "w1", v.LeaseEpoch, VerdictApprove, "looks fine")
	if !errors.Is(err, ErrSelfReview) {
		t.Fatalf("Verdict = %v, want ErrSelfReview", err)
	}

	item, _ := b.Get(ctx, "w1")
	if item.Status != StatusReview {
		t.Fatalf("status = %s, want review", item.Status)
	}
}

func TestSelfReviewIgnoresEmptySubmitter(t *testing.T) {
	item := WorkItem{}
	if item.SelfReview("agent-1") {
		t.Fatal("empty submitter should not be treated as self review")
	}
	item.SubmittedBy = "agent-1"
	if !item.SelfReview("agent-1") {
		t.Fatal("same agent should be self review")
	}
	if item.SelfReview("agent-2") {
		t.Fatal("different agent should not be self review")
	}
}
