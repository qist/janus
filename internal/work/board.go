package work

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultLeaseSeconds int64 = 20 * 60
	MaxLeaseSeconds     int64 = 2 * 60 * 60
	MaxAttempts               = 3
	MaxPlanBytes              = 512
)

var (
	ErrNotFound            = errors.New("work item not found")
	ErrAlreadyExists       = errors.New("work item already exists")
	ErrInvalidTransition   = errors.New("invalid work item transition")
	ErrAlreadyClaimed      = errors.New("work item already claimed")
	ErrLeaseExpired        = errors.New("lease expired")
	ErrStaleEpoch          = errors.New("stale lease epoch")
	ErrAttemptsExhausted   = errors.New("attempts exhausted")
	ErrPlanRequired        = errors.New("claim plan required")
	ErrPlanTooLong         = errors.New("claim plan too long")
	ErrIdempotencyRequired = errors.New("idempotency key required")
	ErrAlreadySubmitted    = errors.New("work item already submitted")
	ErrDependencyNotDone   = errors.New("dependency not done")
	ErrVersionConflict     = errors.New("work item version conflict")
	ErrEvidenceMissing     = errors.New("submit evidence missing")
	ErrCheckNotRun         = errors.New("contract check missing from evidence")
	ErrCheckFailed         = errors.New("contract check failed")
	ErrSelfReview          = errors.New("verifier must differ from the submitter")
	ErrVerifyRequired      = errors.New("verifier must re-run contract checks before approve")
	ErrPathLocked          = errors.New("path scope locked by another active work item")
	ErrNotHolder           = errors.New("agent does not hold the lease")
)

// Board is the single work-board state machine. It reads and writes only
// through a Repository; MemoryRepo is used by tests, store.Store in production.
//
// Every mutating operation runs inside Repository.Transact, so the item row,
// the claim row and the event row commit together or not at all.
type Board struct {
	mu   sync.Mutex
	repo Repository
	now  func() int64
}

// NewBoard creates a board. A nil repository falls back to a memory repo.
func NewBoard(repo Repository, now func() int64) *Board {
	if repo == nil {
		repo = NewMemoryRepo()
	}
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Board{repo: repo, now: now}
}

// Add validates and inserts a work item.
func (b *Board) Add(ctx context.Context, item WorkItem) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		return b.add(ctx, repo, item)
	})
}

func (b *Board) add(ctx context.Context, repo Repository, item WorkItem) error {
	if strings.TrimSpace(item.ID) == "" {
		return errors.New("work item id required")
	}
	if item.Status == "" {
		item.Status = StatusOpen
	}
	if !item.Status.Valid() {
		return fmt.Errorf("invalid work item status %q", item.Status)
	}
	if item.Contract.Risk == "" {
		item.Contract.Risk = RiskMedium
	}
	if !item.Contract.Risk.Valid() {
		return fmt.Errorf("invalid risk %q", item.Contract.Risk)
	}
	if item.Contract.Autonomy == "" {
		item.Contract.Autonomy = AutonomyL0
	}
	if !item.Contract.Autonomy.Valid() {
		return fmt.Errorf("invalid autonomy %q", item.Contract.Autonomy)
	}
	if item.Contract.Version == 0 {
		item.Contract.Version = 1
	}
	if item.Replicas == 0 {
		item.Replicas = 1
	}

	now := b.now()
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	item.Version = 1

	if err := repo.CreateItem(ctx, item); err != nil {
		return err
	}
	return b.appendEvent(ctx, repo, item.ID, "created", item.CreatedBy)
}

// Get returns a copy of a work item.
func (b *Board) Get(ctx context.Context, id string) (WorkItem, bool) {
	item, err := b.repo.GetItem(ctx, id)
	if err != nil {
		return WorkItem{}, false
	}
	return item, true
}

// List returns items filtered by status and workspace. Empty filters match
// everything. It is a read-only view; expired leases are not reclaimed here.
func (b *Board) List(ctx context.Context, status Status, workspaceID string) ([]WorkItem, error) {
	if status != "" && !status.Valid() {
		return nil, fmt.Errorf("invalid work item status %q", status)
	}
	return b.repo.ListItems(ctx, status, workspaceID)
}

// ClaimOf returns the current active lease for an item.
func (b *Board) ClaimOf(ctx context.Context, itemID string) (Claim, bool) {
	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
	if err != nil {
		return Claim{}, false
	}
	return claim, ok
}

