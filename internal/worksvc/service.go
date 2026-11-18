// Package worksvc composes the work board, capsule store and context pack into
// the operations exposed to agents. Keeping this wiring here lets the work
// package stay free of capsule/contextpack dependencies.
package worksvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"janus/internal/authz"
	"janus/internal/capsule"
	"janus/internal/contextpack"
	"janus/internal/policy"
	"janus/internal/scoring"
	"janus/internal/work"
)

// Service is the agent-facing work facade.
type Service struct {
	Board    *work.Board
	Capsules capsule.Repository
	// Facts returns the stable commands for a workspace; nil means none.
	Facts func(ctx context.Context, workspaceID string) (contextpack.WorkspaceFacts, error)
	// Budget overrides contextpack.DefaultBudget when non-zero.
	Budget int
	// Now overrides the clock used when creating capsules.
	Now func() int64
	// Authz authorizes sensitive actions; nil means the endpoint is disabled.
	Authz *authz.Service
	// Scores gates worker claims by model tier when set.
	Scores *scoring.Engine
}

var (
	// ErrAuthzUnavailable means no authorization service is configured.
	ErrAuthzUnavailable = errors.New("authorization service is not configured")
	// ErrTierDenied means the model's tier may not claim this item.
	ErrTierDenied = errors.New("model tier does not allow this claim")
	// ErrModelRequired means scoring is on but the claim has no model identity.
	ErrModelRequired = errors.New("model_id is required when scoring is enabled")
	// ErrFailureOwnerRequired means a reject was not attributed.
	ErrFailureOwnerRequired = errors.New("failure_owner is required for a reject")
)

// New returns a service with the default clock.
func New(board *work.Board, capsules capsule.Repository) *Service {
	return &Service{Board: board, Capsules: capsules}
}

