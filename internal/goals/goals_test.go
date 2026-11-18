package goals

import (
	"context"
	"errors"
	"testing"
	"time"

	"janus/internal/work"
)

func newTestService(t *testing.T) (*Service, *work.Board) {
	t.Helper()
	now := int64(1_700_000_000)
	board := work.NewBoard(work.NewMemoryRepo(), func() int64 { return now })
	svc := New(NewMemoryRepo(), board)
	svc.Now = func() int64 { return now }
	return svc, board
}

type fakePlanner struct {
	items []work.WorkItem
	err   error
}

func (f fakePlanner) Plan(context.Context, Goal) ([]work.WorkItem, error) {
	return f.items, f.err
}

func TestCreatePlansOneItem(t *testing.T) {
	ctx := context.Background()
	svc, board := newTestService(t)

	goal, err := svc.Create(ctx, CreateRequest{
		Title:       "fix claim race",
		Brief:       "two agents claim at once",
		WorkspaceID: "ws",
		CreatedBy:   "admin",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if goal.Status != StatusActive {
		t.Fatalf("status = %s (%s)", goal.Status, goal.Error)
	}
	if len(goal.ItemIDs) != 1 {
		t.Fatalf("items = %v", goal.ItemIDs)
	}

	item, ok := board.Get(ctx, goal.ItemIDs[0])
	if !ok {
		t.Fatal("item missing from the board")
	}
	if item.Title != "fix claim race" || item.Contract.Goal != "two agents claim at once" {
		t.Fatalf("item = %+v", item)
	}
	if item.CreatedBy != "admin" {
		t.Fatalf("created by = %q", item.CreatedBy)
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	if _, err := svc.Create(ctx, CreateRequest{WorkspaceID: "ws"}); err == nil {
		t.Fatal("accepted a goal without a title")
	}
	if _, err := svc.Create(ctx, CreateRequest{Title: "x"}); err == nil {
		t.Fatal("accepted a goal without a workspace")
	}
}

func TestPlannerFailureMarksGoalFailed(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	svc.Planner = fakePlanner{err: errors.New("planner exploded")}

	goal, err := svc.Create(ctx, CreateRequest{Title: "x", WorkspaceID: "ws"})
	if err == nil {
		t.Fatal("Create did not report the planner error")
	}
	if goal.Status != StatusFailed || goal.Error == "" {
		t.Fatalf("goal = %+v", goal)
	}

	stored, err := svc.Get(ctx, goal.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != StatusFailed {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestPlannerWithNoItemsFails(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	svc.Planner = fakePlanner{}

	goal, err := svc.Create(ctx, CreateRequest{Title: "x", WorkspaceID: "ws"})
	if err == nil {
		t.Fatal("Create accepted an empty plan")
	}
	if goal.Status != StatusFailed {
		t.Fatalf("status = %s", goal.Status)
	}
}

func TestSubmitPlansInBackground(t *testing.T) {
	ctx := context.Background()
	svc, board := newTestService(t)

	goal, err := svc.Submit(ctx, CreateRequest{Title: "background", WorkspaceID: "ws"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if goal.Status != StatusPlanning {
		t.Fatalf("status = %s, want planning", goal.Status)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := svc.Get(ctx, goal.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if current.Status == StatusActive {
			if len(current.ItemIDs) != 1 {
				t.Fatalf("items = %v", current.ItemIDs)
			}
			if _, ok := board.Get(ctx, current.ItemIDs[0]); !ok {
				t.Fatal("planned item missing")
			}
			return
		}
		if current.Status == StatusFailed {
			t.Fatalf("goal failed: %s", current.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("goal never became active")
}

func TestProgressTracksItemsAndCompletesGoal(t *testing.T) {
	ctx := context.Background()
	svc, board := newTestService(t)
	goal, err := svc.Create(ctx, CreateRequest{Title: "ship", WorkspaceID: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	itemID := goal.ItemIDs[0]

	progress, err := svc.Progress(ctx, goal.ID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if progress.Total != 1 || progress.Done != 0 || progress.Percent != 0 {
		t.Fatalf("progress = %+v", progress)
	}

	worker, err := board.Claim(ctx, itemID, "worker-1", work.RoleWorker, "implement", 60)
	if err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if err := board.Submit(ctx, itemID, worker.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	verifier, err := board.Claim(ctx, itemID, "verifier-1", work.RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	if err := board.Verdict(ctx, itemID, verifier.LeaseEpoch, work.VerdictApprove, "ok"); err != nil {
		t.Fatalf("Verdict: %v", err)
	}
	integrator, err := board.Claim(ctx, itemID, "integrator-1", work.RoleIntegrator, "merge", 60)
	if err != nil {
		t.Fatalf("integrator claim: %v", err)
	}
	if err := board.Complete(ctx, itemID, integrator.LeaseEpoch, "done-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	progress, err = svc.Progress(ctx, goal.ID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if progress.Status != StatusDone || progress.Done != 1 || progress.Percent != 100 {
		t.Fatalf("progress = %+v", progress)
	}

	stored, _ := svc.Get(ctx, goal.ID)
	if stored.Status != StatusDone {
		t.Fatalf("stored status = %s", stored.Status)
	}
}

func TestProgressMarksBlockedGoal(t *testing.T) {
	ctx := context.Background()
	svc, board := newTestService(t)
	goal, err := svc.Create(ctx, CreateRequest{Title: "blocked", WorkspaceID: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	itemID := goal.ItemIDs[0]

	worker, _ := board.Claim(ctx, itemID, "worker-1", work.RoleWorker, "implement", 60)
	if err := board.Submit(ctx, itemID, worker.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	verifier, _ := board.Claim(ctx, itemID, "verifier-1", work.RoleVerifier, "review", 60)
	if err := board.Verdict(ctx, itemID, verifier.LeaseEpoch, work.VerdictNeedsHuman, "unclear DoD"); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	progress, err := svc.Progress(ctx, goal.ID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if progress.Status != StatusBlocked || progress.Blocked != 1 {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestCancelGoalIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	goal, _ := svc.Create(ctx, CreateRequest{Title: "x", WorkspaceID: "ws"})

	cancelled, err := svc.Cancel(ctx, goal.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("status = %s", cancelled.Status)
	}

	again, err := svc.Cancel(ctx, goal.ID)
	if err != nil || again.Status != StatusCancelled {
		t.Fatalf("second cancel = %+v, %v", again, err)
	}

	progress, err := svc.Progress(ctx, goal.ID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if progress.Status != StatusCancelled {
		t.Fatalf("progress = %+v", progress)
	}

	if _, err := svc.Cancel(ctx, "goal_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(missing) = %v", err)
	}
}

func TestListGoalsNewestFirst(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	now := int64(1_700_000_000)
	svc.Now = func() int64 { return now }

	first, _ := svc.Create(ctx, CreateRequest{Title: "first", WorkspaceID: "ws"})
	now += 10
	second, _ := svc.Create(ctx, CreateRequest{Title: "second", WorkspaceID: "ws"})

	list, err := svc.List(ctx, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list = %+v", list)
	}

	limited, _ := svc.List(ctx, 1)
	if len(limited) != 1 || limited[0].ID != second.ID {
		t.Fatalf("limited = %+v", limited)
	}
}

func TestSinglePlannerRiskAndBudget(t *testing.T) {
	planner := SinglePlanner{}
	items, err := planner.Plan(context.Background(), Goal{
		ID: "g1", Title: "x", Brief: "b", WorkspaceID: "ws", Risk: work.RiskHigh, BudgetTokens: 5000,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if items[0].Contract.Risk != work.RiskHigh || items[0].Contract.BudgetTokens != 5000 {
		t.Fatalf("item = %+v", items[0])
	}

	// Default risk must be explicit, not empty.
	items, err = planner.Plan(context.Background(), Goal{ID: "g2", Title: "y"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if items[0].Contract.Risk != work.RiskMedium {
		t.Fatalf("risk = %q, want medium", items[0].Contract.Risk)
	}
}
