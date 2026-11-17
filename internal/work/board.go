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
)

// Board is the single work-board state machine. It reads and writes only
// through a Repository; MemoryRepo is used by tests, store.Store in production.
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
	if strings.TrimSpace(item.ID) == "" {
		return errors.New("work item id required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

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

	if err := b.repo.CreateItem(ctx, item); err != nil {
		return err
	}
	return b.appendEvent(ctx, item.ID, "created", item.CreatedBy)
}

// Get returns a copy of a work item.
func (b *Board) Get(ctx context.Context, id string) (WorkItem, bool) {
	item, err := b.repo.GetItem(ctx, id)
	if err != nil {
		return WorkItem{}, false
	}
	return item, true
}

// ClaimOf returns the current active lease for an item.
func (b *Board) ClaimOf(ctx context.Context, itemID string) (Claim, bool) {
	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
	if err != nil {
		return Claim{}, false
	}
	return claim, ok
}

// Claim takes a lease for role. Worker: open -> running. Verifier: review
// stays review. Integrator: approved -> integrating.
func (b *Board) Claim(ctx context.Context, itemID, agentID string, role Role, plan string, leaseSeconds int64) (Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, err := b.repo.GetItem(ctx, itemID)
	if err != nil {
		return Claim{}, err
	}
	if strings.TrimSpace(agentID) == "" {
		return Claim{}, errors.New("agent id required")
	}
	if !role.Valid() {
		return Claim{}, fmt.Errorf("invalid role %q", role)
	}
	if strings.TrimSpace(plan) == "" {
		return Claim{}, ErrPlanRequired
	}
	if len(plan) > MaxPlanBytes {
		return Claim{}, ErrPlanTooLong
	}

	expired, err := b.expireLocked(ctx, itemID)
	if err != nil {
		return Claim{}, err
	}
	if expired {
		item, err = b.repo.GetItem(ctx, itemID)
		if err != nil {
			return Claim{}, err
		}
	}
	if _, ok, err := b.repo.GetActiveClaim(ctx, itemID); err != nil {
		return Claim{}, err
	} else if ok {
		return Claim{}, ErrAlreadyClaimed
	}
	if err := b.checkClaimableLocked(ctx, &item, role); err != nil {
		return Claim{}, err
	}

	if leaseSeconds <= 0 {
		leaseSeconds = DefaultLeaseSeconds
	}
	if leaseSeconds > MaxLeaseSeconds {
		leaseSeconds = MaxLeaseSeconds
	}

	epoch, err := b.repo.NextLeaseEpoch(ctx, itemID)
	if err != nil {
		return Claim{}, err
	}

	now := b.now()
	claim := Claim{
		ID:             fmt.Sprintf("cl_%s_%d", itemID, epoch),
		WorkItemID:     itemID,
		AgentID:        agentID,
		Role:           role,
		LeaseEpoch:     epoch,
		LeaseExpiresAt: now + leaseSeconds,
		HeartbeatAt:    now,
		Plan:           plan,
		Status:         ClaimActive,
		CreatedAt:      now,
	}

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

	if err := b.repo.SaveClaim(ctx, claim); err != nil {
		return Claim{}, err
	}
	if err := b.repo.UpdateItem(ctx, item, item.Version); err != nil {
		claim.Status = ClaimReleased
		_ = b.repo.SaveClaim(ctx, claim)
		return Claim{}, err
	}
	if err := b.appendEvent(ctx, itemID, "claimed", agentID); err != nil {
		return Claim{}, err
	}
	return claim, nil
}

// Heartbeat extends an active lease. A stale epoch or expired lease is rejected.
func (b *Board) Heartbeat(ctx context.Context, itemID string, epoch int64, extendSeconds int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if expired, err := b.expireLocked(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}
	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
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
	return b.repo.SaveClaim(ctx, claim)
}

// Submit moves a worker's running item to review. It is idempotent for the
// same key while the item is in review or a later state.
func (b *Board) Submit(ctx context.Context, itemID string, epoch int64, idempotencyKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if strings.TrimSpace(idempotencyKey) == "" {
		return ErrIdempotencyRequired
	}
	item, err := b.repo.GetItem(ctx, itemID)
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
	if expired, err := b.expireLocked(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
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

	now := b.now()
	item.Status = StatusReview
	item.LastIdempotencyKey = idempotencyKey
	item.UpdatedAt = now
	claim.Status = ClaimSubmitted
	claim.IdempotencyKey = idempotencyKey

	if err := b.repo.UpdateItem(ctx, item, item.Version); err != nil {
		return err
	}
	if err := b.repo.SaveClaim(ctx, claim); err != nil {
		return err
	}
	return b.appendEvent(ctx, itemID, "submitted", claim.AgentID)
}

// Verdict closes a verifier lease. Approve -> approved. Reject -> open with
// attempts+1, or blocked at the attempt limit. NeedsHuman -> blocked.
func (b *Board) Verdict(ctx context.Context, itemID string, epoch int64, verdict Verdict, reasons string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, err := b.repo.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if expired, err := b.expireLocked(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
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
	if item.Status != StatusReview {
		return ErrInvalidTransition
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

	if err := b.repo.SaveClaim(ctx, claim); err != nil {
		return err
	}
	if err := b.repo.UpdateItem(ctx, item, item.Version); err != nil {
		return err
	}
	return b.appendEvent(ctx, itemID, "verdict:"+string(verdict), claim.AgentID)
}

// Complete closes an integrator lease. It is idempotent for the same key.
func (b *Board) Complete(ctx context.Context, itemID string, epoch int64, idempotencyKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if strings.TrimSpace(idempotencyKey) == "" {
		return ErrIdempotencyRequired
	}
	item, err := b.repo.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if item.Status == StatusDone && item.LastIdempotencyKey == idempotencyKey {
		return nil
	}
	if item.Status == StatusDone {
		return ErrAlreadySubmitted
	}
	if expired, err := b.expireLocked(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
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

	if err := b.repo.UpdateItem(ctx, item, item.Version); err != nil {
		return err
	}
	if err := b.repo.SaveClaim(ctx, claim); err != nil {
		return err
	}
	return b.appendEvent(ctx, itemID, "done", claim.AgentID)
}

// Release returns the item to the state matching the released role.
func (b *Board) Release(ctx context.Context, itemID string, epoch int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, err := b.repo.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if expired, err := b.expireLocked(ctx, itemID); err != nil {
		return err
	} else if expired {
		return ErrLeaseExpired
	}

	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
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

	if err := b.repo.UpdateItem(ctx, item, item.Version); err != nil {
		return err
	}
	if err := b.repo.SaveClaim(ctx, claim); err != nil {
		return err
	}
	return b.appendEvent(ctx, itemID, "released", claim.AgentID)
}

// Expire reclaims an expired lease and reports whether it did so.
func (b *Board) Expire(ctx context.Context, itemID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expireLocked(ctx, itemID)
}

// Claimable returns items that role may claim, ordered by priority then age.
func (b *Board) Claimable(ctx context.Context, role Role) ([]WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	status := statusForRole(role)
	if status == "" {
		return nil, fmt.Errorf("invalid role %q", role)
	}

	items, err := b.repo.ListItems(ctx, status, "")
	if err != nil {
		return nil, err
	}

	out := make([]WorkItem, 0, len(items))
	for _, item := range items {
		expired, err := b.expireLocked(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		if expired {
			item, err = b.repo.GetItem(ctx, item.ID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		if _, ok, err := b.repo.GetActiveClaim(ctx, item.ID); err != nil {
			return nil, err
		} else if ok {
			continue
		}
		if b.checkClaimableLocked(ctx, &item, role) != nil {
			continue
		}
		out = append(out, item)
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

func (b *Board) checkClaimableLocked(ctx context.Context, item *WorkItem, role Role) error {
	switch role {
	case RoleWorker:
		if item.Attempts >= MaxAttempts {
			return ErrAttemptsExhausted
		}
		if item.Status != StatusOpen {
			return ErrInvalidTransition
		}
		for _, dep := range item.DependsOn {
			d, err := b.repo.GetItem(ctx, dep)
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

// expireLocked requeues an active lease that has passed its deadline.
func (b *Board) expireLocked(ctx context.Context, itemID string) (bool, error) {
	claim, ok, err := b.repo.GetActiveClaim(ctx, itemID)
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

	item, err := b.repo.GetItem(ctx, itemID)
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

	if err := b.repo.UpdateItem(ctx, item, item.Version); err != nil {
		return false, err
	}
	if err := b.repo.SaveClaim(ctx, claim); err != nil {
		return false, err
	}
	if err := b.appendEvent(ctx, itemID, "expired", claim.AgentID); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Board) appendEvent(ctx context.Context, itemID, typ, actor string) error {
	return b.repo.AppendEvent(ctx, Event{
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