// Claims returns the lease history for an item, oldest epoch first.
func (b *Board) Claims(ctx context.Context, itemID string) ([]Claim, error) {
	return b.repo.ListClaims(ctx, itemID)
}

// Reviews returns the verifier history for an item, oldest first.
func (b *Board) Reviews(ctx context.Context, itemID string) ([]Review, error) {
	return b.repo.ListReviews(ctx, itemID)
}

// Claim takes a lease for role. Worker: open -> running. Verifier: review
// stays review. Integrator: approved -> integrating.
func (b *Board) Claim(ctx context.Context, itemID, agentID string, role Role, plan string, leaseSeconds int64) (Claim, error) {
	return b.ClaimWith(ctx, itemID, Claimant{AgentID: agentID}, role, plan, leaseSeconds)
}

// ClaimWith takes a lease and records the model identity that will be scored.
func (b *Board) ClaimWith(ctx context.Context, itemID string, who Claimant, role Role, plan string, leaseSeconds int64) (Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Expiry commits on its own so a failed claim still requeues the item.
	if _, err := b.expireOutside(ctx, itemID); err != nil {
		return Claim{}, err
	}

	var claim Claim
	err := b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		c, err := b.claim(ctx, repo, itemID, who, role, plan, leaseSeconds)
		if err != nil {
			return err
		}
		claim = c
		return nil
	})
	if err != nil {
		return Claim{}, err
	}
	return claim, nil
}

// Assign is forced dispatch: an admin hands the item to a specific agent,
// bypassing attempts, dependencies and path locks. A live lease and terminal
// states are still refused, and the event log records who forced it.
func (b *Board) Assign(ctx context.Context, itemID string, target Claimant, role Role, plan string, leaseSeconds int64, actor string) (Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if strings.TrimSpace(actor) == "" {
		return Claim{}, errors.New("assigning actor required")
	}
	if _, err := b.expireOutside(ctx, itemID); err != nil {
		return Claim{}, err
	}

	var claim Claim
	err := b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		item, err := repo.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if err := validateClaimInput(target, role, plan); err != nil {
			return err
		}
		if _, ok, err := repo.GetActiveClaim(ctx, itemID); err != nil {
			return err
		} else if ok {
			return ErrAlreadyClaimed
		}

		switch role {
		case RoleWorker:
			if item.Status != StatusOpen && item.Status != StatusBlocked {
				return ErrInvalidTransition
			}
			item.Status = StatusRunning
		case RoleVerifier:
			if item.Status != StatusReview {
				return ErrInvalidTransition
			}
		case RoleIntegrator:
			if item.Status != StatusApproved {
				return ErrInvalidTransition
			}
			item.Status = StatusIntegrating
		}

		epoch, err := repo.NextLeaseEpoch(ctx, itemID)
		if err != nil {
			return err
		}
		now := b.now()
		claim = newClaim(itemID, target, role, plan, epoch, now, clampLease(leaseSeconds))
		claim.Forced = true
		item.LastIdempotencyKey = ""
		item.UpdatedAt = now

		if err := repo.SaveClaim(ctx, claim); err != nil {
			return err
		}
		if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
			return err
		}
		return b.appendEvent(ctx, repo, itemID, "assigned", actor)
	})
	if err != nil {
		return Claim{}, err
	}
	return claim, nil
}

func validateClaimInput(who Claimant, role Role, plan string) error {
	if strings.TrimSpace(who.AgentID) == "" {
		return errors.New("agent id required")
	}
	if !role.Valid() {
		return fmt.Errorf("invalid role %q", role)
	}
	if strings.TrimSpace(plan) == "" {
		return ErrPlanRequired
	}
	if len(plan) > MaxPlanBytes {
		return ErrPlanTooLong
	}
	return nil
}

func clampLease(seconds int64) int64 {
	if seconds <= 0 {
		return DefaultLeaseSeconds
	}
	if seconds > MaxLeaseSeconds {
		return MaxLeaseSeconds
	}
	return seconds
}

