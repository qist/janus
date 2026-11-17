package scoring

import (
	"testing"

	"janus/internal/work"
)

func testKey() Key {
	return Key{AgentID: "agent-1", ModelID: "model-1", TaskClass: "refactor", ToolMode: "native", ModelVersion: "v1"}
}

func newTestEngine() (*Engine, Key) {
	now := int64(1_700_000_000)
	return New(Config{Now: func() int64 { return now }}), testKey()
}

func approve(t *testing.T, e *Engine, key Key, attempt int) Score {
	t.Helper()
	score, err := e.Record(Event{Key: key, Type: EventVerdict, Verdict: work.VerdictApprove, Attempt: attempt})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return score
}

func reject(t *testing.T, e *Engine, key Key, owner FailureOwner) Score {
	t.Helper()
	score, err := e.Record(Event{Key: key, Type: EventVerdict, Verdict: work.VerdictReject, Owner: owner, Attempt: 1})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	return score
}

func TestNewKeyStartsOnProbation(t *testing.T) {
	e, key := newTestEngine()
	score, ok := e.Get(key)
	if ok {
		t.Fatalf("unknown key exists: %+v", score)
	}

	allowed, reason := e.CanClaim(key, Requirement{Risk: work.RiskLow})
	if !allowed {
		t.Fatalf("low-risk claim denied for a new key: %s", reason)
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskMedium}); allowed {
		t.Fatal("probation may not claim medium-risk items")
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskLow, Sensitive: true}); allowed {
		t.Fatal("probation may not claim sensitive items")
	}
}

