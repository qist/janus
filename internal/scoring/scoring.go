// Package scoring decides what an (agent, model, task class, tool mode) pair
// is allowed to claim. It only reads verifiable delivery outcomes: verifier
// verdicts, attempts, lease expiries and real usage. Self-reports are ignored.
//
// The score is explainable: every change is an append-only Event carrying the
// delta and the reason, so it can be recomputed and audited.
package scoring

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"janus/internal/work"
)

// Tier is what a pair may claim.
type Tier string

const (
	TierProbation Tier = "probation"
	TierActive    Tier = "active"
	TierTrusted   Tier = "trusted"
	TierSuspended Tier = "suspended"
	TierRetired   Tier = "retired"
)

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool {
	switch t {
	case TierProbation, TierActive, TierTrusted, TierSuspended, TierRetired:
		return true
	}
	return false
}

// Key identifies one scored pair. ModelVersion is part of the key: a model
// upgrade starts from zero instead of inheriting the old score.
type Key struct {
	AgentID      string `json:"agent_id"`
	ModelID      string `json:"model_id"`
	TaskClass    string `json:"task_class"`
	ToolMode     string `json:"tool_mode"` // native|bridge|none
	ModelVersion string `json:"model_version"`
}

// String is stable enough for logs.
func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", k.AgentID, k.ModelID, k.TaskClass, k.ToolMode, k.ModelVersion)
}

// EventType mirrors REFACTOR §8.4 plus lease_expired, which reliability needs as
// its own signal rather than an opaque incident.
type EventType string

const (
	EventClaim       EventType = "claim"
	EventSubmit      EventType = "submit"
	EventVerdict     EventType = "verdict"
	EventMerge       EventType = "merge"
	EventLeaseExpiry EventType = "lease_expired"
	EventIncident    EventType = "incident"
	EventOverride    EventType = "override"
	EventReset       EventType = "reset"
)

// FailureOwner attributes a failure. Only FailureModel moves the model score;
// account/harness/client problems must not punish the model.
type FailureOwner string

const (
	OwnerModel   FailureOwner = "model"
	OwnerHarness FailureOwner = "harness"
	OwnerClient  FailureOwner = "client"
	OwnerTask    FailureOwner = "task"
	OwnerAccount FailureOwner = "account"
	OwnerInfra   FailureOwner = "infra"
)

// Incident kinds that suspend immediately without waiting for a weighted score.
const (
	IncidentFakeEvidence = "fake_evidence"
	IncidentSecurity     = "security"
	IncidentData         = "data"
)

// Override actions available to an admin.
const (
	OverridePin       = "pin"
	OverrideUnpin     = "unpin"
	OverrideSuspend   = "suspend"
	OverrideReinstate = "reinstate"
	OverrideReset     = "reset"
	OverrideRetire    = "retire"
)