func newClaim(itemID string, who Claimant, role Role, plan string, epoch, now, leaseSeconds int64) Claim {
	return Claim{
		ID:             fmt.Sprintf("cl_%s_%d", itemID, epoch),
		WorkItemID:     itemID,
		AgentID:        who.AgentID,
		ModelID:        who.ModelID,
		ModelVersion:   who.ModelVersion,
		ToolMode:       who.ToolMode,
		TaskClass:      who.TaskClass,
		Role:           role,
		LeaseEpoch:     epoch,
		LeaseExpiresAt: now + leaseSeconds,
		HeartbeatAt:    now,
		Plan:           plan,
		Status:         ClaimActive,
		CreatedAt:      now,
	}
}

func (b *Board) claim(ctx context.Context, repo Repository, itemID string, who Claimant, role Role, plan string, leaseSeconds int64) (Claim, error) {
	item, err := repo.GetItem(ctx, itemID)
	if err != nil {
		return Claim{}, err
	}
	if err := validateClaimInput(who, role, plan); err != nil {
		return Claim{}, err
	}

	if _, ok, err := repo.GetActiveClaim(ctx, itemID); err != nil {
		return Claim{}, err
	} else if ok {
		return Claim{}, ErrAlreadyClaimed
	}
	if err := b.checkClaimable(ctx, repo, &item, role); err != nil {
		return Claim{}, err
	}
	if role != RoleVerifier {
		if err := b.checkPathLocks(ctx, repo, &item); err != nil {
			return Claim{}, err
		}
	}

	leaseSeconds = clampLease(leaseSeconds)

	epoch, err := repo.NextLeaseEpoch(ctx, itemID)
	if err != nil {
		return Claim{}, err
	}

	now := b.now()
	claim := newClaim(itemID, who, role, plan, epoch, now, leaseSeconds)

	switch role {
	case RoleWorker:
		item.Status = StatusRunning
	case RoleVerifier:
		// review remains review until verdict
	case RoleIntegrator:
		item.Status = StatusIntegrating
	}
	item.LastIdempotencyKey = ""
	item.UpdatedAt = now

	if err := repo.SaveClaim(ctx, claim); err != nil {
		return Claim{}, err
	}
	if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
		return Claim{}, err
	}
	if err := b.appendEvent(ctx, repo, itemID, "claimed", who.AgentID); err != nil {
		return Claim{}, err
	}
	return claim, nil
}

// Heartbeat extends an active lease. A stale epoch or expired lease is rejected.
func (b *Board) Heartbeat(ctx context.Context, itemID string, epoch int64, extendSeconds int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireOutside(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	return b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		claim, ok, err := repo.GetActiveClaim(ctx, itemID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseExpired
		}
		if claim.LeaseEpoch != epoch {
			return ErrStaleEpoch
		}
		if extendSeconds <= 0 {
			extendSeconds = DefaultLeaseSeconds
		}
		if extendSeconds > MaxLeaseSeconds {
			extendSeconds = MaxLeaseSeconds
		}

		now := b.now()
		claim.HeartbeatAt = now
		claim.LeaseExpiresAt = now + extendSeconds
		return repo.SaveClaim(ctx, claim)
	})
}

// Submit moves a worker's running item to review. It is idempotent for the
// same key while the item is in review or a later state.
//
// Evidence is mandatory: at least one artifact or diff hash, plus the real
// result of every check declared in the contract. Without it the item never
// reaches a verifier.
func (b *Board) Submit(ctx context.Context, itemID string, epoch int64, idempotencyKey string, evidence Evidence) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireOutside(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	return b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		if strings.TrimSpace(idempotencyKey) == "" {
			return ErrIdempotencyRequired
		}
		item, err := repo.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.LastIdempotencyKey == idempotencyKey {
			switch item.Status {
			case StatusReview, StatusApproved, StatusIntegrating, StatusDone:
				return nil
			}
		}
		switch item.Status {
		case StatusReview, StatusApproved, StatusIntegrating, StatusDone:
			return ErrAlreadySubmitted
		}

		claim, ok, err := repo.GetActiveClaim(ctx, itemID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseExpired
		}
		if claim.Role != RoleWorker {
			return ErrInvalidTransition
		}
		if claim.LeaseEpoch != epoch {
			return ErrStaleEpoch
		}
		if item.Status != StatusRunning {
			return ErrInvalidTransition
		}
		if err := validateEvidence(item.Contract, evidence); err != nil {
			return err
		}

		now := b.now()
		item.Status = StatusReview
		item.SubmittedBy = claim.AgentID
		item.LastIdempotencyKey = idempotencyKey
		item.UpdatedAt = now
		claim.Status = ClaimSubmitted
		claim.IdempotencyKey = idempotencyKey
		claim.Evidence = &evidence

		if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
			return err
		}
		if err := repo.SaveClaim(ctx, claim); err != nil {
			return err
		}
		return b.appendEvent(ctx, repo, itemID, "submitted", claim.AgentID)
	})
}

