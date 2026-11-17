package work

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func testBoard() (*Board, *int64) {
	now := int64(1_700_000_000)
	return NewBoard(func() int64 { return now }), &now
}

func addItem(t *testing.T, b *Board, id string) {
	t.Helper()
	if err := b.Add(WorkItem{ID: id, WorkspaceID: "ws", Title: id}); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
}

func claimWorker(t *testing.T, b *Board, id string) Claim {
	t.Helper()
	c, err := b.Claim(id, "worker-1", RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("claim worker %s: %v", id, err)
	}
	return c
}

func claimVerifier(t *testing.T, b *Board, id string) Claim {
	t.Helper()
	c, err := b.Claim(id, "verifier-1", RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("claim verifier %s: %v", id, err)
	}
	return c
}

func claimIntegrator(t *testing.T, b *Board, id string) Claim {
	t.Helper()
	c, err := b.Claim(id, "integrator-1", RoleIntegrator, "merge", 60)
	if err != nil {
		t.Fatalf("claim integrator %s: %v", id, err)
	}
	return c
}

func finishItem(t *testing.T, b *Board, id string) {
	t.Helper()
	w := claimWorker(t, b, id)
	if err := b.Submit(id, w.LeaseEpoch, "submit-"+id); err != nil {
		t.Fatalf("submit %s: %v", id, err)
	}
	v := claimVerifier(t, b, id)
	if err := b.Verdict(id, v.LeaseEpoch, VerdictApprove, "ok"); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
	i := claimIntegrator(t, b, id)
	if err := b.Complete(id, i.LeaseEpoch, "done-"+id); err != nil {
		t.Fatalf("complete %s: %v", id, err)
	}
}

func TestClaimRequiresPlan(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	if _, err := b.Claim("w1", "a1", RoleWorker, "", 60); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("got %v, want ErrPlanRequired", err)
	}
}

func TestPlanTooLong(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	plan := strings.Repeat("x", MaxPlanBytes+1)
	if _, err := b.Claim("w1", "a1", RoleWorker, plan, 60); !errors.Is(err, ErrPlanTooLong) {
		t.Fatalf("got %v, want ErrPlanTooLong", err)
	}
}

func TestClaimFencingAndSubmit(t *testing.T) {
	b, now := testBoard()
	addItem(t, b, "w1")

	c := claimWorker(t, b, "w1")
	item, _ := b.Get("w1")
	if item.Status != StatusRunning {
		t.Fatalf("status = %s, want running", item.Status)
	}

	if _, err := b.Claim("w1", "worker-2", RoleWorker, "again", 60); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second claim = %v, want ErrAlreadyClaimed", err)
	}
	if err := b.Heartbeat("w1", c.LeaseEpoch+1, 60); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale heartbeat = %v, want ErrStaleEpoch", err)
	}

	*now += 10
	if err := b.Heartbeat("w1", c.LeaseEpoch, 60); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := b.Submit("w1", c.LeaseEpoch, ""); !errors.Is(err, ErrIdempotencyRequired) {
		t.Fatalf("empty idempotency = %v, want ErrIdempotencyRequired", err)
	}
	if err := b.Submit("w1", c.LeaseEpoch, "k1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := b.Submit("w1", c.LeaseEpoch, "k1"); err != nil {
		t.Fatalf("idempotent submit: %v", err)
	}
	if err := b.Submit("w1", c.LeaseEpoch, "k2"); !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("different key = %v, want ErrAlreadySubmitted", err)
	}

	item, _ = b.Get("w1")
	if item.Status != StatusReview {
		t.Fatalf("status = %s, want review", item.Status)
	}
}

func TestExpiredLeaseRequeues(t *testing.T) {
	b, now := testBoard()
	addItem(t, b, "w1")

	c := claimWorker(t, b, "w1")
	*now += 61

	if !b.Expire("w1") {
		t.Fatal("Expire = false, want true")
	}
	item, _ := b.Get("w1")
	if item.Status != StatusOpen {
		t.Fatalf("status = %s, want open", item.Status)
	}
	if item.LeaseExpiries != 1 {
		t.Fatalf("lease expiries = %d, want 1", item.LeaseExpiries)
	}
	if _, ok := b.ClaimOf("w1"); ok {
		t.Fatal("active claim still present")
	}

	c2, err := b.Claim("w1", "worker-2", RoleWorker, "retry", 60)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if c2.LeaseEpoch != c.LeaseEpoch+1 {
		t.Fatalf("epoch = %d, want %d", c2.LeaseEpoch, c.LeaseEpoch+1)
	}
}

func TestSubmitRejectsExpiredLease(t *testing.T) {
	b, now := testBoard()
	addItem(t, b, "w1")

	c := claimWorker(t, b, "w1")
	*now += 61

	if err := b.Submit("w1", c.LeaseEpoch, "k1"); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("submit = %v, want ErrLeaseExpired", err)
	}
	item, _ := b.Get("w1")
	if item.Status != StatusOpen {
		t.Fatalf("status = %s, want open", item.Status)
	}
}

