// Package capsule implements the bounded context capsule described in
// REFACTOR §12.3. A capsule is the only handover format between agents: it is
// versioned, capped by a token budget, and invalidated when the base commit
// moves. Conversation history is never a valid substitute.
package capsule

import (
	"errors"
	"fmt"
	"strings"
)

// Kind is the task class, which selects the token budget.
type Kind string

const (
	KindDefault  Kind = "default"
	KindDocs     Kind = "docs"
	KindTests    Kind = "tests"
	KindInfra    Kind = "infra"
	KindSecurity Kind = "security"
)

const (
	// DefaultBudget is the cap for ordinary code work. Issue/docs work gets
	// less, infra/security gets more, and nothing may exceed HardLimit.
	DefaultBudget  = 1200
	DocsBudget     = 800
	TestsBudget    = 800
	InfraBudget    = 1600
	SecurityBudget = 1600
	HardLimit      = 2000
)

// ErrBudgetExceeded means the write would push the capsule past its budget.
// The caller must archive or trim before retrying; content is left unchanged.
var ErrBudgetExceeded = errors.New("capsule budget exceeded")

// BudgetFor maps a task class to its token budget.
func BudgetFor(kind Kind) int {
	switch kind {
	case KindDocs, KindTests:
		return DocsBudget
	case KindInfra, KindSecurity:
		return InfraBudget
	default:
		return DefaultBudget
	}
}

// Decision is one choice that later agents must not silently revisit.
type Decision struct {
	Text      string `json:"text"`
	Rationale string `json:"rationale,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// Failure is one failed attempt, kept so the next agent does not repeat it.
type Failure struct {
	Attempt   int    `json:"attempt"`
	Summary   string `json:"summary"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// ArtifactRef points at evidence; the body stays in the workspace.
type ArtifactRef struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // diff|patch|log|test|report|note
	URI     string `json:"uri"`
	SHA256  string `json:"sha256,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Capsule is the bounded handover state of one work item.
type Capsule struct {
	ID            string `json:"id"`
	WorkItemID    string `json:"work_item_id"`
	WorkspaceID   string `json:"workspace_id"`
	Kind          Kind   `json:"kind"`
	Version       int    `json:"version"`
	BaseCommit    string `json:"base_commit"`
	PrefixVersion string `json:"prefix_version"`
	StaleFlag     bool   `json:"stale"`

	Goal          string        `json:"goal"`
	Constraints   []string      `json:"constraints,omitempty"`
	Invariants    []string      `json:"invariants,omitempty"`
	Decisions     []Decision    `json:"decisions,omitempty"`
	Failures      []Failure     `json:"failures,omitempty"`
	Artifacts     []ArtifactRef `json:"artifacts,omitempty"`
	OpenQuestions []string      `json:"open_questions,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	budget int
	now    func() int64
}

// New creates an empty capsule at version 1. now may be nil.
func New(id, workItemID string, kind Kind, now func() int64) *Capsule {
	if now == nil {
		now = func() int64 { return 0 }
	}
	budget := BudgetFor(kind)
	if budget > HardLimit {
		budget = HardLimit
	}
	ts := now()
	return &Capsule{
		ID:         id,
		WorkItemID: workItemID,
		Kind:       kind,
		Version:    1,
		CreatedAt:  ts,
		UpdatedAt:  ts,
		budget:     budget,
		now:        now,
	}
}

// Budget is the token cap for this capsule.
func (c *Capsule) Budget() int { return c.budget }

// UsedTokens is the estimated size of the rendered body.
func (c *Capsule) UsedTokens() int { return EstimateTokens(c.body()) }

// Remaining is the number of tokens still available.
func (c *Capsule) Remaining() int {
	left := c.budget - c.UsedTokens()
	if left < 0 {
		return 0
	}
	return left
}

func (c *Capsule) SetGoal(text string) error {
	old := c.Goal
	c.Goal = strings.TrimSpace(text)
	if c.UsedTokens() > c.budget {
		c.Goal = old
		return c.budgetError()
	}
	c.touch()
	return nil
}

func (c *Capsule) AddConstraint(text string) error {
	return c.appendString(&c.Constraints, text)
}

func (c *Capsule) AddInvariant(text string) error {
	return c.appendString(&c.Invariants, text)
}

func (c *Capsule) AddOpenQuestion(text string) error {
	return c.appendString(&c.OpenQuestions, text)
}

func (c *Capsule) AddDecision(text, rationale string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("decision text required")
	}
	c.Decisions = append(c.Decisions, Decision{Text: text, Rationale: strings.TrimSpace(rationale), CreatedAt: c.now()})
	if c.UsedTokens() > c.budget {
		c.Decisions = c.Decisions[:len(c.Decisions)-1]
		return c.budgetError()
	}
	c.touch()
	return nil
}

func (c *Capsule) AddFailure(attempt int, summary, reason string) error {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return errors.New("failure summary required")
	}
	c.Failures = append(c.Failures, Failure{Attempt: attempt, Summary: summary, Reason: strings.TrimSpace(reason), CreatedAt: c.now()})
	if c.UsedTokens() > c.budget {
		c.Failures = c.Failures[:len(c.Failures)-1]
		return c.budgetError()
	}
	c.touch()
	return nil
}

