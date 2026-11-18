package worksvc

import (
	"context"
	"errors"
	"testing"

	"janus/internal/scoring"
	"janus/internal/work"
)

func scoredService(t *testing.T) (*Service, *work.Board, *scoring.Engine, *int64) {
	t.Helper()
	svc, board, _, now := setup(t)
	engine := scoring.New(scoring.Config{Now: func() int64 { return *now }})
	svc.Scores = engine
	return svc, board, engine, now
}

func addItem(t *testing.T, board *work.Board, id string, risk work.Risk) work.WorkItem {
	t.Helper()
	item := work.WorkItem{
		ID:          id,
		WorkspaceID: "ws",
		Title:       id,
		Contract:    work.Contract{Goal: "ship", Risk: risk, Paths: []string{"internal/work"}},
	}
	if err := board.Add(context.Background(), item); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
	stored, ok := board.Get(context.Background(), id)
	if !ok {
		t.Fatalf("item %s missing", id)
	}
	return stored
}

func TestScoringGatesWorkerClaim(t *testing.T) {
	ctx := context.Background()
	svc, board, _, _ := scoredService(t)
	addItem(t, board, "w2", work.RiskMedium)

	_, err := svc.Claim(ctx, "w2", work.Claimant{AgentID: "agent-worker", ModelID: "m1"}, work.RoleWorker, "p", 60)
	if !errors.Is(err, ErrTierDenied) {
		t.Fatalf("Claim = %v, want ErrTierDenied", err)
	}
}