func TestRejectRequeues(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	w := claimWorker(t, b, "w1")
	if err := b.Submit("w1", w.LeaseEpoch, "k1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	v := claimVerifier(t, b, "w1")
	if err := b.Verdict("w1", v.LeaseEpoch, VerdictReject, "tests failed"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	item, _ := b.Get("w1")
	if item.Status != StatusOpen {
		t.Fatalf("status = %s, want open", item.Status)
	}
	if item.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", item.Attempts)
	}

	w2 := claimWorker(t, b, "w1")
	if w2.LeaseEpoch <= v.LeaseEpoch {
		t.Fatalf("epoch = %d, want > %d", w2.LeaseEpoch, v.LeaseEpoch)
	}
}

func TestThreeRejectsBlock(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	for i := 0; i < MaxAttempts; i++ {
		w := claimWorker(t, b, "w1")
		if err := b.Submit("w1", w.LeaseEpoch, fmt.Sprintf("k%d", i)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		v := claimVerifier(t, b, "w1")
		if err := b.Verdict("w1", v.LeaseEpoch, VerdictReject, "no"); err != nil {
			t.Fatalf("reject %d: %v", i, err)
		}
	}

	item, _ := b.Get("w1")
	if item.Status != StatusBlocked {
		t.Fatalf("status = %s, want blocked", item.Status)
	}
	if item.Attempts != MaxAttempts {
		t.Fatalf("attempts = %d, want %d", item.Attempts, MaxAttempts)
	}
	if _, err := b.Claim("w1", "worker-x", RoleWorker, "retry", 60); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("claim = %v, want ErrAttemptsExhausted", err)
	}
}

func TestApproveAndComplete(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	w := claimWorker(t, b, "w1")
	if err := b.Submit("w1", w.LeaseEpoch, "k1"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	v := claimVerifier(t, b, "w1")
	if err := b.Verdict("w1", v.LeaseEpoch, VerdictApprove, "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	item, _ := b.Get("w1")
	if item.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", item.Status)
	}

	i := claimIntegrator(t, b, "w1")
	if err := b.Complete("w1", i.LeaseEpoch, "done-1"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := b.Complete("w1", i.LeaseEpoch, "done-1"); err != nil {
		t.Fatalf("idempotent complete: %v", err)
	}

	item, _ = b.Get("w1")
	if item.Status != StatusDone {
		t.Fatalf("status = %s, want done", item.Status)
	}
}

func TestDependencyBlocksClaim(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")
	if err := b.Add(WorkItem{ID: "w2", WorkspaceID: "ws", Title: "w2", DependsOn: []string{"w1"}}); err != nil {
		t.Fatalf("Add(w2): %v", err)
	}

	if _, err := b.Claim("w2", "worker-1", RoleWorker, "start", 60); !errors.Is(err, ErrDependencyNotDone) {
		t.Fatalf("claim before dependency = %v, want ErrDependencyNotDone", err)
	}

	finishItem(t, b, "w1")

	if _, err := b.Claim("w2", "worker-1", RoleWorker, "start", 60); err != nil {
		t.Fatalf("claim after dependency: %v", err)
	}
}

func TestReleaseRequeues(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	c := claimWorker(t, b, "w1")
	if err := b.Release("w1", c.LeaseEpoch); err != nil {
		t.Fatalf("release: %v", err)
	}

	item, _ := b.Get("w1")
	if item.Status != StatusOpen {
		t.Fatalf("status = %s, want open", item.Status)
	}
	c2 := claimWorker(t, b, "w1")
	if c2.LeaseEpoch != c.LeaseEpoch+1 {
		t.Fatalf("epoch = %d, want %d", c2.LeaseEpoch, c.LeaseEpoch+1)
	}
}

func TestRoleStateGuards(t *testing.T) {
	b, _ := testBoard()
	addItem(t, b, "w1")

	if _, err := b.Claim("w1", "v", RoleVerifier, "review", 60); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("verifier on open = %v, want ErrInvalidTransition", err)
	}
	if _, err := b.Claim("w1", "i", RoleIntegrator, "merge", 60); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("integrator on open = %v, want ErrInvalidTransition", err)
	}
}

func TestClaimableOrder(t *testing.T) {
	b, _ := testBoard()
	if err := b.Add(WorkItem{ID: "low", WorkspaceID: "ws", Title: "low", Priority: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(WorkItem{ID: "high", WorkspaceID: "ws", Title: "high", Priority: 10}); err != nil {
		t.Fatal(err)
	}

	got := b.Claimable(RoleWorker)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "high" {
		t.Fatalf("first = %s, want high", got[0].ID)
	}
}
