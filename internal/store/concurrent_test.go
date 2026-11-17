package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"janus/internal/work"
)

// Two boards on the same database simulate two control-plane instances. Only
// one claim may win; the loser must not leave a claim or a half-updated item.
func TestConcurrentClaimSingleWinner(t *testing.T) {
	ctx := context.Background()
	now := int64(1_700_000_000)

	s, err := Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.CreateWorkItem(ctx, work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "w1",
		Status:      work.StatusOpen,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	const contenders = 8
	boards := make([]*work.Board, contenders)
	for i := range boards {
		boards[i] = work.NewBoard(s, func() int64 { return now })
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		results []error
	)
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := boards[i].Claim(ctx, "w1", "agent-"+string(rune('a'+i)), work.RoleWorker, "implement", 60)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				results = append(results, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("wins = %d, want 1 (errors: %v)", wins, results)
	}
	for _, err := range results {
		if !errors.Is(err, work.ErrAlreadyClaimed) && !errors.Is(err, work.ErrVersionConflict) &&
			!errors.Is(err, work.ErrInvalidTransition) {
			t.Fatalf("unexpected loser error: %v", err)
		}
	}

	item, err := s.GetWorkItem(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if item.Status != work.StatusRunning || item.Version != 2 {
		t.Fatalf("item = %+v, want running at version 2", item)
	}

	claims, err := s.ListClaims(ctx, "w1")
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	if len(claims) != 1 || claims[0].Status != work.ClaimActive {
		t.Fatalf("claims = %+v, want exactly one active claim", claims)
	}

	events, err := s.ListEvents(ctx, "w1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != "claimed" {
		t.Fatalf("events = %+v, want exactly one claimed event", events)
	}
}
