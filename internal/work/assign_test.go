package work

import (
	"context"
	"errors"
	"testing"
)

func addStatusItem(t *testing.T, b *Board, id string, status Status) {
	t.Helper()
	if err := b.Add(context.Background(), WorkItem{
		ID:          id,
		WorkspaceID: "ws",
		Title:       id,
		Status:      status,
		DependsOn:   []string{"never-created"},
		Contract:    Contract{Goal: "ship", Paths: []string{"internal/work"}},
	}); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
}

func TestAssignForcesBlockedItemToWorker(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addStatusItem(t, b, "w1", StatusBlocked)

	claim, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-x", ModelID: "m1"}, RoleWorker, "fix it", 60, "admin-1")
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !claim.Forced {
		t.Fatal("claim is not marked forced")
	}
	if claim.AgentID != "agent-x" || claim.ModelID != "m1" || claim.LeaseEpoch != 1 {
		t.Fatalf("claim = %+v", claim)
	}

	item, ok := b.Get(ctx, "w1")
	if !ok || item.Status != StatusRunning {
		t.Fatalf("item = %+v ok=%v", item, ok)
	}

	// A live lease still wins: force does not create a second holder.
	if _, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-y"}, RoleWorker, "again", 60, "admin-1"); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second assign = %v, want ErrAlreadyClaimed", err)
	}
}

func TestAssignBypassesDependenciesAndAttempts(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	if err := b.Add(ctx, WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "w1",
		DependsOn:   []string{"w0"},
		Attempts:    MaxAttempts,
		Contract:    Contract{Goal: "ship", Paths: []string{"internal/work"}},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Normal claim is blocked by both the dependency and the attempt limit.
	if _, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "p", 60); !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("claim = %v, want ErrAttemptsExhausted", err)
	}

	if _, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-x"}, RoleWorker, "forced", 60, "admin-1"); err != nil {
		t.Fatalf("Assign: %v", err)
	}
}

func TestAssignVerifierAndIntegrator(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addStatusItem(t, b, "w1", StatusReview)
	addStatusItem(t, b, "w2", StatusApproved)

	if _, err := b.Assign(ctx, "w1", Claimant{AgentID: "v1"}, RoleVerifier, "review", 60, "admin-1"); err != nil {
		t.Fatalf("assign verifier: %v", err)
	}
	if _, err := b.Assign(ctx, "w2", Claimant{AgentID: "i1"}, RoleIntegrator, "merge", 60, "admin-1"); err != nil {
		t.Fatalf("assign integrator: %v", err)
	}

	item, _ := b.Get(ctx, "w2")
	if item.Status != StatusIntegrating {
		t.Fatalf("status = %s, want integrating", item.Status)
	}
}

func TestAssignRefusesTerminalItems(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addStatusItem(t, b, "w1", StatusDone)

	if _, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-x"}, RoleWorker, "again", 60, "admin-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Assign(done) = %v, want ErrInvalidTransition", err)
	}
}

func TestAssignRequiresActorAndPlan(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addStatusItem(t, b, "w1", StatusOpen)

	if _, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-x"}, RoleWorker, "p", 60, ""); err == nil {
		t.Fatal("assign without an actor was accepted")
	}
	if _, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-x"}, RoleWorker, "", 60, "admin-1"); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("Assign = %v, want ErrPlanRequired", err)
	}
	if _, err := b.Assign(ctx, "w1", Claimant{}, RoleWorker, "p", 60, "admin-1"); err == nil {
		t.Fatal("assign without a target agent was accepted")
	}
}

func TestAssignClampsLease(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addStatusItem(t, b, "w1", StatusOpen)

	claim, err := b.Assign(ctx, "w1", Claimant{AgentID: "agent-x"}, RoleWorker, "p", 1_000_000, "admin-1")
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if claim.LeaseExpiresAt-claim.HeartbeatAt != MaxLeaseSeconds {
		t.Fatalf("lease = %d", claim.LeaseExpiresAt-claim.HeartbeatAt)
	}
}
