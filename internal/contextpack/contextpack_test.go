package contextpack

import (
	"strings"
	"testing"

	"janus/internal/capsule"
	"janus/internal/work"
)

func sampleItem() work.WorkItem {
	return work.WorkItem{
		ID:          "w1",
		WorkspaceID: "ws",
		Title:       "fix claim race",
		Brief:       "two agents can claim at once",
		Contract: work.Contract{
			Goal:       "one claim wins",
			Acceptance: []string{"second claim returns 409"},
			Checks: []work.Check{
				{Name: "unit", Command: "go test ./internal/work/...", TimeoutSeconds: 120},
			},
			Paths: []string{"internal/work"},
		},
		DependsOn: []string{"w0"},
	}
}

func sampleCapsule(t *testing.T) *capsule.Capsule {
	t.Helper()
	c := capsule.New("cap-1", "w1", capsule.KindDefault, func() int64 { return 100 })
	if err := c.SetGoal("one claim wins"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDecision("use CAS", "version compare in one transaction"); err != nil {
		t.Fatal(err)
	}
	return c
}

func rejection() work.Review {
	return work.Review{
		ID:         "rv_w1_2",
		WorkItemID: "w1",
		ReviewerID: "verifier-1",
		Verdict:    work.VerdictReject,
		Reasons:    "unit test failed",
		Checks: []work.CheckResult{
			{Name: "unit", ExitCode: 1, OutputTail: "FAIL: claim race"},
		},
	}
}

func TestBuildIncludesAllSections(t *testing.T) {
	pack := Build(Input{
		Item:    sampleItem(),
		Capsule: sampleCapsule(t),
		Reviews: []work.Review{rejection()},
		Facts: WorkspaceFacts{
			Build: "go build ./...",
			Test:  "go test ./...",
			Lint:  "go vet ./...",
			Notes: []string{"module janus"},
		},
	})

	for _, want := range []string{
		"work w1: fix claim race",
		"goal: one claim wins",
		"acceptance:",
		"checks:",
		"paths: internal/work",
		"depends_on: w0",
		"capsule cap-1",
		"use CAS",
		"last review reject: unit test failed",
		"unit exit=1: FAIL: claim race",
		"build: go build ./...",
		"test: go test ./...",
		"lint: go vet ./...",
	} {
		if !strings.Contains(pack.Text, want) {
			t.Fatalf("pack missing %q:\n%s", want, pack.Text)
		}
	}
	if pack.Tokens > pack.Budget {
		t.Fatalf("tokens %d > budget %d", pack.Tokens, pack.Budget)
	}
	if len(pack.Omitted) != 0 {
		t.Fatalf("omitted = %v, want none", pack.Omitted)
	}
	if pack.Budget != DefaultBudget {
		t.Fatalf("budget = %d, want %d", pack.Budget, DefaultBudget)
	}
}

func TestBuildWithoutCapsuleOrRejection(t *testing.T) {
	pack := Build(Input{Item: sampleItem()})
	if strings.Contains(pack.Text, "capsule:") || strings.Contains(pack.Text, "last review") {
		t.Fatalf("unexpected sections:\n%s", pack.Text)
	}
	if pack.Tokens > pack.Budget {
		t.Fatalf("tokens %d > budget %d", pack.Tokens, pack.Budget)
	}
}

func TestBuildRespectsTinyBudget(t *testing.T) {
	item := sampleItem()
	item.Brief = strings.Repeat("long brief ", 400)

	pack := Build(Input{
		Item:    item,
		Capsule: sampleCapsule(t),
		Reviews: []work.Review{rejection()},
		Facts:   WorkspaceFacts{Build: "go build ./..."},
		Budget:  40,
	})

	if pack.Tokens > 40 {
		t.Fatalf("tokens %d > budget 40", pack.Tokens)
	}
	if pack.Text == "" {
		t.Fatal("pack is empty; the brief is mandatory")
	}
	if len(pack.Omitted) == 0 {
		t.Fatalf("omitted = %v, want dropped sections", pack.Omitted)
	}
}

func TestBuildTruncatesOversizedCapsule(t *testing.T) {
	c := capsule.New("cap-1", "w1", capsule.KindDefault, func() int64 { return 100 })
	c.Decisions = []capsule.Decision{{Text: strings.Repeat("d", 4000)}}

	pack := Build(Input{
		Item:    sampleItem(),
		Capsule: c,
		Budget:  900,
	})

	if pack.Tokens > 900 {
		t.Fatalf("tokens %d > budget 900", pack.Tokens)
	}
	if !hasOmitted(pack.Omitted, "capsule") {
		t.Fatalf("omitted = %v, want capsule marked truncated", pack.Omitted)
	}
	if !strings.Contains(pack.Text, "[truncated]") {
		t.Fatalf("capsule was not truncated:\n%s", pack.Text)
	}
}

func TestLastRejection(t *testing.T) {
	reviews := []work.Review{
		{ID: "r1", Verdict: work.VerdictApprove},
		{ID: "r2", Verdict: work.VerdictReject},
		{ID: "r3", Verdict: work.VerdictNeedsHuman},
	}
	got := LastRejection(reviews)
	if got == nil || got.ID != "r3" {
		t.Fatalf("LastRejection = %+v, want r3", got)
	}

	if LastRejection(nil) != nil {
		t.Fatal("LastRejection(nil) should be nil")
	}
	if LastRejection([]work.Review{{Verdict: work.VerdictApprove}}) != nil {
		t.Fatal("approve-only history should have no rejection")
	}
}

func TestTruncateTokensNeverExceeds(t *testing.T) {
	long := strings.Repeat("abcdefghij ", 200)
	for max := 1; max <= 120; max++ {
		out, cut := truncateTokens(long, max)
		if got := capsule.EstimateTokens(out); got > max {
			t.Fatalf("max=%d produced %d tokens", max, got)
		}
		if !cut {
			t.Fatalf("max=%d did not report truncation", max)
		}
	}
}

func hasOmitted(omitted []string, name string) bool {
	for _, entry := range omitted {
		if entry == name {
			return true
		}
	}
	return false
}