// CheckRunner executes contract checks in a clean workspace. verify.Runner
// implements it; the Board never shells out itself.
type CheckRunner interface {
	Run(ctx context.Context, checks []Check) ([]CheckResult, error)
}

// Verify runs the contract checks for a verifier lease without changing state.
// The returned results are what the verifier must hand back to
// VerdictWithChecks when approving.
//
// The board lock is not held while the commands run: a check suite can take
// minutes and must not block other claims.
func (b *Board) Verify(ctx context.Context, itemID string, epoch int64, runner CheckRunner) ([]CheckResult, error) {
	if runner == nil {
		return nil, errors.New("check runner required")
	}

	item, err := b.verifierLease(ctx, itemID, epoch)
	if err != nil {
		return nil, err
	}
	results, err := runner.Run(ctx, item.Contract.Checks)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// verifierLease validates that agent-independent review is allowed right now.
func (b *Board) verifierLease(ctx context.Context, itemID string, epoch int64) (WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireOutside(ctx, itemID); err != nil {
		return WorkItem{}, err
	} else if expired {
		return WorkItem{}, ErrLeaseExpired
	}

	item, err := b.repo.GetItem(ctx, itemID)
	if err != nil {
		return WorkItem{}, err
	}
	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
	if err != nil {
		return WorkItem{}, err
	}
	if !ok {
		return WorkItem{}, ErrLeaseExpired
	}
	if claim.Role != RoleVerifier {
		return WorkItem{}, ErrInvalidTransition
	}
	if claim.LeaseEpoch != epoch {
		return WorkItem{}, ErrStaleEpoch
	}
	if item.SelfReview(claim.AgentID) {
		return WorkItem{}, ErrSelfReview
	}
	if item.Status != StatusReview {
		return WorkItem{}, ErrInvalidTransition
	}
	return item, nil
}

// Verdict closes a verifier lease. Approve -> approved. Reject -> open with
// attempts+1, or blocked at the attempt limit. NeedsHuman -> blocked.
//
// Approving an item whose contract declares checks requires the verifier's own
// re-run results; see VerdictWithChecks.
func (b *Board) Verdict(ctx context.Context, itemID string, epoch int64, verdict Verdict, reasons string) error {
	return b.VerdictWithChecks(ctx, itemID, epoch, verdict, reasons, nil)
}

// VerdictWithChecks records the verdict plus the check results the verifier
// observed. For approve, every contract check must be present and pass, so a
// paper review cannot approve a code item.
func (b *Board) VerdictWithChecks(ctx context.Context, itemID string, epoch int64, verdict Verdict, reasons string, checks []CheckResult) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireOutside(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	return b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		item, err := repo.GetItem(ctx, itemID)
		if err != nil {
			return err
		}

		claim, ok, err := repo.GetActiveClaim(ctx, itemID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseExpired
		}
		if claim.Role != RoleVerifier {
			return ErrInvalidTransition
		}
		if claim.LeaseEpoch != epoch {
			return ErrStaleEpoch
		}
		if item.SelfReview(claim.AgentID) {
			return ErrSelfReview
		}
		if item.Status != StatusReview {
			return ErrInvalidTransition
		}
		if verdict == VerdictApprove && len(item.Contract.Checks) > 0 {
			if len(checks) == 0 {
				return ErrVerifyRequired
			}
			if err := validateCheckResults(item.Contract, checks); err != nil {
				return err
			}
		}

		now := b.now()
		switch verdict {
		case VerdictApprove:
			item.Status = StatusApproved
			item.LastIdempotencyKey = ""
			claim.Status = ClaimApproved
		case VerdictReject:
			item.Attempts++
			if item.Attempts >= MaxAttempts {
				item.Status = StatusBlocked
			} else {
				item.Status = StatusOpen
			}
			item.LastIdempotencyKey = ""
			claim.Status = ClaimRejected
		case VerdictNeedsHuman:
			item.Status = StatusBlocked
			item.LastIdempotencyKey = ""
			claim.Status = ClaimNeedsHuman
		default:
			return fmt.Errorf("invalid verdict %q", verdict)
		}
		item.UpdatedAt = now
		claim.Note = reasons

		if err := repo.CreateReview(ctx, Review{
			ID:         fmt.Sprintf("rv_%s_%d", itemID, epoch),
			WorkItemID: itemID,
			ReviewerID: claim.AgentID,
			Verdict:    verdict,
			Reasons:    reasons,
			Checks:     checks,
			CreatedAt:  now,
		}); err != nil {
			return err
		}
		if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
			return err
		}
		if err := repo.SaveClaim(ctx, claim); err != nil {
			return err
		}
		return b.appendEvent(ctx, repo, itemID, "verdict:"+string(verdict), claim.AgentID)
	})
}