func TestFiveApprovalsPromoteToActive(t *testing.T) {
	e, key := newTestEngine()

	for i := 0; i < 5; i++ {
		if _, err := e.Record(Event{Key: key, Type: EventClaim}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		approve(t, e, key, 1)
	}

	score, _ := e.Get(key)
	if score.Samples != 5 || score.FirstPass != 5 || score.Approvals != 5 {
		t.Fatalf("score = %+v", score)
	}
	if score.Tier != TierActive {
		t.Fatalf("tier = %s, want active (score %d)", score.Tier, score.Score)
	}
	if allowed, reason := e.CanClaim(key, Requirement{Risk: work.RiskMedium}); !allowed {
		t.Fatalf("active cannot claim medium risk: %s", reason)
	}
}

func TestTrustedRequiresTwentySamples(t *testing.T) {
	e, key := newTestEngine()

	for i := 0; i < 10; i++ {
		approve(t, e, key, 1)
	}
	score, _ := e.Get(key)
	if score.Tier == TierTrusted {
		t.Fatalf("trusted too early: %+v", score)
	}

	for i := 0; i < 10; i++ {
		approve(t, e, key, 1)
	}
	score, _ = e.Get(key)
	if score.Tier != TierTrusted {
		t.Fatalf("tier = %s, want trusted (%+v)", score.Tier, score)
	}
	if allowed, reason := e.CanClaim(key, Requirement{Risk: work.RiskHigh, Sensitive: true}); !allowed {
		t.Fatalf("trusted cannot claim sensitive: %s", reason)
	}
}

func TestSecondModelRejectDropsToProbation(t *testing.T) {
	e, key := newTestEngine()
	approve(t, e, key, 1)
	approve(t, e, key, 1)
	approve(t, e, key, 1)
	approve(t, e, key, 1)
	approve(t, e, key, 1)

	if score, _ := e.Get(key); score.Tier != TierActive {
		t.Fatalf("setup tier = %s", score.Tier)
	}

	reject(t, e, key, OwnerModel)
	score, _ := e.Get(key)
	if score.Tier != TierActive {
		t.Fatalf("tier after one reject = %s, want active", score.Tier)
	}

	reject(t, e, key, OwnerModel)
	score, _ = e.Get(key)
	if score.Tier != TierProbation {
		t.Fatalf("tier after two rejects = %s, want probation", score.Tier)
	}
	if score.RejectStreak != 2 {
		t.Fatalf("streak = %d", score.RejectStreak)
	}
}

func TestThirdModelRejectSuspends(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 3; i++ {
		reject(t, e, key, OwnerModel)
	}
	score, _ := e.Get(key)
	if score.Tier != TierSuspended || !score.Suspended {
		t.Fatalf("score = %+v", score)
	}
	if allowed, reason := e.CanClaim(key, Requirement{Risk: work.RiskLow}); allowed {
		t.Fatalf("suspended key allowed: %s", reason)
	}
}

func TestNonModelRejectsDoNotPunishScore(t *testing.T) {
	e, key := newTestEngine()
	approve(t, e, key, 1)
	before, _ := e.Get(key)

	for _, owner := range []FailureOwner{OwnerClient, OwnerHarness, OwnerTask, OwnerAccount, OwnerInfra} {
		reject(t, e, key, owner)
	}
	after, _ := e.Get(key)
	if after.Samples != before.Samples || after.RejectStreak != 0 {
		t.Fatalf("non-model failure changed the score: %+v", after)
	}
	if after.Score != before.Score {
		t.Fatalf("score changed: %d -> %d", before.Score, after.Score)
	}
}

func TestLeaseExpiryRateDropsToProbation(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 10; i++ {
		if _, err := e.Record(Event{Key: key, Type: EventClaim}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := e.Record(Event{Key: key, Type: EventLeaseExpiry}); err != nil {
			t.Fatal(err)
		}
	}

	score, _ := e.Get(key)
	if score.LeaseExpiryRate() <= 0.20 {
		t.Fatalf("rate = %v", score.LeaseExpiryRate())
	}
	if score.Tier != TierProbation {
		t.Fatalf("tier = %s, want probation", score.Tier)
	}
}

func TestLeaseExpiryUnderTwentyPercentIsFine(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 10; i++ {
		if _, err := e.Record(Event{Key: key, Type: EventClaim}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Record(Event{Key: key, Type: EventLeaseExpiry}); err != nil {
		t.Fatal(err)
	}
	score, _ := e.Get(key)
	if score.Tier != TierProbation {
		// Still probation because there are no approvals, but not for reliability.
		t.Fatalf("tier = %s", score.Tier)
	}
	if score.Reliability() != 0.9 {
		t.Fatalf("reliability = %v", score.Reliability())
	}
}

func TestQualityGateBlocksCheapFastDelivery(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 5; i++ {
		approve(t, e, key, 1)
	}
	// Six more attempts, mostly failures: enough samples for the gate.
	for i := 0; i < 5; i++ {
		approve(t, e, key, 1)
		reject(t, e, key, OwnerModel)
	}

	score, _ := e.Get(key)
	if score.Samples < 10 {
		t.Fatalf("samples = %d", score.Samples)
	}
	if score.Tier != TierProbation {
		t.Fatalf("tier = %s, want probation (wilson %.3f firstPass %.3f)", score.Tier,
			wilsonLower(score.Approvals, score.Samples, 1.96), score.FirstPassRate())
	}
}

func TestIncidentSuspendsImmediately(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 5; i++ {
		approve(t, e, key, 1)
	}

	score, err := e.Record(Event{Key: key, Type: EventIncident, Incident: IncidentFakeEvidence})
	if err != nil {
		t.Fatalf("incident: %v", err)
	}
	if !score.Suspended || score.Tier != TierSuspended {
		t.Fatalf("score = %+v", score)
	}
	if score.Penalty != 30 {
		t.Fatalf("penalty = %d", score.Penalty)
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskLow}); allowed {
		t.Fatal("suspended key allowed to claim")
	}
}

func TestReinstateReturnsToProbation(t *testing.T) {
	e, key := newTestEngine()
	if _, err := e.Record(Event{Key: key, Type: EventIncident, Incident: IncidentSecurity}); err != nil {
		t.Fatal(err)
	}

	score, err := e.Record(Event{Key: key, Type: EventOverride, Override: OverrideReinstate})
	if err != nil {
		t.Fatal(err)
	}
	if score.Suspended || score.Tier != TierProbation {
		t.Fatalf("score = %+v", score)
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskLow}); !allowed {
		t.Fatal("reinstated key cannot claim low risk")
	}
}

func TestPinSurvivesAutomaticDowngrade(t *testing.T) {
	e, key := newTestEngine()
	if _, err := e.Record(Event{Key: key, Type: EventOverride, Override: OverridePin, Tier: TierTrusted}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		reject(t, e, key, OwnerModel)
	}
	score, _ := e.Get(key)
	if score.Tier != TierTrusted || !score.Pinned {
		t.Fatalf("score = %+v", score)
	}

	if _, err := e.Record(Event{Key: key, Type: EventOverride, Override: OverrideUnpin}); err != nil {
		t.Fatal(err)
	}
	score, _ = e.Get(key)
	if score.Tier != TierSuspended {
		t.Fatalf("tier after unpin = %s, want suspended", score.Tier)
	}
}

func TestResetClearsEverything(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 5; i++ {
		approve(t, e, key, 1)
	}
	if _, err := e.Record(Event{Key: key, Type: EventIncident, Incident: IncidentData}); err != nil {
		t.Fatal(err)
	}

	score, err := e.Record(Event{Key: key, Type: EventReset})
	if err != nil {
		t.Fatal(err)
	}
	if score.Score != 0 || score.Samples != 0 || score.Penalty != 0 || score.Tier != TierProbation || score.Suspended {
		t.Fatalf("score = %+v", score)
	}
}

func TestCostAndCycleMedians(t *testing.T) {
	e, key := newTestEngine()
	if _, err := e.Record(Event{Key: key, Type: EventMerge, CostTokens: 500, CycleSeconds: 40}); err != nil {
		t.Fatal(err)
	}
	score, err := e.Record(Event{Key: key, Type: EventMerge, CostTokens: 300, CycleSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	if score.CostMedian != 400 {
		t.Fatalf("cost median = %d, want 400", score.CostMedian)
	}
	if score.CycleMedian != 30 {
		t.Fatalf("cycle median = %d, want 30", score.CycleMedian)
	}
}

func TestConfigTargetsLowerResourceContribution(t *testing.T) {
	key := testKey()
	cheap := New(Config{Now: func() int64 { return 1 }, BudgetTokens: 1000, TargetSeconds: 100})
	expensive := New(Config{Now: func() int64 { return 1 }, BudgetTokens: 100, TargetSeconds: 10})

	for _, engine := range []*Engine{cheap, expensive} {
		for i := 0; i < 5; i++ {
			if _, err := engine.Record(Event{Key: key, Type: EventVerdict, Verdict: work.VerdictApprove, Attempt: 1}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := engine.Record(Event{Key: key, Type: EventMerge, CostTokens: 500, CycleSeconds: 50}); err != nil {
			t.Fatal(err)
		}
	}

	cheapScore, _ := cheap.Get(key)
	expensiveScore, _ := expensive.Get(key)
	if cheapScore.Score <= expensiveScore.Score {
		t.Fatalf("budget 1000 scored %d, budget 100 scored %d; cheap delivery should score higher",
			cheapScore.Score, expensiveScore.Score)
	}
}

func TestCanClaimProbationBudgetCap(t *testing.T) {
	e, key := newTestEngine()

	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskLow, BudgetTokens: 400, BudgetLimit: 1000}); !allowed {
		t.Fatal("400/1000 should be allowed")
	}
	if allowed, reason := e.CanClaim(key, Requirement{Risk: work.RiskLow, BudgetTokens: 600, BudgetLimit: 1000}); allowed {
		t.Fatalf("600/1000 should be denied: %s", reason)
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskLow, Priority: 10}); allowed {
		t.Fatal("probation may not claim high priority")
	}
}

func TestActiveCannotClaimSensitive(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 5; i++ {
		approve(t, e, key, 1)
	}
	if allowed, reason := e.CanClaim(key, Requirement{Risk: work.RiskHigh, Sensitive: true}); allowed {
		t.Fatalf("active claimed a sensitive item: %s", reason)
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskCritical}); allowed {
		t.Fatal("active claimed a critical item")
	}
}

func TestEventsAreAppendOnlyWithDelta(t *testing.T) {
	e, key := newTestEngine()
	other := Key{AgentID: "agent-2", ModelID: "model-1"}

	score, err := e.Record(Event{Key: key, Type: EventVerdict, Verdict: work.VerdictApprove, Attempt: 1, Actor: "verifier-1"})
	if err != nil {
		t.Fatal(err)
	}
	if score.Score == 0 {
		t.Fatal("first approval scored zero")
	}
	if _, err := e.Record(Event{Key: other, Type: EventClaim}); err != nil {
		t.Fatal(err)
	}

	events := e.Events(&key)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].ID == "" || events[0].CreatedAt == 0 {
		t.Fatalf("event = %+v", events[0])
	}
	if events[0].Delta != score.Score {
		t.Fatalf("delta = %d, want %d", events[0].Delta, score.Score)
	}
	if events[0].Actor != "verifier-1" {
		t.Fatalf("actor = %q", events[0].Actor)
	}

	if all := e.Events(nil); len(all) != 2 {
		t.Fatalf("all events = %d", len(all))
	}
}

func TestRejectDeltaIsNegative(t *testing.T) {
	e, key := newTestEngine()
	for i := 0; i < 3; i++ {
		approve(t, e, key, 1)
	}
	before, _ := e.Get(key)
	score := reject(t, e, key, OwnerModel)
	if score.Score >= before.Score {
		t.Fatalf("score did not drop: %d -> %d", before.Score, score.Score)
	}
	events := e.Events(&key)
	last := events[len(events)-1]
	if last.Delta >= 0 {
		t.Fatalf("delta = %d, want negative", last.Delta)
	}
}

func TestInvalidEvents(t *testing.T) {
	e, key := newTestEngine()
	if _, err := e.Record(Event{Type: EventClaim}); err == nil {
		t.Fatal("accepted an event without a key")
	}
	if _, err := e.Record(Event{Key: key, Type: "bogus"}); err == nil {
		t.Fatal("accepted an unknown event type")
	}
}

func TestWilsonLowerBound(t *testing.T) {
	small := wilsonLower(1, 1, 1.96)
	large := wilsonLower(20, 20, 1.96)
	if small >= large {
		t.Fatalf("1/1 = %.3f should be below 20/20 = %.3f", small, large)
	}
	if wilsonLower(0, 0, 1.96) != 0 {
		t.Fatal("empty sample should be zero")
	}
	if wilsonLower(0, 10, 1.96) != 0 {
		t.Fatal("no approvals should be zero")
	}
}

func TestScoresListingIsStable(t *testing.T) {
	e, key := newTestEngine()
	other := Key{AgentID: "agent-0", ModelID: "model-1"}
	if _, err := e.Record(Event{Key: key, Type: EventClaim}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Record(Event{Key: other, Type: EventClaim}); err != nil {
		t.Fatal(err)
	}

	scores := e.Scores()
	if len(scores) != 2 || scores[0].Key.AgentID != "agent-0" {
		t.Fatalf("scores = %+v", scores)
	}
}

func TestRetireIsSticky(t *testing.T) {
	e, key := newTestEngine()
	if _, err := e.Record(Event{Key: key, Type: EventOverride, Override: OverrideRetire}); err != nil {
		t.Fatal(err)
	}
	approve(t, e, key, 1)
	score, _ := e.Get(key)
	if score.Tier != TierRetired {
		t.Fatalf("tier = %s, want retired", score.Tier)
	}
	if allowed, _ := e.CanClaim(key, Requirement{Risk: work.RiskLow}); allowed {
		t.Fatal("retired key allowed to claim")
	}
}
