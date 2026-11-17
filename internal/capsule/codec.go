package capsule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound means no capsule is stored under that id.
var ErrNotFound = errors.New("capsule not found")

// Repository persists capsules. The current version of a capsule is stored
// under its id; Version tracks content changes for staleness checks.
type Repository interface {
	SaveCapsule(ctx context.Context, c *Capsule) error
	GetCapsule(ctx context.Context, id string) (*Capsule, error)
	ListCapsules(ctx context.Context, workItemID string) ([]*Capsule, error)
}

func (c *Capsule) budgetLimit() int {
	budget := c.BudgetTokens
	if budget <= 0 {
		budget = BudgetFor(c.Kind)
	}
	if budget > HardLimit {
		budget = HardLimit
	}
	return budget
}

// Marshal returns the persisted form. The clock is not part of it.
func (c *Capsule) Marshal() ([]byte, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode capsule %q: %w", c.ID, err)
	}
	return data, nil
}

// Unmarshal restores a capsule and rebinds its clock. now may be nil.
func Unmarshal(data []byte, now func() int64) (*Capsule, error) {
	var c Capsule
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode capsule: %w", err)
	}
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	c.now = now
	if c.Version == 0 {
		c.Version = 1
	}
	c.BudgetTokens = c.budgetLimit()
	return &c, nil
}
