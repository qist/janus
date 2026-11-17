package work

import (
	"context"
	"errors"
	"testing"
)

// failingEventRepo fails every event append. It lets tests prove that a
// mid-operation failure rolls the whole board operation back.
type failingEventRepo struct {
	Repository
	err error
}

func (r *failingEventRepo) AppendEvent(context.Context, Event) error {
	return r.err
}

func (r *failingEventRepo) Transact(ctx context.Context, fn func(context.Context, Repository) error) error {
	return r.Repository.Transact(ctx, func(ctx context.Context, repo Repository) error {
		return fn(ctx, &failingEventRepo{Repository: repo, err: r.err})
	})
}

func TestMemoryRepoTransactRollsBack(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepo()

	boom := errors.New("boom")
	err := repo.Transact(ctx, func(ctx context.Context, r Repository) error {
		if err := r.CreateItem(ctx, WorkItem{ID: "w1", WorkspaceID: "ws", Title: "w1"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Transact = %v, want boom", err)
	}
	if _, err := repo.GetItem(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem after rollback = %v, want ErrNotFound", err)
	}
}

func TestFailedClaimLeavesNoPartialState(t *testing.T) {
	ctx := context.Background()
	now := int64(1_700_000_000)

	mem := NewMemoryRepo()
	b := NewBoard(mem, func() int64 { return now })
	if err := b.Add(ctx, WorkItem{ID: "w1", WorkspaceID: "ws", Title: "w1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	b.repo = &failingEventRepo{Repository: mem, err: errors.New("event store down")}
	if _, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "implement", 60); err == nil {
		t.Fatal("Claim succeeded while event append failed")
	}

	item, ok := b.Get(ctx, "w1")
	if !ok {
		t.Fatal("item missing after failed claim")
	}
	if item.Status != StatusOpen {
		t.Fatalf("status = %s, want open", item.Status)
	}
	if _, ok := b.ClaimOf(ctx, "w1"); ok {
		t.Fatal("active claim survived rollback")
	}
	claims, err := mem.ListClaims(ctx, "w1")
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %+v, want none", claims)
	}

	// The rolled-back lease must not burn a fencing epoch.
	b.repo = mem
	c, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim after rollback: %v", err)
	}
	if c.LeaseEpoch != 1 {
		t.Fatalf("lease epoch = %d, want 1", c.LeaseEpoch)
	}
}

func TestFailedSubmitKeepsRunning(t *testing.T) {
	ctx := context.Background()
	now := int64(1_700_000_000)

	mem := NewMemoryRepo()
	b := NewBoard(mem, func() int64 { return now })
	if err := b.Add(ctx, WorkItem{ID: "w1", WorkspaceID: "ws", Title: "w1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	c, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	b.repo = &failingEventRepo{Repository: mem, err: errors.New("event store down")}
	if err := b.Submit(ctx, "w1", c.LeaseEpoch, "k1"); err == nil {
		t.Fatal("Submit succeeded while event append failed")
	}

	item, ok := b.Get(ctx, "w1")
	if !ok {
		t.Fatal("item missing after failed submit")
	}
	if item.Status != StatusRunning {
		t.Fatalf("status = %s, want running", item.Status)
	}
	if item.LastIdempotencyKey != "" {
		t.Fatalf("idempotency key = %q, want empty", item.LastIdempotencyKey)
	}
	active, ok := b.ClaimOf(ctx, "w1")
	if !ok || active.Status != ClaimActive {
		t.Fatalf("claim after rollback = %+v, ok=%v", active, ok)
	}

	// The same idempotency key must still be accepted once the store recovers.
	b.repo = mem
	if err := b.Submit(ctx, "w1", c.LeaseEpoch, "k1"); err != nil {
		t.Fatalf("Submit after rollback: %v", err)
	}
}
