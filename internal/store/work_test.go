package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"janus/internal/work"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleItem(id string) work.WorkItem {
	return work.WorkItem{
		ID:          id,
		WorkspaceID: "ws",
		Title:       "sample " + id,
		Brief:       "brief",
		Contract: work.Contract{
			Goal:       "ship it",
			Acceptance: []string{"works"},
			Checks: []work.Check{
				{Name: "unit", Command: "go test ./...", TimeoutSeconds: 60, ExpectExit: 0},
			},
			Risk:          work.RiskLow,
			Paths:         []string{"internal/"},
			BudgetTokens:  1000,
			Autonomy:      work.AutonomyL1,
			CapsuleID:     "cap-1",
			PrefixVersion: "prefix-1",
			Version:       1,
		},
		Capabilities:  []string{"go"},
		ExecutionSite: "server",
		RequiredModes: []string{"native"},
		DependsOn:     []string{},
		Priority:      5,
		Status:        work.StatusOpen,
		Replicas:      1,
		CreatedBy:     "human",
		Version:       1,
		CreatedAt:     100,
		UpdatedAt:     100,
	}
}

func TestWorkItemRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	item := sampleItem("w1")
	if err := s.CreateWorkItem(ctx, item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	got, err := s.GetWorkItem(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if !reflect.DeepEqual(got, item) {
		t.Fatalf("round trip mismatch:\n got=%+v\nwant=%+v", got, item)
	}
}

func TestWorkItemOptimisticLock(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	item := sampleItem("w1")
	if err := s.CreateWorkItem(ctx, item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	got, err := s.GetWorkItem(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	got.Attempts = 1
	if err := s.UpdateWorkItem(ctx, got, 1); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}

	updated, err := s.GetWorkItem(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkItem after update: %v", err)
	}
	if updated.Version != 2 || updated.Attempts != 1 {
		t.Fatalf("updated = %+v, want version 2 attempts 1", updated)
	}

	if err := s.UpdateWorkItem(ctx, got, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update = %v, want ErrVersionConflict", err)
	}
}

func TestWorkItemNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetWorkItem(context.Background(), "missing"); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("got %v, want work.ErrNotFound", err)
	}
}

func TestListWorkItemsOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	low := sampleItem("low")
	low.Priority = 1
	high := sampleItem("high")
	high.Priority = 10

	if err := s.CreateWorkItem(ctx, low); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkItem(ctx, high); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListWorkItems(ctx, work.StatusOpen, "ws")
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(got) != 2 || got[0].ID != "high" {
		t.Fatalf("got %+v, want high first", got)
	}
}

func TestClaimActiveLookup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	claim := work.Claim{
		ID:             "cl_w1_1",
		WorkItemID:     "w1",
		AgentID:        "worker-1",
		Role:           work.RoleWorker,
		LeaseEpoch:     1,
		LeaseExpiresAt: 1000,
		HeartbeatAt:    900,
		Plan:           "implement",
		Status:         work.ClaimActive,
		CreatedAt:      900,
	}
	if err := s.SaveClaim(ctx, claim); err != nil {
		t.Fatalf("SaveClaim: %v", err)
	}

	active, ok, err := s.GetActiveClaim(ctx, "w1")
	if err != nil {
		t.Fatalf("GetActiveClaim: %v", err)
	}
	if !ok || !reflect.DeepEqual(active, claim) {
		t.Fatalf("active = %+v, ok=%v", active, ok)
	}

	claim.Status = work.ClaimSubmitted
	if err := s.SaveClaim(ctx, claim); err != nil {
		t.Fatalf("SaveClaim submitted: %v", err)
	}
	if _, ok, err := s.GetActiveClaim(ctx, "w1"); err != nil || ok {
		t.Fatalf("active after submit: ok=%v err=%v", ok, err)
	}

	history, err := s.ListClaims(ctx, "w1")
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	if len(history) != 1 || history[0].Status != work.ClaimSubmitted {
		t.Fatalf("history = %+v", history)
	}
}

func TestArtifactsReviewsEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	artifact := work.Artifact{
		ID:         "a1",
		WorkItemID: "w1",
		Kind:       "diff",
		URI:        "artifacts/a1.diff",
		SHA256:     "abc123",
		Bytes:      42,
		Summary:    "change",
		CreatedBy:  "worker-1",
		CreatedAt:  100,
	}
	if err := s.CreateArtifact(ctx, artifact); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	artifacts, err := s.ListArtifacts(ctx, "w1")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 || !reflect.DeepEqual(artifacts[0], artifact) {
		t.Fatalf("artifacts = %+v", artifacts)
	}

	review := work.Review{
		ID:         "r1",
		WorkItemID: "w1",
		ReviewerID: "verifier-1",
		Verdict:    work.VerdictApprove,
		Reasons:    "tests pass",
		Evidence:   []string{"a1"},
		CreatedAt:  200,
	}
	if err := s.CreateReview(ctx, review); err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	reviews, err := s.ListReviews(ctx, "w1")
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 1 || !reflect.DeepEqual(reviews[0], review) {
		t.Fatalf("reviews = %+v", reviews)
	}

	for i, typ := range []string{"claim", "submit"} {
		if err := s.AppendEvent(ctx, work.Event{
			WorkItemID: "w1",
			Type:       typ,
			Actor:      "worker-1",
			Payload:    []byte(`{"ok":true}`),
			CreatedAt:  int64(300 + i),
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	events, err := s.ListEvents(ctx, "w1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 || events[0].Type != "claim" || events[1].Type != "submit" {
		t.Fatalf("events = %+v", events)
	}
}