func TestScoringAllowsLowRiskForProbation(t *testing.T) {
	ctx := context.Background()
	svc, board, engine, _ := scoredService(t)
	item := addItem(t, board, "w2", work.RiskLow)

	result, err := svc.Claim(ctx, "w2", work.Claimant{AgentID: "agent-worker", ModelID: "m1"}, work.RoleWorker, "p", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if result.Claim.ModelID != "m1" {
		t.Fatalf("claim model = %q", result.Claim.ModelID)
	}

	key := ScoreKey(result.Claim.Claimant(), item)
	if _, ok := engine.Get(key); ok {
		t.Fatal("a claim alone should not create a score")
	}
}

func TestScoringRequiresModelID(t *testing.T) {
	ctx := context.Background()
	svc, board, _, _ := scoredService(t)
	addItem(t, board, "w2", work.RiskLow)

	_, err := svc.Claim(ctx, "w2", work.Claimant{AgentID: "agent-worker"}, work.RoleWorker, "p", 60)
	if !errors.Is(err, ErrModelRequired) {
		t.Fatalf("Claim = %v, want ErrModelRequired", err)
	}
}

func TestVerdictRecordsScoringEvent(t *testing.T) {
	ctx := context.Background()
	svc, board, engine, _ := scoredService(t)
	item := addItem(t, board, "w2", work.RiskLow)
	who := work.Claimant{AgentID: "agent-worker", ModelID: "m1", ModelVersion: "v1", ToolMode: "native", TaskClass: "default"}

	claimed, err := svc.Claim(ctx, "w2", who, work.RoleWorker, "p", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := svc.Submit(ctx, "w2", who.AgentID, claimed.Claim.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	verifier, err := svc.Claim(ctx, "w2", work.Claimant{AgentID: "verifier-1", ModelID: "m-verify"}, work.RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}
	if err := svc.Verdict(ctx, "w2", "verifier-1", verifier.Claim.LeaseEpoch, work.VerdictApprove, "ok", nil, ""); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	key := ScoreKey(who, item)
	events := engine.Events(&key)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != scoring.EventVerdict || events[0].Verdict != work.VerdictApprove {
		t.Fatalf("event = %+v", events[0])
	}
	if events[0].Attempt != 1 {
		t.Fatalf("attempt = %d", events[0].Attempt)
	}

	score, ok := engine.Get(key)
	if !ok || score.Samples != 1 || score.Approvals != 1 {
		t.Fatalf("score = %+v ok=%v", score, ok)
	}
}

func TestRejectRequiresFailureOwner(t *testing.T) {
	ctx := context.Background()
	svc, board, _, _ := scoredService(t)
	addItem(t, board, "w2", work.RiskLow)
	who := work.Claimant{AgentID: "agent-worker", ModelID: "m1"}

	claimed, err := svc.Claim(ctx, "w2", who, work.RoleWorker, "p", 60)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := svc.Submit(ctx, "w2", who.AgentID, claimed.Claim.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	verifier, err := svc.Claim(ctx, "w2", work.Claimant{AgentID: "verifier-1"}, work.RoleVerifier, "review", 60)
	if err != nil {
		t.Fatalf("verifier claim: %v", err)
	}

	err = svc.Verdict(ctx, "w2", "verifier-1", verifier.Claim.LeaseEpoch, work.VerdictReject, "bad", nil, "")
	if !errors.Is(err, ErrFailureOwnerRequired) {
		t.Fatalf("Verdict = %v, want ErrFailureOwnerRequired", err)
	}

	// The lease is untouched by the rejected call.
	if active, ok := board.ClaimOf(ctx, "w2"); !ok || active.LeaseEpoch != verifier.Claim.LeaseEpoch {
		t.Fatalf("claim = %+v ok=%v", active, ok)
	}
}

func TestNonModelRejectDoesNotCount(t *testing.T) {
	ctx := context.Background()
	svc, board, engine, _ := scoredService(t)
	addItem(t, board, "w2", work.RiskLow)
	who := work.Claimant{AgentID: "agent-worker", ModelID: "m1"}

	claimed, _ := svc.Claim(ctx, "w2", who, work.RoleWorker, "p", 60)
	if err := svc.Submit(ctx, "w2", who.AgentID, claimed.Claim.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	verifier, _ := svc.Claim(ctx, "w2", work.Claimant{AgentID: "verifier-1"}, work.RoleVerifier, "review", 60)

	if err := svc.Verdict(ctx, "w2", "verifier-1", verifier.Claim.LeaseEpoch, work.VerdictReject, "harness timeout", nil, scoring.OwnerHarness); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	score, ok := engine.Get(ClaimKey(claimed.Claim))
	if !ok {
		t.Fatal("no score recorded for the attempt")
	}
	if score.Samples != 0 || score.RejectStreak != 0 {
		t.Fatalf("harness failure changed the model score: %+v", score)
	}
}

func TestCompleteRecordsMergeCycle(t *testing.T) {
	ctx := context.Background()
	svc, board, engine, now := scoredService(t)
	addItem(t, board, "w2", work.RiskLow)
	who := work.Claimant{AgentID: "agent-worker", ModelID: "m1"}

	worker, _ := svc.Claim(ctx, "w2", who, work.RoleWorker, "p", 60)
	if err := svc.Submit(ctx, "w2", who.AgentID, worker.Claim.LeaseEpoch, "k1", work.Evidence{Artifacts: []string{"a1"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	verifier, _ := svc.Claim(ctx, "w2", work.Claimant{AgentID: "verifier-1"}, work.RoleVerifier, "review", 60)
	if err := svc.Verdict(ctx, "w2", "verifier-1", verifier.Claim.LeaseEpoch, work.VerdictApprove, "ok", nil, ""); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	*now += 120
	integrator, err := svc.Claim(ctx, "w2", work.Claimant{AgentID: "integrator-1", ModelID: "m-int"}, work.RoleIntegrator, "merge", 60)
	if err != nil {
		t.Fatalf("integrator claim: %v", err)
	}
	if err := svc.Complete(ctx, "w2", "integrator-1", integrator.Claim.LeaseEpoch, "done-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	events := engine.Events(nil)
	var merge *scoring.Event
	for i := range events {
		if events[i].Type == scoring.EventMerge {
			merge = &events[i]
		}
	}
	if merge == nil {
		t.Fatalf("no merge event: %+v", events)
	}
	if merge.CycleSeconds != 120 {
		t.Fatalf("cycle = %d, want 120", merge.CycleSeconds)
	}
}

func TestTaskClassAndSensitivity(t *testing.T) {
	item := work.WorkItem{Contract: work.Contract{Paths: []string{"internal/auth/capability.go"}}}
	if got := TaskClassFor(item); got != "security" {
		t.Fatalf("task class = %q", got)
	}
	if !SensitiveItem(item) {
		t.Fatal("auth path should be sensitive")
	}

	docs := work.WorkItem{Contract: work.Contract{Paths: []string{"docs/REFACTOR.md"}}}
	if got := TaskClassFor(docs); got != "docs" {
		t.Fatalf("task class = %q", got)
	}
	if SensitiveItem(docs) {
		t.Fatal("docs path should not be sensitive")
	}

	critical := work.WorkItem{Contract: work.Contract{Risk: work.RiskCritical}}
	if !SensitiveItem(critical) {
		t.Fatal("critical risk should be sensitive")
	}
}

func TestAssignBypassesTierGate(t *testing.T) {
	ctx := context.Background()
	svc, board, _, _ := scoredService(t)
	addItem(t, board, "w2", work.RiskMedium)

	target := work.Claimant{AgentID: "agent-worker", ModelID: "m1"}
	if _, err := svc.Claim(ctx, "w2", target, work.RoleWorker, "p", 60); !errors.Is(err, ErrTierDenied) {
		t.Fatalf("Claim = %v, want ErrTierDenied", err)
	}

	result, err := svc.Assign(ctx, "w2", "admin-1", target, work.RoleWorker, "forced fix", 60)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !result.Claim.Forced {
		t.Fatal("assigned claim is not marked forced")
	}
	if result.Claim.AgentID != "agent-worker" || result.Claim.ModelID != "m1" {
		t.Fatalf("claim = %+v", result.Claim)
	}
	if result.Pack.Text == "" {
		t.Fatal("forced assignment did not return a pack")
	}
}
