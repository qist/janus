package store

import (
	"context"
	"errors"
	"testing"

	"janus/internal/capsule"
)

func TestCapsuleRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	c := capsule.New("cap-1", "w1", capsule.KindInfra, func() int64 { return 100 })
	if err := c.SetGoal("ship the work board"); err != nil {
		t.Fatalf("SetGoal: %v", err)
	}
	if err := c.AddConstraint("no network"); err != nil {
		t.Fatalf("AddConstraint: %v", err)
	}
	if err := c.AddDecision("use transactions", "atomic item+claim+event"); err != nil {
		t.Fatalf("AddDecision: %v", err)
	}
	if err := c.AddArtifact(capsule.ArtifactRef{ID: "a1", Kind: "diff", SHA256: "sha-1"}); err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	c.Rebase("abc123")

	if err := s.SaveCapsule(ctx, c); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}

	got, err := s.GetCapsule(ctx, "cap-1")
	if err != nil {
		t.Fatalf("GetCapsule: %v", err)
	}
	if got.Goal != c.Goal || got.Version != c.Version || got.BaseCommit != "abc123" {
		t.Fatalf("capsule = %+v", got)
	}
	if got.Budget() != capsule.InfraBudget {
		t.Fatalf("budget = %d, want %d", got.Budget(), capsule.InfraBudget)
	}
	if len(got.Constraints) != 1 || len(got.Decisions) != 1 || len(got.Artifacts) != 1 {
		t.Fatalf("sections lost: %+v", got)
	}
	if got.UsedTokens() == 0 {
		t.Fatal("used tokens lost")
	}

	// A loaded capsule must still be writable.
	before := got.Version
	if err := got.AddFailure(1, "tests failed", "missing index"); err != nil {
		t.Fatalf("AddFailure after load: %v", err)
	}
	if got.Version <= before {
		t.Fatalf("version = %d, want > %d", got.Version, before)
	}
	if err := s.SaveCapsule(ctx, got); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	reloaded, err := s.GetCapsule(ctx, "cap-1")
	if err != nil {
		t.Fatalf("GetCapsule: %v", err)
	}
	if len(reloaded.Failures) != 1 || reloaded.Version != got.Version {
		t.Fatalf("reloaded = %+v", reloaded)
	}
}

func TestListCapsulesNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	first := capsule.New("cap-1", "w1", capsule.KindDocs, func() int64 { return 100 })
	second := capsule.New("cap-2", "w1", capsule.KindDefault, func() int64 { return 200 })
	for i := 0; i < 3; i++ {
		if err := second.AddConstraint("later"); err != nil {
			t.Fatalf("AddConstraint: %v", err)
		}
	}
	if err := s.SaveCapsule(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCapsule(ctx, second); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListCapsules(ctx, "w1")
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(list) != 2 || list[0].ID != "cap-2" {
		t.Fatalf("list = %+v, want cap-2 first", list)
	}
	if list[0].Version <= list[1].Version {
		t.Fatalf("expected descending versions, got %d then %d", list[0].Version, list[1].Version)
	}

	other, err := s.ListCapsules(ctx, "missing")
	if err != nil {
		t.Fatalf("ListCapsules(missing): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("list for missing item = %+v", other)
	}
}

func TestGetCapsuleNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetCapsule(context.Background(), "nope"); !errors.Is(err, capsule.ErrNotFound) {
		t.Fatalf("GetCapsule = %v, want capsule.ErrNotFound", err)
	}
}

func TestCapsuleStaleFlagPersists(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	c := capsule.New("cap-1", "w1", capsule.KindDefault, func() int64 { return 100 })
	c.MarkStale()
	if err := s.SaveCapsule(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetCapsule(ctx, "cap-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StaleFlag {
		t.Fatalf("stale flag lost: %+v", got)
	}
}
