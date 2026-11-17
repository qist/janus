package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"janus/internal/capsule"
)

var _ capsule.Repository = (*Store)(nil)

// SaveCapsule upserts the current version of a capsule.
func (s *Store) SaveCapsule(ctx context.Context, c *capsule.Capsule) error {
	if c == nil {
		return errors.New("capsule required")
	}
	body, err := c.Marshal()
	if err != nil {
		return err
	}
	m := WorkCapsuleModel{
		ID:            c.ID,
		WorkItemID:    c.WorkItemID,
		WorkspaceID:   c.WorkspaceID,
		Kind:          string(c.Kind),
		Version:       c.Version,
		BaseCommit:    c.BaseCommit,
		PrefixVersion: c.PrefixVersion,
		Stale:         c.StaleFlag,
		BudgetTokens:  c.Budget(),
		Body:          body,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save capsule %q: %w", c.ID, err)
	}
	return nil
}

// GetCapsule loads one capsule by id.
func (s *Store) GetCapsule(ctx context.Context, id string) (*capsule.Capsule, error) {
	var m WorkCapsuleModel
	err := s.DB.WithContext(ctx).First(&m, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, capsule.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return capsule.Unmarshal(m.Body, nil)
}

// ListCapsules returns every stored version of a work item's capsules,
// newest version first.
func (s *Store) ListCapsules(ctx context.Context, workItemID string) ([]*capsule.Capsule, error) {
	var rows []WorkCapsuleModel
	if err := s.DB.WithContext(ctx).
		Where("work_item_id = ?", workItemID).
		Order("version DESC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]*capsule.Capsule, 0, len(rows))
	for _, m := range rows {
		c, err := capsule.Unmarshal(m.Body, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
