package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"janus/internal/goals"
	"janus/internal/work"
)

var _ goals.Repository = (*Store)(nil)

// GoalModel is the persisted form of goals.Goal.
type GoalModel struct {
	ID           string `gorm:"primaryKey;size:64"`
	Title        string `gorm:"size:200"`
	Brief        string `gorm:"type:text"`
	WorkspaceID  string `gorm:"index;size:64"`
	GroupID      string `gorm:"index;size:64"`
	BudgetTokens int64
	Risk         string `gorm:"size:16"`
	Status       string `gorm:"index;size:16"`
	ItemIDsJSON  []byte `gorm:"type:blob"`
	CreatedBy    string `gorm:"size:64"`
	Error        string `gorm:"size:1024"`
	CreatedAt    int64  `gorm:"index"`
	UpdatedAt    int64
}

// TableName fixes the table name used by migrations and queries.
func (GoalModel) TableName() string { return "work_goals" }

// SaveGoal upserts a goal.
func (s *Store) SaveGoal(ctx context.Context, goal goals.Goal) error {
	itemIDs, err := json.Marshal(goal.ItemIDs)
	if err != nil {
		return fmt.Errorf("encode goal items: %w", err)
	}
	m := GoalModel{
		ID:           goal.ID,
		Title:        goal.Title,
		Brief:        goal.Brief,
		WorkspaceID:  goal.WorkspaceID,
		GroupID:      goal.GroupID,
		BudgetTokens: goal.BudgetTokens,
		Risk:         string(goal.Risk),
		Status:       string(goal.Status),
		ItemIDsJSON:  itemIDs,
		CreatedBy:    goal.CreatedBy,
		Error:        goal.Error,
		CreatedAt:    goal.CreatedAt,
		UpdatedAt:    goal.UpdatedAt,
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save goal %q: %w", goal.ID, err)
	}
	return nil
}

// GetGoal loads one goal.
func (s *Store) GetGoal(ctx context.Context, id string) (goals.Goal, error) {
	var m GoalModel
	err := s.DB.WithContext(ctx).First(&m, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return goals.Goal{}, goals.ErrNotFound
	}
	if err != nil {
		return goals.Goal{}, err
	}
	return goalFromModel(m)
}

// ListGoals returns goals newest first.
func (s *Store) ListGoals(ctx context.Context, limit int) ([]goals.Goal, error) {
	query := s.DB.WithContext(ctx).Order("created_at DESC, id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}

	var rows []GoalModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]goals.Goal, 0, len(rows))
	for _, m := range rows {
		goal, err := goalFromModel(m)
		if err != nil {
			return nil, err
		}
		out = append(out, goal)
	}
	return out, nil
}

func riskFromString(value string) work.Risk {
	risk := work.Risk(value)
	if !risk.Valid() {
		return ""
	}
	return risk
}

func goalFromModel(m GoalModel) (goals.Goal, error) {
	goal := goals.Goal{
		ID:           m.ID,
		Title:        m.Title,
		Brief:        m.Brief,
		WorkspaceID:  m.WorkspaceID,
		GroupID:      m.GroupID,
		BudgetTokens: m.BudgetTokens,
		Risk:         riskFromString(m.Risk),
		Status:       goals.Status(m.Status),
		CreatedBy:    m.CreatedBy,
		Error:        m.Error,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
	if len(m.ItemIDsJSON) > 0 {
		if err := json.Unmarshal(m.ItemIDsJSON, &goal.ItemIDs); err != nil {
			return goals.Goal{}, fmt.Errorf("decode goal items: %w", err)
		}
	}
	return goal, nil
}
