package work

import (
	"context"
	"sort"
	"sync"
)

// MemoryRepo is the in-memory Repository used by domain tests and local dev.
type MemoryRepo struct {
	mu          sync.Mutex
	items       map[string]WorkItem
	claims      map[string][]Claim
	active      map[string]string
	epochs      map[string]int64
	events      []Event
	nextEventID uint64
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		items:  make(map[string]WorkItem),
		claims: make(map[string][]Claim),
		active: make(map[string]string),
		epochs: make(map[string]int64),
	}
}

func (r *MemoryRepo) CreateItem(_ context.Context, item WorkItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.items[item.ID]; ok {
		return ErrAlreadyExists
	}
	if item.Version == 0 {
		item.Version = 1
	}
	r.items[item.ID] = cloneItem(&item)
	return nil
}

func (r *MemoryRepo) GetItem(_ context.Context, id string) (WorkItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	item, ok := r.items[id]
	if !ok {
		return WorkItem{}, ErrNotFound
	}
	return cloneItem(&item), nil
}

func (r *MemoryRepo) UpdateItem(_ context.Context, item WorkItem, expectedVersion int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.items[item.ID]
	if !ok {
		return ErrNotFound
	}
	if current.Version != expectedVersion {
		return ErrVersionConflict
	}
	item.Version = expectedVersion + 1
	r.items[item.ID] = cloneItem(&item)
	return nil
}

func (r *MemoryRepo) ListItems(_ context.Context, status Status, workspaceID string) ([]WorkItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]WorkItem, 0, len(r.items))
	for _, item := range r.items {
		if status != "" && item.Status != status {
			continue
		}
		if workspaceID != "" && item.WorkspaceID != workspaceID {
			continue
		}
		out = append(out, cloneItem(&item))
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

func (r *MemoryRepo) SaveClaim(_ context.Context, claim Claim) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	claims := r.claims[claim.WorkItemID]
	replaced := false
	for i := range claims {
		if claims[i].ID == claim.ID {
			claims[i] = claim
			replaced = true
			break
		}
	}
	if !replaced {
		claims = append(claims, claim)
	}
	r.claims[claim.WorkItemID] = claims

	if claim.Status == ClaimActive {
		r.active[claim.WorkItemID] = claim.ID
	} else if r.active[claim.WorkItemID] == claim.ID {
		delete(r.active, claim.WorkItemID)
	}
	return nil
}

func (r *MemoryRepo) GetActiveClaim(_ context.Context, itemID string) (Claim, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := r.active[itemID]
	if id == "" {
		return Claim{}, false, nil
	}
	for _, claim := range r.claims[itemID] {
		if claim.ID == id && claim.Status == ClaimActive {
			return claim, true, nil
		}
	}
	return Claim{}, false, nil
}

func (r *MemoryRepo) NextLeaseEpoch(_ context.Context, itemID string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.epochs[itemID]++
	return r.epochs[itemID], nil
}

func (r *MemoryRepo) ListClaims(_ context.Context, itemID string) ([]Claim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	claims := r.claims[itemID]
	out := make([]Claim, len(claims))
	copy(out, claims)
	sort.Slice(out, func(i, j int) bool { return out[i].LeaseEpoch < out[j].LeaseEpoch })
	return out, nil
}

func (r *MemoryRepo) AppendEvent(_ context.Context, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextEventID++
	event.ID = r.nextEventID
	r.events = append(r.events, event)
	return nil
}
