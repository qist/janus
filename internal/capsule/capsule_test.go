package capsule

import (
	"errors"
	"strings"
	"testing"
)

func fixedClock(ts int64) func() int64 {
	return func() int64 { return ts }
}

func TestBudgetFor(t *testing.T) {
	cases := map[Kind]int{
		KindDefault:  DefaultBudget,
		KindDocs:     DocsBudget,
		KindTests:    TestsBudget,
		KindInfra:    InfraBudget,
		KindSecurity: SecurityBudget,
	}
	for kind, want := range cases {
		if got := BudgetFor(kind); got != want {
			t.Fatalf("BudgetFor(%q) = %d, want %d", kind, got, want)
		}
		if got := BudgetFor(kind); got > HardLimit {
			t.Fatalf("BudgetFor(%q) = %d, exceeds HardLimit %d", kind, got, HardLimit)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("cap-1", "w1", KindDocs, fixedClock(100))
	if c.Version != 1 || c.CreatedAt != 100 || c.UpdatedAt != 100 {
		t.Fatalf("capsule = %+v", c)
	}
	if c.Budget() != DocsBudget {
		t.Fatalf("budget = %d, want %d", c.Budget(), DocsBudget)
	}
	if c.UsedTokens() != 0 || c.Remaining() != DocsBudget {
		t.Fatalf("used = %d, remaining = %d", c.UsedTokens(), c.Remaining())
	}
}

func TestBudgetExceededLeavesCapsuleUnchanged(t *testing.T) {
	c := New("cap-1", "w1", KindDocs, fixedClock(100))

	err := c.AddConstraint(strings.Repeat("x", 4000))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("AddConstraint = %v, want ErrBudgetExceeded", err)
	}
	if len(c.Constraints) != 0 {
		t.Fatalf("constraints = %v, want none", c.Constraints)
	}
	if c.Version != 1 {
		t.Fatalf("version = %d, want 1", c.Version)
	}

	if err := c.AddConstraint("stay inside internal/work"); err != nil {
		t.Fatalf("AddConstraint: %v", err)
	}
	if c.Version != 2 || c.UpdatedAt != 100 {
		t.Fatalf("version = %d updated = %d", c.Version, c.UpdatedAt)
	}
}

func TestSetGoalRejectsOverflow(t *testing.T) {
	c := New("cap-1", "w1", KindDocs, fixedClock(100))
	if err := c.SetGoal("short goal"); err != nil {
		t.Fatalf("SetGoal: %v", err)
	}
	before := c.Goal

	if err := c.SetGoal(strings.Repeat("g", 5000)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("SetGoal = %v, want ErrBudgetExceeded", err)
	}
	if c.Goal != before {
		t.Fatalf("goal = %q, want %q", c.Goal, before)
	}
}

func TestArtifactDedup(t *testing.T) {
	c := New("cap-1", "w1", KindDefault, fixedClock(100))

	ref := ArtifactRef{ID: "a1", Kind: "diff", SHA256: "sha-1", URI: "artifacts/a1.diff"}
	if err := c.AddArtifact(ref); err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	version := c.Version

	if err := c.AddArtifact(ref); err != nil {
		t.Fatalf("duplicate AddArtifact: %v", err)
	}
	if err := c.AddArtifact(ArtifactRef{ID: "a2", Kind: "log", SHA256: "sha-1"}); err != nil {
		t.Fatalf("same-hash AddArtifact: %v", err)
	}
	if len(c.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want 1", c.Artifacts)
	}
	if c.Version != version {
		t.Fatalf("version changed on duplicate: %d -> %d", version, c.Version)
	}
}

func TestRenderIncludesBoundedSections(t *testing.T) {
	c := New("cap-1", "w1", KindDefault, fixedClock(100))
	if err := c.SetGoal("ship the work board"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddConstraint("no network"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddInvariant("do not rewrite history"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDecision("use transactions", "item+claim+event must commit together"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddFailure(1, "tests failed", "missing index"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddArtifact(ArtifactRef{ID: "a1", Kind: "diff", SHA256: "sha-1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddOpenQuestion("who approves L2 merges?"); err != nil {
		t.Fatal(err)
	}

	out := c.Render()
	for _, want := range []string{
		"capsule cap-1 v8", "kind=default", "goal: ship the work board",
		"constraints:", "no network", "invariants:", "do not rewrite history",
		"decisions:", "use transactions", "failures:", "attempt 1: tests failed",
		"artifacts:", "diff a1", "open:", "who approves L2 merges?",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render missing %q:\n%s", want, out)
		}
	}
	if c.UsedTokens() > c.Budget() {
		t.Fatalf("used %d > budget %d", c.UsedTokens(), c.Budget())
	}
}

func TestTrimDropsLowestPriorityFirst(t *testing.T) {
	c := New("cap-1", "w1", KindDefault, fixedClock(100))
	c.Goal = "goal"
	c.Constraints = []string{"keep"}
	c.Decisions = []Decision{{Text: strings.Repeat("d", 200)}}
	c.Failures = []Failure{{Attempt: 1, Summary: strings.Repeat("f", 200)}}
	c.OpenQuestions = []string{strings.Repeat("q", 200)}
	c.Artifacts = []ArtifactRef{{ID: "a1", Kind: "diff"}}
	c.budget = 120

	removed := c.Trim()
	if removed == 0 {
		t.Fatal("Trim removed nothing")
	}
	if c.UsedTokens() > c.budget {
		t.Fatalf("still over budget: %d > %d", c.UsedTokens(), c.budget)
	}
	if c.Goal != "goal" || len(c.Constraints) != 1 {
		t.Fatalf("protected fields were dropped: %+v", c)
	}
	if c.Version != 2 {
		t.Fatalf("version = %d, want 2 after trim", c.Version)
	}
}

func TestTrimKeepsProtectedFieldsAndReportsFailure(t *testing.T) {
	c := New("cap-1", "w1", KindDefault, fixedClock(100))
	c.Constraints = []string{strings.Repeat("c", 4000)}
	c.budget = 10

	if removed := c.Trim(); removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if len(c.Constraints) != 1 {
		t.Fatal("constraint was dropped")
	}
}

func TestStaleAndRebase(t *testing.T) {
	c := New("cap-1", "w1", KindDefault, fixedClock(100))

	if c.StaleFor("abc") {
		t.Fatal("capsule without base commit should not be stale")
	}
	c.Rebase("abc")
	if c.StaleFor("abc") {
		t.Fatal("same commit reported stale")
	}
	if !c.StaleFor("def") {
		t.Fatal("different commit not reported stale")
	}

	c.MarkStale()
	if !c.StaleFlag || !strings.Contains(c.Render(), "stale=true") {
		t.Fatalf("stale flag not rendered: %s", c.Render())
	}

	before := c.Version
	c.Rebase("def")
	if c.StaleFlag || c.BaseCommit != "def" {
		t.Fatalf("rebase failed: %+v", c)
	}
	if c.Version <= before {
		t.Fatalf("version = %d, want > %d", c.Version, before)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abcd", 1},
		{"abcde", 2},
		{"测试", 2},
		{"a测", 2},
	}
	for _, tc := range cases {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Fatalf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestAddRejectsEmptyEntries(t *testing.T) {
	c := New("cap-1", "w1", KindDefault, fixedClock(100))
	if err := c.AddConstraint("   "); err == nil {
		t.Fatal("AddConstraint accepted blank text")
	}
	if err := c.AddDecision("", "x"); err == nil {
		t.Fatal("AddDecision accepted blank text")
	}
	if err := c.AddFailure(1, "", "x"); err == nil {
		t.Fatal("AddFailure accepted blank summary")
	}
}