// ClaimResult is what work_claim returns: the lease plus a bounded handover.
type ClaimResult struct {
	Claim    work.Claim       `json:"claim"`
	Pack     contextpack.Pack `json:"pack"`
	Capsule  *capsule.Capsule `json:"capsule,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
}

// ItemView is what work_get returns.
type ItemView struct {
	Item     work.WorkItem    `json:"item"`
	Claim    *work.Claim      `json:"claim,omitempty"`
	Capsule  *capsule.Capsule `json:"capsule,omitempty"`
	Reviews  []work.Review    `json:"reviews"`
	Warnings []string         `json:"warnings,omitempty"`
}

// Claim takes a lease and builds the handover pack for the new holder. Worker
// claims are gated by the model's tier when scoring is enabled.
func (s *Service) Claim(ctx context.Context, itemID string, who work.Claimant, role work.Role, plan string, leaseSeconds int64) (ClaimResult, error) {
	if role == work.RoleWorker && s.Scores != nil {
		item, ok := s.Board.Get(ctx, itemID)
		if !ok {
			return ClaimResult{}, work.ErrNotFound
		}
		if strings.TrimSpace(who.ModelID) == "" {
			return ClaimResult{}, ErrModelRequired
		}
		if allowed, reason := s.Scores.CanClaim(ScoreKey(who, item), ScoringRequirement(item)); !allowed {
			return ClaimResult{}, fmt.Errorf("%w: %s", ErrTierDenied, reason)
		}
	}

	claim, err := s.Board.ClaimWith(ctx, itemID, who, role, plan, leaseSeconds)
	if err != nil {
		return ClaimResult{}, err
	}
	return s.resultFor(ctx, claim)
}

// Assign is forced dispatch by an admin. It bypasses the tier gate on purpose:
// the admin has already decided. The board still refuses a live lease and
// terminal items, and records the forcing actor in the event log.
func (s *Service) Assign(ctx context.Context, itemID, actor string, target work.Claimant, role work.Role, plan string, leaseSeconds int64) (ClaimResult, error) {
	claim, err := s.Board.Assign(ctx, itemID, target, role, plan, leaseSeconds, actor)
	if err != nil {
		return ClaimResult{}, err
	}
	return s.resultFor(ctx, claim)
}

func (s *Service) resultFor(ctx context.Context, claim work.Claim) (ClaimResult, error) {
	item, ok := s.Board.Get(ctx, claim.WorkItemID)
	if !ok {
		return ClaimResult{Claim: claim}, work.ErrNotFound
	}
	caps, warnings := s.capsuleFor(ctx, item)
	pack, packWarnings := s.buildPack(ctx, item, caps)
	return ClaimResult{
		Claim:    claim,
		Pack:     pack,
		Capsule:  caps,
		Warnings: append(warnings, packWarnings...),
	}, nil
}

// List returns board items, optionally filtered by status and workspace.
func (s *Service) List(ctx context.Context, status work.Status, workspaceID string) ([]work.WorkItem, error) {
	return s.Board.List(ctx, status, workspaceID)
}

// Add creates a work item.
func (s *Service) Add(ctx context.Context, item work.WorkItem) error {
	return s.Board.Add(ctx, item)
}

// Heartbeat extends the lease held on itemID. Only the current holder may
// extend it; the caller's identity comes from the transport, not the body.
func (s *Service) Heartbeat(ctx context.Context, itemID, agentID string, epoch, extendSeconds int64) error {
	if err := s.requireHolder(ctx, itemID, agentID, epoch, ""); err != nil {
		return err
	}
	return s.Board.Heartbeat(ctx, itemID, epoch, extendSeconds)
}

// Release gives the item back without completing it.
func (s *Service) Release(ctx context.Context, itemID, agentID string, epoch int64) error {
	if err := s.requireHolder(ctx, itemID, agentID, epoch, ""); err != nil {
		return err
	}
	return s.Board.Release(ctx, itemID, epoch)
}

// Complete closes an integrator lease and marks the item done.
func (s *Service) Complete(ctx context.Context, itemID, agentID string, epoch int64, idempotencyKey string) error {
	if err := s.requireHolder(ctx, itemID, agentID, epoch, work.RoleIntegrator); err != nil {
		return err
	}
	claim, hasClaim := s.workerClaim(ctx, itemID)
	if err := s.Board.Complete(ctx, itemID, epoch, idempotencyKey); err != nil {
		return err
	}
	if s.Scores != nil && hasClaim && claim.ModelID != "" {
		cycle := int64(0)
		now := s.nowUnix()
		if claim.CreatedAt > 0 && claim.CreatedAt < now {
			cycle = now - claim.CreatedAt
		}
		_, _ = s.Scores.Record(scoring.Event{
			Key:          ClaimKey(claim),
			WorkItemID:   itemID,
			Type:         scoring.EventMerge,
			CycleSeconds: cycle,
			Actor:        agentID,
		})
	}
	return nil
}

// ScoreKey builds the scoring key for a claimant on an item.
func ScoreKey(who work.Claimant, item work.WorkItem) scoring.Key {
	taskClass := strings.TrimSpace(who.TaskClass)
	if taskClass == "" {
		taskClass = TaskClassFor(item)
	}
	toolMode := strings.TrimSpace(who.ToolMode)
	if toolMode == "" {
		toolMode = "native"
	}
	version := strings.TrimSpace(who.ModelVersion)
	if version == "" {
		version = "unknown"
	}
	return scoring.Key{
		AgentID:      who.AgentID,
		ModelID:      who.ModelID,
		TaskClass:    taskClass,
		ToolMode:     toolMode,
		ModelVersion: version,
	}
}

// ClaimKey builds the scoring key from a stored lease.
func ClaimKey(claim work.Claim) scoring.Key {
	return ScoreKey(work.Claimant{
		AgentID:      claim.AgentID,
		ModelID:      claim.ModelID,
		ModelVersion: claim.ModelVersion,
		ToolMode:     claim.ToolMode,
		TaskClass:    claim.TaskClass,
	}, work.WorkItem{})
}

// TaskClassFor derives a coarse class from the declared paths.
func TaskClassFor(item work.WorkItem) string {
	for _, path := range item.Contract.Paths {
		lower := strings.ToLower(path)
		switch {
		case strings.HasPrefix(lower, "docs/") || strings.HasSuffix(lower, ".md"):
			return "docs"
		case strings.HasSuffix(lower, "_test.go") || strings.HasPrefix(lower, "tests/"):
			return "tests"
		case strings.Contains(lower, "auth") || strings.Contains(lower, "payment") || strings.Contains(lower, "billing"):
			return "security"
		case strings.HasPrefix(lower, "deploy/") || strings.HasPrefix(lower, ".github/"):
			return "infra"
		}
	}
	return "default"
}

// SensitiveItem reports whether an item touches security-critical code or data.
func SensitiveItem(item work.WorkItem) bool {
	if item.Contract.Risk == work.RiskCritical {
		return true
	}
	for _, path := range item.Contract.Paths {
		lower := strings.ToLower(path)
		for _, marker := range []string{"internal/auth", "/auth/", "payment", "billing", "migrat", "secret", ".github/"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

// ScoringRequirement projects an item onto the scoring access rules.
func ScoringRequirement(item work.WorkItem) scoring.Requirement {
	return scoring.Requirement{
		Risk:         item.Contract.Risk,
		Priority:     item.Priority,
		Sensitive:    SensitiveItem(item),
		BudgetTokens: item.Contract.BudgetTokens,
	}
}

// workerClaim returns the newest worker lease for an item. Scoring attributes
// outcomes to the worker, never to the verifier or integrator holding the
// lease when the verdict or merge happens.
func (s *Service) workerClaim(ctx context.Context, itemID string) (work.Claim, bool) {
	claims, err := s.Board.Claims(ctx, itemID)
	if err != nil {
		return work.Claim{}, false
	}
	var found work.Claim
	for _, claim := range claims {
		if claim.Role != work.RoleWorker {
			continue
		}
		if found.ID == "" || claim.LeaseEpoch >= found.LeaseEpoch {
			found = claim
		}
	}
	return found, found.ID != ""
}

func (s *Service) nowUnix() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().Unix()
}

// Verify runs the contract checks for an existing verifier lease.
func (s *Service) Verify(ctx context.Context, itemID, agentID string, epoch int64, runner work.CheckRunner) ([]work.CheckResult, error) {
	if err := s.requireHolder(ctx, itemID, agentID, epoch, work.RoleVerifier); err != nil {
		return nil, err
	}
	return s.Board.Verify(ctx, itemID, epoch, runner)
}

// requireHolder rejects a caller that does not own the active lease. Role is
// checked when role is non-empty. The board re-checks the epoch under its own
// lock, so a lease change between these two steps fails as a stale epoch
// rather than acting for the wrong agent.
func (s *Service) requireHolder(ctx context.Context, itemID, agentID string, epoch int64, role work.Role) error {
	if strings.TrimSpace(agentID) == "" {
		return work.ErrNotHolder
	}
	claim, ok := s.Board.ClaimOf(ctx, itemID)
	if !ok {
		return work.ErrLeaseExpired
	}
	if claim.LeaseEpoch != epoch {
		return work.ErrStaleEpoch
	}
	if claim.AgentID != agentID {
		return work.ErrNotHolder
	}
	if role != "" && claim.Role != role {
		return work.ErrInvalidTransition
	}
	return nil
}

// Authorize asks policy + auditors whether the lease holder may perform
// action, and returns a single-use capability token when it may. The contract
// comes from the stored item, never from the caller.
func (s *Service) Authorize(ctx context.Context, itemID, agentID string, epoch int64, action policy.Action, reason string, evidence []string, workerModel string) (authz.Outcome, error) {
	if s.Authz == nil {
		return authz.Outcome{}, ErrAuthzUnavailable
	}
	if err := s.requireHolder(ctx, itemID, agentID, epoch, ""); err != nil {
		return authz.Outcome{}, err
	}
	item, ok := s.Board.Get(ctx, itemID)
	if !ok {
		return authz.Outcome{}, work.ErrNotFound
	}
	return s.Authz.Authorize(ctx, authz.Request{
		WorkItemID:  itemID,
		LeaseEpoch:  epoch,
		AgentID:     agentID,
		Action:      action,
		Contract:    item.Contract,
		Reason:      reason,
		Evidence:    evidence,
		WorkerModel: workerModel,
	})
}

// Handoff rebuilds the pack without taking a lease.
func (s *Service) Handoff(ctx context.Context, itemID string) (contextpack.Pack, []string, error) {
	item, ok := s.Board.Get(ctx, itemID)
	if !ok {
		return contextpack.Pack{}, nil, work.ErrNotFound
	}
	caps, warnings := s.capsuleFor(ctx, item)
	pack, packWarnings := s.buildPack(ctx, item, caps)
	return pack, append(warnings, packWarnings...), nil
}

// Get returns the item, its active lease, latest capsule and review history.
func (s *Service) Get(ctx context.Context, itemID string) (ItemView, error) {
	item, ok := s.Board.Get(ctx, itemID)
	if !ok {
		return ItemView{}, work.ErrNotFound
	}

	view := ItemView{Item: item}
	if claim, ok := s.Board.ClaimOf(ctx, itemID); ok {
		view.Claim = &claim
	}
	if reviews, err := s.Board.Reviews(ctx, itemID); err != nil {
		view.Warnings = append(view.Warnings, "reviews unavailable: "+err.Error())
	} else {
		view.Reviews = reviews
	}

	caps, warnings := s.capsuleFor(ctx, item)
	view.Capsule = caps
	view.Warnings = append(view.Warnings, warnings...)
	return view, nil
}

// Submit delegates to the board and records the evidence in the capsule.
// Capsule bookkeeping is best effort: a failed write must not undo a submit.
func (s *Service) Submit(ctx context.Context, itemID, agentID string, epoch int64, idempotencyKey string, evidence work.Evidence) error {
	if err := s.requireHolder(ctx, itemID, agentID, epoch, work.RoleWorker); err != nil {
		return err
	}
	if err := s.Board.Submit(ctx, itemID, epoch, idempotencyKey, evidence); err != nil {
		return err
	}
	s.recordEvidence(ctx, itemID, evidence)
	return nil
}

// Verdict delegates to the board, records the outcome for scoring and, on a
// rejection, stores the failure in the capsule so the next claimer starts from
// it instead of from zero.
func (s *Service) Verdict(ctx context.Context, itemID, agentID string, epoch int64, verdict work.Verdict, reasons string, checks []work.CheckResult, owner scoring.FailureOwner) error {
	if verdict == work.VerdictReject && s.Scores != nil && !owner.Valid() {
		return ErrFailureOwnerRequired
	}
	if err := s.requireHolder(ctx, itemID, agentID, epoch, work.RoleVerifier); err != nil {
		return err
	}

	// Score the worker's lease, not the verifier's: the model that wrote the
	// change is the one being judged.
	claim, hasClaim := s.workerClaim(ctx, itemID)
	item, hasItem := s.Board.Get(ctx, itemID)

	if err := s.Board.VerdictWithChecks(ctx, itemID, epoch, verdict, reasons, checks); err != nil {
		return err
	}
	s.recordVerdictScore(itemID, item, hasItem, claim, hasClaim, agentID, verdict, owner)
	if verdict == work.VerdictApprove {
		return nil
	}
	s.recordFailure(ctx, itemID, reasons, checks)
	return nil
}

func (s *Service) recordVerdictScore(itemID string, item work.WorkItem, hasItem bool, claim work.Claim, hasClaim bool, agentID string, verdict work.Verdict, owner scoring.FailureOwner) {
	if s.Scores == nil || !hasClaim || claim.ModelID == "" {
		return
	}
	attempt := 1
	if hasItem {
		attempt = item.Attempts + 1
	}
	if verdict == work.VerdictNeedsHuman && !owner.Valid() {
		// Lack of clarity is a task-definition problem, not a model failure.
		owner = scoring.OwnerTask
	}
	_, _ = s.Scores.Record(scoring.Event{
		Key:        ClaimKey(claim),
		WorkItemID: itemID,
		Type:       scoring.EventVerdict,
		Verdict:    verdict,
		Attempt:    attempt,
		Owner:      owner,
		Actor:      agentID,
	})
}

func (s *Service) recordEvidence(ctx context.Context, itemID string, evidence work.Evidence) {
	item, ok := s.Board.Get(ctx, itemID)
	if !ok {
		return
	}
	caps, _ := s.capsuleFor(ctx, item)
	if caps == nil {
		return
	}

	before := caps.Version
	for _, ref := range evidence.Artifacts {
		s.addArtifact(ctx, caps, capsule.ArtifactRef{ID: ref, Kind: "artifact"})
	}
	if evidence.DiffHash != "" {
		s.addArtifact(ctx, caps, capsule.ArtifactRef{ID: "diff", Kind: "diff", SHA256: evidence.DiffHash})
	}
	if caps.Version != before {
		s.save(ctx, caps)
	}
}

func (s *Service) addArtifact(ctx context.Context, caps *capsule.Capsule, ref capsule.ArtifactRef) {
	err := caps.AddArtifact(ref)
	if errors.Is(err, capsule.ErrBudgetExceeded) {
		caps.Trim()
		_ = caps.AddArtifact(ref)
	}
}

func (s *Service) recordFailure(ctx context.Context, itemID, reasons string, checks []work.CheckResult) {
	item, ok := s.Board.Get(ctx, itemID)
	if !ok {
		return
	}
	caps, _ := s.capsuleFor(ctx, item)
	if caps == nil {
		return
	}

	summary := strings.TrimSpace(reasons)
	if summary == "" {
		summary = "review rejected"
	}
	reason := ""
	for _, check := range checks {
		if check.ExitCode != 0 {
			reason = fmt.Sprintf("%s exit=%d: %s", check.Name, check.ExitCode, tail(check.OutputTail, 200))
			break
		}
	}

	err := caps.AddFailure(item.Attempts, summary, reason)
	if errors.Is(err, capsule.ErrBudgetExceeded) {
		caps.Trim()
		err = caps.AddFailure(item.Attempts, summary, reason)
	}
	if err == nil {
		s.save(ctx, caps)
	}
}

func (s *Service) save(ctx context.Context, caps *capsule.Capsule) {
	if s.Capsules == nil {
		return
	}
	if err := s.Capsules.SaveCapsule(ctx, caps); err != nil {
		caps.Trim()
		_ = s.Capsules.SaveCapsule(ctx, caps)
	}
}

func (s *Service) buildPack(ctx context.Context, item work.WorkItem, caps *capsule.Capsule) (contextpack.Pack, []string) {
	var warnings []string

	reviews, err := s.Board.Reviews(ctx, item.ID)
	if err != nil {
		warnings = append(warnings, "reviews unavailable: "+err.Error())
	}

	var facts contextpack.WorkspaceFacts
	if s.Facts != nil {
		got, err := s.Facts(ctx, item.WorkspaceID)
		if err != nil {
			warnings = append(warnings, "workspace facts unavailable: "+err.Error())
		} else {
			facts = got
		}
	}

	return contextpack.Build(contextpack.Input{
		Item:    item,
		Capsule: caps,
		Reviews: reviews,
		Facts:   facts,
		Budget:  s.Budget,
	}), warnings
}

// capsuleFor returns the item's capsule, creating an unsaved one when absent.
func (s *Service) capsuleFor(ctx context.Context, item work.WorkItem) (*capsule.Capsule, []string) {
	if s.Capsules == nil {
		return nil, nil
	}

	if id := strings.TrimSpace(item.Contract.CapsuleID); id != "" {
		caps, err := s.Capsules.GetCapsule(ctx, id)
		if err == nil {
			return caps, nil
		}
		if !errors.Is(err, capsule.ErrNotFound) {
			return nil, []string{"capsule unavailable: " + err.Error()}
		}
		return s.newCapsule(item, id), nil
	}

	list, err := s.Capsules.ListCapsules(ctx, item.ID)
	if err != nil {
		return nil, []string{"capsule list failed: " + err.Error()}
	}
	if len(list) > 0 {
		return list[0], nil
	}
	return s.newCapsule(item, "cap_"+item.ID), nil
}

func (s *Service) newCapsule(item work.WorkItem, id string) *capsule.Capsule {
	caps := capsule.New(id, item.ID, KindFor(item), s.clock())
	caps.WorkspaceID = item.WorkspaceID
	caps.PrefixVersion = item.Contract.PrefixVersion
	if goal := strings.TrimSpace(item.Contract.Goal); goal != "" {
		_ = caps.SetGoal(goal)
	}
	return caps
}

func (s *Service) clock() func() int64 {
	if s.Now != nil {
		return s.Now
	}
	return func() int64 { return time.Now().Unix() }
}

// KindFor picks a capsule budget class from the write scope.
func KindFor(item work.WorkItem) capsule.Kind {
	for _, path := range item.Contract.Paths {
		switch {
		case strings.HasPrefix(path, "docs/") || strings.HasSuffix(path, ".md"):
			return capsule.KindDocs
		case path == "tests" || strings.HasPrefix(path, "tests/") || strings.HasSuffix(path, "_test.go"):
			return capsule.KindTests
		case strings.HasPrefix(path, "deploy/") || strings.HasPrefix(path, ".github/"):
			return capsule.KindInfra
		case strings.HasPrefix(path, "internal/auth") || strings.Contains(path, "security"):
			return capsule.KindSecurity
		}
	}
	return capsule.KindDefault
}

func tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}
