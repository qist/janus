package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"janus/internal/goals"
	"janus/internal/work"
)

func openGoalStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "goals.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGoalRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openGoalStore(t)

	goal := goals.Goal{
		ID:           "goal_1",
		Title:        "ship it",
		Brief:        "one paragraph",
		WorkspaceID:  "ws",
		GroupID:      "g1",
		BudgetTokens: 9000,
		Risk:         work.RiskHigh,
		Status:       goals.StatusActive,
		ItemIDs:      []string{"w1", "w2"},
		CreatedBy:    "admin",
		CreatedAt:    100,
		UpdatedAt:    200,
	}
	if err := s.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	got, err := s.GetGoal(ctx, goal.ID)
	if err != nil {
		t.Fatalf("GetGoal: %v", err)
	}
	if got.Title != goal.Title || got.Risk != work.RiskHigh || got.BudgetTokens != 9000 {
		t.Fatalf("goal = %+v", got)
	}
	if len(got.ItemIDs) != 2 || got.ItemIDs[1] != "w2" {
		t.Fatalf("item ids = %v", got.ItemIDs)
	}

	// Updates overwrite in place.
	goal.Status = goals.StatusDone
	goal.ItemIDs = []string{"w1"}
	if err := s.SaveGoal(ctx, goal); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ = s.GetGoal(ctx, goal.ID)
	if got.Status != goals.StatusDone || len(got.ItemIDs) != 1 {
		t.Fatalf("goal after update = %+v", got)
	}
}

func TestGoalNotFound(t *testing.T) {
	s := openGoalStore(t)
	if _, err := s.GetGoal(context.Background(), "missing"); !errors.Is(err, goals.ErrNotFound) {
		t.Fatalf("GetGoal = %v, want goals.ErrNotFound", err)
	}
}

func TestListGoalsNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openGoalStore(t)

	for _, goal := range []goals.Goal{
		{ID: "g1", Title: "one", WorkspaceID: "ws", Status: goals.StatusActive, CreatedAt: 100},
		{ID: "g2", Title: "two", WorkspaceID: "ws", Status: goals.StatusActive, CreatedAt: 300},
		{ID: "g3", Title: "three", WorkspaceID: "ws", Status: goals.StatusActive, CreatedAt: 200},
	} {
		if err := s.SaveGoal(ctx, goal); err != nil {
			t.Fatalf("SaveGoal: %v", err)
		}
	}

	list, err := s.ListGoals(ctx, 0)
	if err != nil {
		t.Fatalf("ListGoals: %v", err)
	}
	if len(list) != 3 || list[0].ID != "g2" || list[2].ID != "g1" {
		t.Fatalf("list = %+v", list)
	}

	limited, err := s.ListGoals(ctx, 1)
	if err != nil {
		t.Fatalf("ListGoals(1): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "g2" {
		t.Fatalf("limited = %+v", limited)
	}
}

func TestGoalPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SaveGoal(ctx, goals.Goal{
		ID: "goal_1", Title: "x", WorkspaceID: "ws", Status: goals.StatusPlanning, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetGoal(ctx, "goal_1")
	if err != nil {
		t.Fatalf("GetGoal: %v", err)
	}
	if got.Title != "x" || got.Status != goals.StatusPlanning {
		t.Fatalf("goal = %+v", got)
	}
}