// AddArtifact stores a reference only. The same id or SHA256 is never added
// twice, so re-attaching one diff does not burn context.
func (c *Capsule) AddArtifact(ref ArtifactRef) error {
	for _, existing := range c.Artifacts {
		if ref.ID != "" && existing.ID == ref.ID {
			return nil
		}
		if ref.SHA256 != "" && existing.SHA256 == ref.SHA256 {
			return nil
		}
	}
	c.Artifacts = append(c.Artifacts, ref)
	if c.UsedTokens() > c.budget {
		c.Artifacts = c.Artifacts[:len(c.Artifacts)-1]
		return c.budgetError()
	}
	c.touch()
	return nil
}

// Trim drops the least valuable entries until the capsule fits: oldest
// decisions, then oldest failures, then open questions, then artifacts.
// Goal, constraints and invariants are never dropped — if they alone overflow,
// Trim returns 0 and the caller must shorten them.
func (c *Capsule) Trim() int {
	removed := 0
	for c.UsedTokens() > c.budget {
		switch {
		case len(c.Decisions) > 0:
			c.Decisions = c.Decisions[1:]
		case len(c.Failures) > 0:
			c.Failures = c.Failures[1:]
		case len(c.OpenQuestions) > 0:
			c.OpenQuestions = c.OpenQuestions[1:]
		case len(c.Artifacts) > 0:
			c.Artifacts = c.Artifacts[:len(c.Artifacts)-1]
		default:
			return removed
		}
		removed++
	}
	if removed > 0 {
		c.touch()
	}
	return removed
}

// StaleFor reports whether the capsule was built against a different commit.
func (c *Capsule) StaleFor(baseCommit string) bool {
	if c.BaseCommit == "" || baseCommit == "" {
		return false
	}
	return c.BaseCommit != baseCommit
}

// MarkStale flags the capsule after the underlying code moved.
func (c *Capsule) MarkStale() {
	if c.StaleFlag {
		return
	}
	c.StaleFlag = true
	c.touch()
}

// Rebase points the capsule at a new base commit and clears the stale flag.
func (c *Capsule) Rebase(baseCommit string) {
	c.BaseCommit = strings.TrimSpace(baseCommit)
	c.StaleFlag = false
	c.touch()
}

// Render returns the handover text. It never contains conversation history.
func (c *Capsule) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "capsule %s v%d kind=%s base=%s tokens=%d/%d",
		c.ID, c.Version, c.Kind, c.BaseCommit, c.UsedTokens(), c.budget)
	if c.StaleFlag {
		b.WriteString(" stale=true")
	}
	b.WriteString("\n")
	b.WriteString(c.body())
	return b.String()
}

func (c *Capsule) body() string {
	var b strings.Builder
	if c.Goal != "" {
		fmt.Fprintf(&b, "goal: %s\n", c.Goal)
	}
	writeList(&b, "constraints", c.Constraints)
	writeList(&b, "invariants", c.Invariants)

	if len(c.Decisions) > 0 {
		b.WriteString("decisions:\n")
		for _, d := range c.Decisions {
			fmt.Fprintf(&b, "- %s", d.Text)
			if d.Rationale != "" {
				fmt.Fprintf(&b, " (why: %s)", d.Rationale)
			}
			b.WriteString("\n")
		}
	}
	if len(c.Failures) > 0 {
		b.WriteString("failures:\n")
		for _, f := range c.Failures {
			fmt.Fprintf(&b, "- attempt %d: %s", f.Attempt, f.Summary)
			if f.Reason != "" {
				fmt.Fprintf(&b, " (%s)", f.Reason)
			}
			b.WriteString("\n")
		}
	}
	if len(c.Artifacts) > 0 {
		b.WriteString("artifacts:\n")
		for _, a := range c.Artifacts {
			fmt.Fprintf(&b, "- %s %s", a.Kind, a.ID)
			if a.SHA256 != "" {
				fmt.Fprintf(&b, " sha256=%s", a.SHA256)
			}
			if a.URI != "" {
				fmt.Fprintf(&b, " uri=%s", a.URI)
			}
			if a.Summary != "" {
				fmt.Fprintf(&b, " (%s)", a.Summary)
			}
			b.WriteString("\n")
		}
	}
	writeList(&b, "open", c.OpenQuestions)
	return b.String()
}

func (c *Capsule) appendString(dst *[]string, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("capsule entry required")
	}
	*dst = append(*dst, text)
	if c.UsedTokens() > c.budget {
		*dst = (*dst)[:len(*dst)-1]
		return c.budgetError()
	}
	c.touch()
	return nil
}

func (c *Capsule) budgetError() error {
	return fmt.Errorf("%w: %d/%d, archive or trim first", ErrBudgetExceeded, c.UsedTokens(), c.budget)
}

func (c *Capsule) touch() {
	c.Version++
	c.UpdatedAt = c.now()
}

func writeList(b *strings.Builder, name string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", name)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
}
