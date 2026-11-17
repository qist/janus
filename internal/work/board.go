package work

import (
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
)

// Board is an in-memory work board. Persistence and HTTP are separate layers;
// this type owns the state machine and lease/fencing semantics.
type Board struct {
	mu         sync.Mutex
	items      map[string]*WorkItem
	active     map[string]*Claim
	lastEpoch  map[string]int64
	lastSubmit map[string]string
	now        func() int64
}

// NewBoard creates an empty board. now may be nil; Unix seconds are used then.
func NewBoard(now func() int64) *Board {
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Board{
		items:      make(map[string]*WorkItem),
		active:     make(map[string]*Claim),
		lastEpoch:  make(map[string]int64),
		lastSubmit: make(map[string]string),
		now:        now,
	}
}

// Add inserts a work item. The board owns Status/Version timestamps.
func (b *Board) Add(item WorkItem) error {
	if strings.TrimSpace(item.ID) == "" {
		return errors.New("work item id required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.items[item.ID]; ok {
		return ErrAlreadyExists
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
	b.items[item.ID] = &item
	return nil
}

// Get returns a copy of a work item.
func (b *Board) Get(id string) (WorkItem, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, ok := b.items[id]
	if !ok {
		return WorkItem{}, false
	}
	return cloneItem(item), true
}

// ClaimOf returns the current active lease for an item.
func (b *Board) ClaimOf(itemID string) (Claim, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	claim, ok := b.active[itemID]
	if !ok {
		return Claim{}, false
	}
	return *claim, true
}

// Claim takes a lease for role. Worker: open -> running. Verifier: review
// stays review. Integrator: approved -> integrating.
func (b *Board) Claim(itemID, agentID string, role Role, plan string, leaseSeconds int64) (Claim, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, ok := b.items[itemID]
	if !ok {
		return Claim{}, ErrNotFound
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

	b.expireLocked(itemID)

	if _, ok := b.active[itemID]; ok {
		return Claim{}, ErrAlreadyClaimed
	}
	if err := b.checkClaimableLocked(item, role); err != nil {
		return Claim{}, err
	}

	if leaseSeconds <= 0 {
		leaseSeconds = DefaultLeaseSeconds
	}
	if leaseSeconds > MaxLeaseSeconds {
		leaseSeconds = MaxLeaseSeconds
	}

	now := b.now()
	epoch := b.lastEpoch[itemID] + 1
	b.lastEpoch[itemID] = epoch

	claim := &Claim{
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

	item.UpdatedAt = now
	item.Version++
	b.active[itemID] = claim
	return *claim, nil
}

// Heartbeat extends an active lease. A stale epoch or expired lease is rejected.
func (b *Board) Heartbeat(itemID string, epoch int64, extendSeconds int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.expireLocked(itemID) {
		return ErrLeaseExpired
	}
	claim, ok := b.active[itemID]
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

	item := b.items[itemID]
	item.UpdatedAt = now
	item.Version++
	return nil
}

// Submit moves a worker's running item to review. It is idempotent for the
// same key while the item is in review or a later state.
func (b *Board) Submit(itemID string, epoch int64, idempotencyKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if strings.TrimSpace(idempotencyKey) == "" {
		return ErrIdempotencyRequired
	}
	item, ok := b.items[itemID]
	if !ok {
		return ErrNotFound
	}
	if b.lastSubmit[itemID] == idempotencyKey {
		switch item.Status {
		case StatusReview, StatusApproved, StatusIntegrating, StatusDone:
			return nil
		}
	}
	switch item.Status {
	case StatusReview, StatusApproved, StatusIntegrating, StatusDone:
		return ErrAlreadySubmitted
	}
	if b.expireLocked(itemID) {
		return ErrLeaseExpired
	}
	claim, ok := b.active[itemID]
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
		if item.Status == StatusReview || item.Status == StatusApproved || item.Status == StatusIntegrating || item.Status == StatusDone {
			return ErrAlreadySubmitted
		}
		return ErrInvalidTransition
	}

	now := b.now()
	item.Status = StatusReview
	item.UpdatedAt = now
	item.Version++

	claim.Status = ClaimSubmitted
	claim.IdempotencyKey = idempotencyKey
	delete(b.active, itemID)
	b.lastSubmit[itemID] = idempotencyKey
	return nil
}

// Verdict closes a verifier lease. Approve -> approved. Reject -> open with
// attempts+1, or blocked at the attempt limit. NeedsHuman -> blocked.
func (b *Board) Verdict(itemID string, epoch int64, verdict Verdict, reasons string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, ok := b.items[itemID]
	if !ok {
		return ErrNotFound
	}
	if b.expireLocked(itemID) {
		return ErrLeaseExpired
	}
	claim, ok := b.active[itemID]
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
		claim.Status = ClaimApproved
	case VerdictReject:
		item.Attempts++
		if item.Attempts >= MaxAttempts {
			item.Status = StatusBlocked
		} else {
			item.Status = StatusOpen
		}
		claim.Status = ClaimRejected
		delete(b.lastSubmit, itemID)
	case VerdictNeedsHuman:
		item.Status = StatusBlocked
		claim.Status = ClaimNeedsHuman
	default:
		return fmt.Errorf("invalid verdict %q", verdict)
	}

	item.UpdatedAt = now
	item.Version++
	claim.Note = reasons
	delete(b.active, itemID)
	return nil
}

// Complete closes an integrator lease. It is idempotent for the same key.
func (b *Board) Complete(itemID string, epoch int64, idempotencyKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if strings.TrimSpace(idempotencyKey) == "" {
		return ErrIdempotencyRequired
	}
	item, ok := b.items[itemID]
	if !ok {
		return ErrNotFound
	}
	if item.Status == StatusDone && b.lastSubmit[itemID] == idempotencyKey {
		return nil
	}
	if b.expireLocked(itemID) {
		return ErrLeaseExpired
	}
	claim, ok := b.active[itemID]
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
	item.UpdatedAt = now
	item.Version++

	claim.Status = ClaimDone
	claim.IdempotencyKey = idempotencyKey
	delete(b.active, itemID)
	b.lastSubmit[itemID] = idempotencyKey
	return nil
}

// Release returns the item to the state matching the released role.
func (b *Board) Release(itemID string, epoch int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	item, ok := b.items[itemID]
	if !ok {
		return ErrNotFound
	}
	if b.expireLocked(itemID) {
		return ErrLeaseExpired
	}
	claim, ok := b.active[itemID]
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

	claim.Status = ClaimReleased
	item.UpdatedAt = now
	item.Version++
	delete(b.active, itemID)
	delete(b.lastSubmit, itemID)
	return nil
}

// Expire reclaims an expired lease and reports whether it did so.
func (b *Board) Expire(itemID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expireLocked(itemID)
}

// Claimable returns items that role may claim, ordered by priority then age.
func (b *Board) Claimable(role Role) []WorkItem {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []WorkItem
	for id, item := range b.items {
		b.expireLocked(id)
		if _, ok := b.active[id]; ok {
			continue
		}
		if b.checkClaimableLocked(item, role) != nil {
			continue
		}
		out = append(out, cloneItem(item))
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
	return out
}

func (b *Board) checkClaimableLocked(item *WorkItem, role Role) error {
	switch role {
	case RoleWorker:
		if item.Attempts >= MaxAttempts {
			return ErrAttemptsExhausted
		}
		if item.Status != StatusOpen {
			return ErrInvalidTransition
		}
		for _, dep := range item.DependsOn {
			d, ok := b.items[dep]
			if !ok || d.Status != StatusDone {
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
func (b *Board) expireLocked(itemID string) bool {
	claim, ok := b.active[itemID]
	if !ok {
		return false
	}
	now := b.now()
	if claim.LeaseExpiresAt > now {
		return false
	}

	item := b.items[itemID]
	claim.Status = ClaimExpired
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
	item.UpdatedAt = now
	item.Version++

	delete(b.active, itemID)
	delete(b.lastSubmit, itemID)
	return true
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
