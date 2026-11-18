package work

import (
	"context"
	"errors"
	"testing"
)

func TestCancelReleasesLease(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addItem(t, b, "w1")
	claimWorker(t, b, "w1")

	if err := b.Cancel(ctx, "w1", "admin-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	item, ok := b.Get(ctx, "w1")
	if !ok || item.Status != StatusCancelled {
		t.Fatalf("item = %+v ok=%v", item, ok)
	}
	if _, ok := b.ClaimOf(ctx, "w1"); ok {
		t.Fatal("cancelled item still has an active lease")
	}
	claims, err := b.Claims(ctx, "w1")
	if err != nil {
		t.Fatalf("Claims: %v", err)
	}
	if len(claims) != 1 || claims[0].Status != ClaimReleased {
		t.Fatalf("claims = %+v", claims)
	}

	// Cancelling twice is a no-op.
	if err := b.Cancel(ctx, "w1", "admin-1"); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
}

func TestCancelRefusesDoneAndMissing(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addItem(t, b, "w1")
	finishItem(t, b, "w1")

	if err := b.Cancel(ctx, "w1", "admin-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Cancel(done) = %v, want ErrInvalidTransition", err)
	}
	if err := b.Cancel(ctx, "missing", "admin-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel(missing) = %v, want ErrNotFound", err)
	}
}

func TestCancelUnblocksWaitingClaim(t *testing.T) {
	ctx := context.Background()
	b, _ := testBoard()
	addItem(t, b, "w1")
	claimWorker(t, b, "w1")

	if err := b.Cancel(ctx, "w1", "admin-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// A cancelled item cannot be claimed again.
	if _, err := b.Claim(ctx, "w1", "worker-2", RoleWorker, "retry", 60); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Claim(cancelled) = %v, want ErrInvalidTransition", err)
	}
}
