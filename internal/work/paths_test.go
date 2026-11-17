package work

import (
	"context"
	"errors"
	"testing"
)

func TestPathsOverlap(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"equal", []string{"internal/work"}, []string{"internal/work"}, true},
		{"child", []string{"internal/work"}, []string{"internal/work/board.go"}, true},
		{"parent", []string{"internal/work/board.go"}, []string{"internal/work"}, true},
		{"sibling", []string{"internal/work"}, []string{"internal/worker"}, false},
		{"disjoint", []string{"internal/api"}, []string{"web/"}, false},
		{"one side empty", []string{"internal/work"}, nil, false},
		{"dot prefix", []string{"./internal/work/"}, []string{"internal/work/x.go"}, true},
		{"empty string", []string{""}, []string{"internal/work"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathsOverlap(tc.a, tc.b); got != tc.want {
				t.Fatalf("PathsOverlap(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func addPathItem(t *testing.T, b *Board, id, workspace string, paths ...string) {
	t.Helper()
	if err := b.Add(context.Background(), WorkItem{
		ID:          id,
		WorkspaceID: workspace,
		Title:       id,
		Contract:    Contract{Goal: "edit", Paths: paths},
	}); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
}

func TestPathLockBlocksOverlappingClaim(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()

	addPathItem(t, b, "w1", "ws", "internal/work")
	addPathItem(t, b, "w2", "ws", "internal/work/board.go")

	if _, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim w1: %v", err)
	}
	if _, err := b.Claim(ctx, "w2", "worker-2", RoleWorker, "edit", 60); !errors.Is(err, ErrPathLocked) {
		t.Fatalf("claim w2 = %v, want ErrPathLocked", err)
	}

	open, err := b.Claimable(ctx, RoleWorker)
	if err != nil {
		t.Fatalf("Claimable: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("claimable = %+v, want none (w2 is path-locked)", open)
	}
}

func TestPathLockReleasedOnRelease(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()

	addPathItem(t, b, "w1", "ws", "internal/work")
	addPathItem(t, b, "w2", "ws", "internal/work")

	c, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "edit", 60)
	if err != nil {
		t.Fatalf("claim w1: %v", err)
	}
	if err := b.Release(ctx, "w1", c.LeaseEpoch); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := b.Claim(ctx, "w2", "worker-2", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

func TestPathLockScopedToWorkspace(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()

	addPathItem(t, b, "w1", "ws-a", "internal/work")
	addPathItem(t, b, "w2", "ws-b", "internal/work")

	if _, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim w1: %v", err)
	}
	if _, err := b.Claim(ctx, "w2", "worker-2", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim in another workspace: %v", err)
	}
}

func TestPathLockAllowsSiblings(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()

	addPathItem(t, b, "w1", "ws", "internal/work")
	addPathItem(t, b, "w2", "ws", "internal/worker")

	if _, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim w1: %v", err)
	}
	if _, err := b.Claim(ctx, "w2", "worker-2", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim sibling: %v", err)
	}
}

func TestPathLockIgnoresVerifier(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()

	addPathItem(t, b, "w1", "ws", "internal/work")
	addPathItem(t, b, "w2", "ws", "internal/work")

	// Move w2 into review first, then start w1 on the same scope.
	w, err := b.Claim(ctx, "w2", "worker-2", RoleWorker, "edit", 60)
	if err != nil {
		t.Fatalf("claim w2: %v", err)
	}
	if err := b.Submit(ctx, "w2", w.LeaseEpoch, "k1", submitEvidence()); err != nil {
		t.Fatalf("submit w2: %v", err)
	}
	if _, err := b.Claim(ctx, "w1", "worker-1", RoleWorker, "edit", 60); err != nil {
		t.Fatalf("claim w1: %v", err)
	}

	// Review only reads, so the overlap must not block the verifier.
	if _, err := b.Claim(ctx, "w2", "verifier-1", RoleVerifier, "review", 60); err != nil {
		t.Fatalf("verifier claim = %v, want success", err)
	}
}
