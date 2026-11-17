// Package contextpack assembles the bounded text returned by work_claim.
// It is the only handover format: a brief, the capsule projection, the last
// rejection and workspace facts. Conversation history is never included.
package contextpack

import (
	"fmt"
	"strings"

	"janus/internal/capsule"
	"janus/internal/work"
)

const (
	// DefaultBudget is the whole pack. Section caps sum to it so a pack can
	// never starve the fields that prevent rework.
	DefaultBudget = 2000
	BriefBudget   = 800
	CapsuleShare  = 600
	RejectBudget  = 200
	FactsBudget   = 400

	truncMarker = "\n[truncated]"
)

// WorkspaceFacts are the stable commands every agent in this workspace needs.
type WorkspaceFacts struct {
	Build string
	Test  string
	Lint  string
	Notes []string
}

// Input is everything the pack may draw from. Reviews should be oldest first;
// Build only looks at the most recent rejection.
type Input struct {
	Item    work.WorkItem
	Capsule *capsule.Capsule
	Reviews []work.Review
	Facts   WorkspaceFacts
	Budget  int
}

// Pack is the rendered handover.
type Pack struct {
	Text    string   `json:"text"`
	Tokens  int      `json:"tokens"`
	Budget  int      `json:"budget"`
	Omitted []string `json:"omitted,omitempty"`
}

// Build renders a pack that is guaranteed to fit its budget.
func Build(in Input) Pack {
	budget := in.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}

	pack := Pack{Budget: budget}
	remaining := budget

	var sections []string
	add := func(name, text string, max int) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if remaining <= 0 {
			pack.Omitted = append(pack.Omitted, name)
			return
		}
		if max > remaining {
			max = remaining
		}
		rendered, cut := truncateTokens(text, max)
		if rendered == "" {
			pack.Omitted = append(pack.Omitted, name)
			return
		}
		if cut {
			pack.Omitted = append(pack.Omitted, name)
		}
		sections = append(sections, rendered)
		remaining -= capsule.EstimateTokens(rendered)
	}

	add("brief", renderBrief(in.Item), BriefBudget)
	if in.Capsule != nil {
		add("capsule", "capsule:\n"+in.Capsule.Render(), CapsuleShare)
	}
	if review := LastRejection(in.Reviews); review != nil {
		add("last_review", renderRejection(review), RejectBudget)
	}
	add("workspace", renderFacts(in.Facts), FactsBudget)

	pack.Text = strings.Join(sections, "\n")
	pack.Tokens = capsule.EstimateTokens(pack.Text)
	return pack
}

// LastRejection returns the newest review that blocked the item, or nil.
func LastRejection(reviews []work.Review) *work.Review {
	for i := len(reviews) - 1; i >= 0; i-- {
		switch reviews[i].Verdict {
		case work.VerdictReject, work.VerdictNeedsHuman:
			return &reviews[i]
		}
	}
	return nil
}

func renderBrief(item work.WorkItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "work %s: %s\n", item.ID, item.Title)
	if brief := strings.TrimSpace(item.Brief); brief != "" {
		fmt.Fprintf(&b, "brief: %s\n", brief)
	}
	if goal := strings.TrimSpace(item.Contract.Goal); goal != "" {
		fmt.Fprintf(&b, "goal: %s\n", goal)
	}
	if len(item.Contract.Acceptance) > 0 {
		b.WriteString("acceptance:\n")
		for _, line := range item.Contract.Acceptance {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(line))
		}
	}
	if len(item.Contract.Checks) > 0 {
		b.WriteString("checks:\n")
		for _, check := range item.Contract.Checks {
			fmt.Fprintf(&b, "- %s: `%s` (exit %d", check.Name, check.Command, check.ExpectExit)
			if check.TimeoutSeconds > 0 {
				fmt.Fprintf(&b, ", timeout %ds", check.TimeoutSeconds)
			}
			b.WriteString(")\n")
		}
	}
	if len(item.Contract.Paths) > 0 {
		fmt.Fprintf(&b, "paths: %s\n", strings.Join(item.Contract.Paths, ", "))
	}
	if len(item.DependsOn) > 0 {
		fmt.Fprintf(&b, "depends_on: %s\n", strings.Join(item.DependsOn, ", "))
	}
	return b.String()
}

func renderRejection(review *work.Review) string {
	var b strings.Builder
	fmt.Fprintf(&b, "last review %s: %s\n", review.Verdict, strings.TrimSpace(review.Reasons))
	for _, check := range review.Checks {
		if check.ExitCode == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s exit=%d: %s\n", check.Name, check.ExitCode, strings.TrimSpace(check.OutputTail))
	}
	return b.String()
}

func renderFacts(facts WorkspaceFacts) string {
	var b strings.Builder
	writeFact := func(name, value string) {
		if value = strings.TrimSpace(value); value != "" {
			fmt.Fprintf(&b, "%s: %s\n", name, value)
		}
	}
	writeFact("build", facts.Build)
	writeFact("test", facts.Test)
	writeFact("lint", facts.Lint)
	for _, note := range facts.Notes {
		if note = strings.TrimSpace(note); note != "" {
			fmt.Fprintf(&b, "- %s\n", note)
		}
	}
	return b.String()
}

// truncateTokens cuts s to at most max estimated tokens. The marker itself is
// counted, so the result never exceeds the limit.
func truncateTokens(s string, max int) (string, bool) {
	if max <= 0 {
		return "", true
	}
	if capsule.EstimateTokens(s) <= max {
		return s, false
	}

	markerCost := capsule.EstimateTokens(truncMarker)
	if max <= markerCost {
		return "", true
	}
	limit := max - markerCost

	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if capsule.EstimateTokens(string(runes[:mid])) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + truncMarker, true
}
