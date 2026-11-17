package store

import (
	"context"
	"errors"
	"fmt"

	"janus/internal/work"
)

var _ work.Repository = (*Store)(nil)

func (s *Store) CreateItem(ctx context.Context, item work.WorkItem) error {
	return s.CreateWorkItem(ctx, item)
}

func (s *Store) GetItem(ctx context.Context, id string) (work.WorkItem, error) {
	return s.GetWorkItem(ctx, id)
}

func (s *Store) UpdateItem(ctx context.Context, item work.WorkItem, expectedVersion int64) error {
	err := s.UpdateWorkItem(ctx, item, expectedVersion)
	if errors.Is(err, ErrVersionConflict) {
		return work.ErrVersionConflict
	}
	return err
}

func (s *Store) ListItems(ctx context.Context, status work.Status, workspaceID string) ([]work.WorkItem, error) {
	return s.ListWorkItems(ctx, status, workspaceID)
}

func (s *Store) NextLeaseEpoch(ctx context.Context, itemID string) (int64, error) {
	var max int64
	row := s.DB.WithContext(ctx).
		Model(&WorkClaimModel{}).
		Where("work_item_id = ?", itemID).
		Select("COALESCE(MAX(lease_epoch), 0)").
		Row()
	if err := row.Scan(&max); err != nil {
		return 0, fmt.Errorf("next lease epoch: %w", err)
	}
	return max + 1, nil
}