// Event is one append-only scoring change.
type Event struct {
	ID         string       `json:"id"`
	Key        Key          `json:"key"`
	WorkItemID string       `json:"work_item_id,omitempty"`
	Type       EventType    `json:"type"`
	Owner      FailureOwner `json:"failure_owner,omitempty"`
	// Verdict/Attempt are used by verdict events.
	Verdict work.Verdict `json:"verdict,omitempty"`
	Attempt int          `json:"attempt,omitempty"`
	// Incident names a hard-gate incident.
	Incident string `json:"incident,omitempty"`
	// Merge accounting.
	CostTokens   int64 `json:"cost_tokens,omitempty"`
	CycleSeconds int64 `json:"cycle_seconds,omitempty"`
	// Override carries the action and, for pin, the forced tier.
	Override string `json:"override,omitempty"`
	Tier     Tier   `json:"tier,omitempty"`
	// Delta is filled by the engine: score after minus score before.
	Delta     int    `json:"delta"`
	Actor     string `json:"actor,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// Score is the current state of one key.
type Score struct {
	Key           Key   `json:"key"`
	Tier          Tier  `json:"tier"`
	Pinned        bool  `json:"pinned,omitempty"`
	Score         int   `json:"score"`
	Samples       int   `json:"samples"`
	Approvals     int   `json:"approvals"`
	FirstPass     int   `json:"first_pass"`
	RejectStreak  int   `json:"reject_streak"`
	Claims        int   `json:"claims"`
	LeaseExpiries int   `json:"lease_expiries"`
	GoodStreak    int   `json:"good_streak"`
	Penalty       int   `json:"penalty"`
	CostMedian    int64 `json:"cost_median"`
	CycleMedian   int64 `json:"cycle_median"`
	Suspended     bool  `json:"suspended"`
	LastEventAt   int64 `json:"last_event_at"`
	UpdatedAt     int64 `json:"updated_at"`

	costs  []int64
	cycles []int64
}

// ApprovalRate is approvals/samples.
func (s Score) ApprovalRate() float64 {
	if s.Samples == 0 {
		return 0
	}
	return float64(s.Approvals) / float64(s.Samples)
}

// FirstPassRate is first-pass approvals/samples.
func (s Score) FirstPassRate() float64 {
	if s.Samples == 0 {
		return 0
	}
	return float64(s.FirstPass) / float64(s.Samples)
}

// Reliability is 1 - expired/claimed leases.
func (s Score) Reliability() float64 {
	if s.Claims == 0 {
		return 1
	}
	ratio := 1 - float64(s.LeaseExpiries)/float64(s.Claims)
	if ratio < 0 {
		return 0
	}
	return ratio
}

// Config tunes the two resource dimensions. Zero values mean "no target", and
// those dimensions score neutral (1.0) instead of inventing a baseline.
type Config struct {
	// BudgetTokens is the typical token budget for the task class.
	BudgetTokens int64
	// TargetSeconds is the typical claim→done duration.
	TargetSeconds int64
	// Now overrides the clock.
	Now func() int64
}

// Engine holds scores and the event log.
type Engine struct {
	mu     sync.Mutex
	scores map[Key]*Score
	events []Event
	nextID uint64
	cfg    Config
	now    func() int64
}

// New returns an empty engine.
func New(cfg Config) *Engine {
	now := cfg.Now
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Engine{scores: make(map[Key]*Score), cfg: cfg, now: now}
}

// ErrInvalidEvent means the event is missing required fields.
var ErrInvalidEvent = errors.New("invalid scoring event")

// Record applies one event and recomputes the affected score. The returned
// score is a copy; the event carries the delta.
func (e *Engine) Record(event Event) (Score, error) {
	if event.Key.AgentID == "" || event.Key.ModelID == "" {
		return Score{}, fmt.Errorf("%w: agent_id and model_id are required", ErrInvalidEvent)
	}
	if !event.Type.Valid() {
		return Score{}, fmt.Errorf("%w: unknown type %q", ErrInvalidEvent, event.Type)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	before := e.ensureLocked(event.Key)
	beforeScore := before.Score

	e.applyLocked(before, event)

	now := e.now()
	e.evaluateLocked(before, now)

	e.nextID++
	event.ID = fmt.Sprintf("se_%d", e.nextID)
	if event.CreatedAt == 0 {
		event.CreatedAt = now
	}
	event.Delta = before.Score - beforeScore
	before.LastEventAt = now
	before.UpdatedAt = now
	e.events = append(e.events, event)

	return *before, nil
}

// Valid reports whether t is a known event type.
func (t EventType) Valid() bool {
	switch t {
	case EventClaim, EventSubmit, EventVerdict, EventMerge, EventLeaseExpiry, EventIncident, EventOverride, EventReset:
		return true
	}
	return false
}

func (e *Engine) applyLocked(s *Score, event Event) {
	switch event.Type {
	case EventClaim:
		s.Claims++
	case EventVerdict:
		if event.Verdict == work.VerdictApprove {
			s.Samples++
			s.Approvals++
			if event.Attempt <= 1 {
				s.FirstPass++
			}
			s.RejectStreak = 0
			s.GoodStreak++
			return
		}
		// Only model-attributed rejections count against the model.
		if event.Owner != "" && event.Owner != OwnerModel {
			return
		}
		s.Samples++
		s.RejectStreak++
		s.GoodStreak = 0
	case EventLeaseExpiry:
		s.LeaseExpiries++
		s.GoodStreak = 0
	case EventMerge:
		if event.CostTokens > 0 {
			s.costs = appendCapped(s.costs, event.CostTokens)
		}
		if event.CycleSeconds > 0 {
			s.cycles = appendCapped(s.cycles, event.CycleSeconds)
		}
		s.CostMedian = median(s.costs)
		s.CycleMedian = median(s.cycles)
	case EventIncident:
		switch event.Incident {
		case IncidentFakeEvidence, IncidentSecurity, IncidentData:
			s.Suspended = true
			s.Tier = TierSuspended
			s.Penalty += 30
			s.GoodStreak = 0
		default:
			s.Penalty += 5
		}
	case EventOverride:
		e.applyOverrideLocked(s, event)
	case EventReset:
		resetLocked(s)
	}
}

func (e *Engine) applyOverrideLocked(s *Score, event Event) {
	switch event.Override {
	case OverridePin:
		if !event.Tier.Valid() {
			return
		}
		s.Pinned = true
		s.Tier = event.Tier
		if event.Tier != TierSuspended {
			s.Suspended = false
		}
	case OverrideUnpin:
		s.Pinned = false
	case OverrideSuspend:
		s.Pinned = false
		s.Suspended = true
		s.Tier = TierSuspended
	case OverrideReinstate:
		s.Pinned = false
		s.Suspended = false
		s.RejectStreak = 0
		s.Tier = TierProbation
	case OverrideReset:
		s.Pinned = false
		resetLocked(s)
	case OverrideRetire:
		s.Pinned = false
		s.Tier = TierRetired
	}
}

func resetLocked(s *Score) {
	s.Score = 0
	s.Tier = TierProbation
	s.Samples = 0
	s.Approvals = 0
	s.FirstPass = 0
	s.RejectStreak = 0
	s.Claims = 0
	s.LeaseExpiries = 0
	s.GoodStreak = 0
	s.Penalty = 0
	s.CostMedian = 0
	s.CycleMedian = 0
	s.Suspended = false
	s.costs = nil
	s.cycles = nil
}

// evaluateLocked recomputes the weighted score and the tier.
func (e *Engine) evaluateLocked(s *Score, now int64) {
	s.Score = clamp(e.weightedLocked(s) - float64(s.Penalty))

	if s.Pinned {
		return
	}
	if s.Tier == TierRetired {
		return
	}
	if s.Suspended || s.RejectStreak >= 3 {
		s.Suspended = true
		s.Tier = TierSuspended
		return
	}
	if s.RejectStreak == 2 {
		s.Tier = TierProbation
		return
	}
	if s.Tier != TierSuspended && e.qualityGateFails(s) {
		s.Tier = TierProbation
		return
	}
	if s.LeaseExpiryRate() > 0.20 && s.Claims >= 10 {
		s.Tier = TierProbation
		return
	}

	switch s.Tier {
	case TierProbation, "":
		if s.GoodStreak >= 5 && s.Score >= 70 {
			s.Tier = TierActive
		} else {
			s.Tier = TierProbation
		}
	case TierActive:
		if s.GoodStreak >= 10 && s.Score >= 85 && s.Samples >= 20 &&
			s.ApprovalRate() >= 0.90 && s.FirstPassRate() >= 0.75 {
			s.Tier = TierTrusted
		}
	case TierTrusted:
		if s.Score < 85 || s.Samples < 20 || s.ApprovalRate() < 0.90 {
			s.Tier = TierActive
		}
	}
}

func (e *Engine) weightedLocked(s *Score) float64 {
	// No verified delivery, no score. Otherwise the neutral reliability and
	// resource terms would hand an unproven pair 35 points for free.
	if s.Samples == 0 {
		return 0
	}
	approval := wilsonLower(s.Approvals, s.Samples, 1.96)
	firstPass := s.FirstPassRate()
	reliability := s.Reliability()

	cost := 1.0
	if e.cfg.BudgetTokens > 0 && s.CostMedian > 0 {
		cost = 1 - float64(s.CostMedian)/float64(e.cfg.BudgetTokens)
	}
	cycle := 1.0
	if e.cfg.TargetSeconds > 0 && s.CycleMedian > 0 {
		cycle = 1 - float64(s.CycleMedian)/float64(e.cfg.TargetSeconds)
	}
	cost = math.Max(0, math.Min(1, cost))
	cycle = math.Max(0, math.Min(1, cycle))

	// Weights from REFACTOR §8.2: quality dominates speed and cost.
	return 100 * (0.50*approval + 0.15*firstPass + 0.15*reliability + 0.12*cost + 0.08*cycle)
}

// qualityGateFails implements the pre-ranking gate: with enough samples, a pair
// that misses any floor drops to probation regardless of cheap or fast delivery.
func (e *Engine) qualityGateFails(s *Score) bool {
	if s.Samples < 10 {
		return false
	}
	return wilsonLower(s.Approvals, s.Samples, 1.96) < 0.80 ||
		s.FirstPassRate() < 0.60 ||
		s.Reliability() < 0.85
}

// LeaseExpiryRate is expired/claimed leases in [0,1].
func (s Score) LeaseExpiryRate() float64 {
	if s.Claims == 0 {
		return 0
	}
	rate := float64(s.LeaseExpiries) / float64(s.Claims)
	if rate > 1 {
		return 1
	}
	return rate
}

// Get returns a copy of the current score.
func (e *Engine) Get(key Key) (Score, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.scores[key]
	if !ok {
		return Score{}, false
	}
	return *s, true
}

// Scores returns every score, sorted by agent/model/class for stable output.
func (e *Engine) Scores() []Score {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]Score, 0, len(e.scores))
	for _, s := range e.scores {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.AgentID != out[j].Key.AgentID {
			return out[i].Key.AgentID < out[j].Key.AgentID
		}
		if out[i].Key.ModelID != out[j].Key.ModelID {
			return out[i].Key.ModelID < out[j].Key.ModelID
		}
		return out[i].Key.TaskClass < out[j].Key.TaskClass
	})
	return out
}

// Events returns the append-only log, optionally filtered by key.
func (e *Engine) Events(key *Key) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]Event, 0, len(e.events))
	for _, event := range e.events {
		if key != nil && event.Key != *key {
			continue
		}
		out = append(out, event)
	}
	return out
}

// Requirement is what an item demands from a pair.
type Requirement struct {
	Risk         work.Risk `json:"risk"`
	Priority     int       `json:"priority"`
	Sensitive    bool      `json:"sensitive"` // auth, payment, migration, security
	BudgetTokens int64     `json:"budget_tokens"`
	// BudgetLimit is the remaining class budget; 0 means unknown.
	BudgetLimit int64 `json:"budget_limit"`
}

// CanClaim applies the §8.3 access table. The returned reason is safe to log.
func (e *Engine) CanClaim(key Key, req Requirement) (bool, string) {
	score, ok := e.Get(key)
	if !ok {
		// Unknown pairs start at probation, the safest default.
		score = Score{Key: key, Tier: TierProbation}
	}

	switch score.Tier {
	case TierSuspended:
		return false, "tier suspended"
	case TierRetired:
		return false, "tier retired"
	case TierTrusted:
		return true, ""
	case TierActive:
		if req.Sensitive {
			return false, "sensitive item requires trusted tier"
		}
		if req.Risk == work.RiskCritical {
			return false, "critical risk requires trusted tier"
		}
		return true, ""
	}

	// Probation: low risk only, at most half of the class budget, and the item
	// still needs an independent verifier (enforced by the board).
	if req.Risk != work.RiskLow {
		return false, "probation may only claim low-risk items"
	}
	if req.Sensitive {
		return false, "probation may not claim sensitive items"
	}
	if req.Priority >= 10 {
		return false, "probation may not claim high-priority items"
	}
	if req.BudgetLimit > 0 && req.BudgetTokens*2 > req.BudgetLimit {
		return false, "probation may use at most half of the class budget"
	}
	return true, ""
}

func (e *Engine) ensureLocked(key Key) *Score {
	s, ok := e.scores[key]
	if !ok {
		s = &Score{Key: key, Tier: TierProbation}
		e.scores[key] = s
	}
	return s
}

func wilsonLower(successes, total int, z float64) float64 {
	if total <= 0 {
		return 0
	}
	n := float64(total)
	p := float64(successes) / n
	denominator := 1 + z*z/n
	centre := p + z*z/(2*n)
	margin := z * math.Sqrt((p*(1-p)+z*z/(4*n))/n)
	value := (centre - margin) / denominator
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func median(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// appendCapped keeps the last 20 samples, matching the sliding window.
func appendCapped(values []int64, value int64) []int64 {
	values = append(values, value)
	if len(values) > 20 {
		values = values[len(values)-20:]
	}
	return values
}

func clamp(value float64) int {
	switch {
	case value <= 0:
		return 0
	case value >= 100:
		return 100
	default:
		return int(math.Round(value))
	}
}
