package store

import (
	"context"
	"path/filepath"
	"testing"

	"janus/internal/work"
)

func TestBoardWithSQLiteRepository(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "board.db")
	now := int64(1_700_000_000)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b := work.NewBoard(s, func() int64 { return now })

	if err := b.Add(ctx, work.WorkItem{ID: "w1", WorkspaceID: "ws", Title: "w1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	c, err := b.Claim(ctx, "w1", "worker-1", work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.Submit(ctx, "w1", c.LeaseEpoch, "submit-1"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	b2 := work.NewBoard(s2, func() int64 { return now })

	item, ok := b2.Get(ctx, "w1")
	if !ok {
		t.Fatal("item missing after reopen")
	}
	if item.Status != work.StatusReview || item.LastIdempotencyKey != "submit-1" {
		t.Fatalf("item after reopen = %+v", item)
	}

	v, err := b2.Claim(ctx, "w1", "verifier-1", work.RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	if err := b2.Verdict(ctx, "w1", v.LeaseEpoch, work.VerdictApprove, "ok"); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	i, err := b2.Claim(ctx, "w1", "integrator-1", work.RoleIntegrator, "merge", 60)
	if err != nil {
		t.Fatalf("integrator claim: %v", err)
	}
	if err := b2.Complete(ctx, "w1", i.LeaseEpoch, "done-1"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, ok = b2.Get(ctx, "w1")
	if !ok || item.Status != work.StatusDone {
		t.Fatalf("item after complete = %+v, ok=%v", item, ok)
	}
}

func TestActiveClaimSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "claim.db")
	now := int64(1_700_000_000)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b := work.NewBoard(s, func() int64 { return now })
	if err := b.Add(ctx, work.WorkItem{ID: "w1", WorkspaceID: "ws", Title: "w1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	c, err := b.Claim(ctx, "w1", "worker-1", work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	b2 := work.NewBoard(s2, func() int64 { return now })

	active, ok := b2.ClaimOf(ctx, "w1")
	if !ok {
		t.Fatal("active claim missing after reopen")
	}
	if active.LeaseEpoch != c.LeaseEpoch || active.AgentID != "worker-1" {
		t.Fatalf("active = %+v, want epoch %d agent worker-1", active, c.LeaseEpoch)
	}
	if err := b2.Heartbeat(ctx, "w1", active.LeaseEpoch, 60); err != nil {
		t.Fatalf("heartbeat after reopen: %v", err)
	}
}