// Complete closes an integrator lease. It is idempotent for the same key.
func (b *Board) Complete(ctx context.Context, itemID string, epoch int64, idempotencyKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireOutside(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	return b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		if strings.TrimSpace(idempotencyKey) == "" {
			return ErrIdempotencyRequired
		}
		item, err := repo.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status == StatusDone && item.LastIdempotencyKey == idempotencyKey {
			return nil
		}
		if item.Status == StatusDone {
			return ErrAlreadySubmitted
		}

		claim, ok, err := repo.GetActiveClaim(ctx, itemID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseExpired
		}
		if claim.Role != RoleIntegrator {
			return ErrInvalidTransition
		}
		if claim.LeaseEpoch != epoch {
			return ErrStaleEpoch
		}
		if item.Status != StatusIntegrating {
			return ErrInvalidTransition
		}

		now := b.now()
		item.Status = StatusDone
		item.LastIdempotencyKey = idempotencyKey
		item.UpdatedAt = now
		claim.Status = ClaimDone
		claim.IdempotencyKey = idempotencyKey

		if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
			return err
		}
		if err := repo.SaveClaim(ctx, claim); err != nil {
			return err
		}
		return b.appendEvent(ctx, repo, itemID, "done", claim.AgentID)
	})
}

// Release returns the item to the state matching the released role.
func (b *Board) Release(ctx context.Context, itemID string, epoch int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireOutside(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	return b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		item, err := repo.GetItem(ctx, itemID)
		if err != nil {
			return err
		}

		claim, ok, err := repo.GetActiveClaim(ctx, itemID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseExpired
		}
		if claim.LeaseEpoch != epoch {
			return ErrStaleEpoch
		}

		now := b.now()
		switch claim.Role {
		case RoleWorker:
			if item.Status == StatusRunning {
				item.Status = StatusOpen
			}
		case RoleVerifier:
			// review remains review
		case RoleIntegrator:
			if item.Status == StatusIntegrating {
				item.Status = StatusApproved
			}
		}
		item.LastIdempotencyKey = ""
		item.UpdatedAt = now
		claim.Status = ClaimReleased

		if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
			return err
		}
		if err := repo.SaveClaim(ctx, claim); err != nil {
			return err
		}
		return b.appendEvent(ctx, repo, itemID, "released", claim.AgentID)
	})
}

// Expire reclaims an expired lease and reports whether it did so.
func (b *Board) Expire(ctx context.Context, itemID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expireOutside(ctx, itemID)
}

// expireOutside runs the expiry in its own transaction. Mutating operations
// call it before their own transaction so that a rejected call (stale epoch,
// lease already gone, ...) still commits the requeue instead of rolling it
// back together with the failure.
func (b *Board) expireOutside(ctx context.Context, itemID string) (bool, error) {
	var expired bool
	err := b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		var err error
		expired, err = b.expire(ctx, repo, itemID)
		return err
	})
	return expired, err
}

