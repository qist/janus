package work

import "context"

// Repository is the persistence boundary for the work board. The Board is the
// only state machine; repositories only load and store rows.
type Repository interface {
	CreateItem(ctx context.Context, item WorkItem) error
	GetItem(ctx context.Context, id string) (WorkItem, error)
	UpdateItem(ctx context.Context, item WorkItem, expectedVersion int64) error
	ListItems(ctx context.Context, status Status, workspaceID string) ([]WorkItem, error)

	SaveClaim(ctx context.Context, claim Claim) error
	GetActiveClaim(ctx context.Context, itemID string) (Claim, bool, error)
	NextLeaseEpoch(ctx context.Context, itemID string) (int64, error)
	ListClaims(ctx context.Context, itemID string) ([]Claim, error)

	AppendEvent(ctx context.Context, event Event) error
}
