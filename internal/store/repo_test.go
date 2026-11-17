package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"janus/internal/work"
)

func TestStoreTransactRollsBack(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	boom := errors.New("boom")
	err := s.Transact(ctx, func(ctx context.Context, repo work.Repository) error {
		if err := repo.CreateItem(ctx, sampleItem("w1")); err != nil {
			return err
		}
		if err := repo.AppendEvent(ctx, work.Event{
			WorkItemID: "w1",
			Type:       "created",
			Actor:      "human",
			CreatedAt:  100,
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Transact = %v, want boom", err)
	}

	if _, err := s.GetWorkItem(ctx, "w1"); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("item after rollback = %v, want ErrNotFound", err)
	}
	events, err := s.ListEvents(ctx, "w1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

func TestStoreTransactCommits(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.Transact(ctx, func(ctx context.Context, repo work.Repository) error {
		return repo.CreateItem(ctx, sampleItem("w1"))
	}); err != nil {
		t.Fatalf("Transact: %v", err)
	}
	if _, err := s.GetWorkItem(ctx, "w1"); err != nil {
		t.Fatalf("item after commit: %v", err)
	}
}

func TestBoardRollbackOnSQLite(t *testing.T) {
	ctx := context.Background()
	now := int64(1_700_000_000)

	s, err := Open(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	b := work.NewBoard(s, func() int64 { return now })
	if err := b.Add(ctx, work.WorkItem{ID: "w1", WorkspaceID: "ws", Title: "w1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A failing transaction must leave the item open and claim-free.
	err = s.Transact(ctx, func(ctx context.Context, repo work.Repository) error {
		if _, ok, err := repo.GetActiveClaim(ctx, "w1"); err != nil || ok {
			t.Fatalf("unexpected existing claim: ok=%v err=%v", ok, err)
		}
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("Transact did not fail")
	}

	item, err := s.GetWorkItem(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if item.Status != work.StatusOpen || item.Version != 1 {
		t.Fatalf("item = %+v, want open at version 1", item)
	}
}