// Claimable returns items that role may claim, ordered by priority then age.
func (b *Board) Claimable(ctx context.Context, role Role) ([]WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	status := statusForRole(role)
	if status == "" {
		return nil, fmt.Errorf("invalid role %q", role)
	}

	var out []WorkItem
	err := b.repo.Transact(ctx, func(ctx context.Context, repo Repository) error {
		items, err := repo.ListItems(ctx, status, "")
		if err != nil {
			return err
		}

		out = make([]WorkItem, 0, len(items))
		for _, item := range items {
			expired, err := b.expire(ctx, repo, item.ID)
			if err != nil {
				return err
			}
			if expired {
				item, err = repo.GetItem(ctx, item.ID)
				if errors.Is(err, ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
			}
			if _, ok, err := repo.GetActiveClaim(ctx, item.ID); err != nil {
				return err
			} else if ok {
				continue
			}
			if b.checkClaimable(ctx, repo, &item, role) != nil {
				continue
			}
			if role != RoleVerifier && b.checkPathLocks(ctx, repo, &item) != nil {
				continue
			}
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (b *Board) checkClaimable(ctx context.Context, repo Repository, item *WorkItem, role Role) error {
	switch role {
	case RoleWorker:
		if item.Attempts >= MaxAttempts {
			return ErrAttemptsExhausted
		}
		if item.Status != StatusOpen {
			return ErrInvalidTransition
		}
		for _, dep := range item.DependsOn {
			d, err := repo.GetItem(ctx, dep)
			if err != nil || d.Status != StatusDone {
				return ErrDependencyNotDone
			}
		}
	case RoleVerifier:
		if item.Status != StatusReview {
			return ErrInvalidTransition
		}
	case RoleIntegrator:
		if item.Status != StatusApproved {
			return ErrInvalidTransition
		}
	default:
		return fmt.Errorf("invalid role %q", role)
	}
	return nil
}

// checkPathLocks rejects a claim when another active item in the same
// workspace declares an overlapping write scope. Two agents editing the same
// files must be split into dependent items instead.
func (b *Board) checkPathLocks(ctx context.Context, repo Repository, item *WorkItem) error {
	if len(item.Contract.Paths) == 0 {
		return nil
	}
	for _, status := range []Status{StatusRunning, StatusIntegrating} {
		others, err := repo.ListItems(ctx, status, item.WorkspaceID)
		if err != nil {
			return err
		}
		for _, other := range others {
			if other.ID == item.ID {
				continue
			}
			if PathsOverlap(item.Contract.Paths, other.Contract.Paths) {
				return fmt.Errorf("%w: %s", ErrPathLocked, other.ID)
			}
		}
	}
	return nil
}

// expire requeues an active lease that has passed its deadline.
func (b *Board) expire(ctx context.Context, repo Repository, itemID string) (bool, error) {
	claim, ok, err := repo.GetActiveClaim(ctx, itemID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	now := b.now()
	if claim.LeaseExpiresAt > now {
		return false, nil
	}

	item, err := repo.GetItem(ctx, itemID)
	if err != nil {
		return false, err
	}
	switch claim.Role {
	case RoleWorker:
		if item.Status == StatusRunning {
			item.Status = StatusOpen
		}
	case RoleVerifier:
		// review remains review
	case RoleIntegrator:
		if item.Status == StatusIntegrating {
			item.Status = StatusApproved
		}
	}
	item.LeaseExpiries++
	item.LastIdempotencyKey = ""
	item.UpdatedAt = now
	claim.Status = ClaimExpired

	if err := repo.UpdateItem(ctx, item, item.Version); err != nil {
		return false, err
	}
	if err := repo.SaveClaim(ctx, claim); err != nil {
		return false, err
	}
	if err := b.appendEvent(ctx, repo, itemID, "expired", claim.AgentID); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Board) appendEvent(ctx context.Context, repo Repository, itemID, typ, actor string) error {
	return repo.AppendEvent(ctx, Event{
		WorkItemID: itemID,
		Type:       typ,
		Actor:      actor,
		CreatedAt:  b.now(),
	})
}

func statusForRole(role Role) Status {
	switch role {
	case RoleWorker:
		return StatusOpen
	case RoleVerifier:
		return StatusReview
	case RoleIntegrator:
		return StatusApproved
	default:
		return ""
	}
}

func cloneItem(in *WorkItem) WorkItem {
	out := *in
	out.Capabilities = append([]string(nil), in.Capabilities...)
	out.RequiredModes = append([]string(nil), in.RequiredModes...)
	out.DependsOn = append([]string(nil), in.DependsOn...)
	out.Contract.Acceptance = append([]string(nil), in.Contract.Acceptance...)
	out.Contract.Checks = append([]Check(nil), in.Contract.Checks...)
	out.Contract.Paths = append([]string(nil), in.Contract.Paths...)
	return out
}
