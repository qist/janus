// Package goals turns a one-paragraph intent into work items and tracks the
// goal as the items move through the board. A goal is a container, not a second
// state machine: item states stay authoritative.
package goals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"janus/internal/work"
)

// Status is the lifecycle of a goal.
type Status string

const (
	StatusPlanning  Status = "planning"
	StatusActive    Status = "active"
	StatusBlocked   Status = "blocked"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
	StatusFailed    Status = "failed"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPlanning, StatusActive, StatusBlocked, StatusDone, StatusCancelled, StatusFailed:
		return true
	}
	return false
}

// Goal is one intent plus the items it was split into.
type Goal struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Brief        string    `json:"brief"`
	WorkspaceID  string    `json:"workspace_id"`
	GroupID      string    `json:"group_id,omitempty"`
	BudgetTokens int64     `json:"budget_tokens,omitempty"`
	Risk         work.Risk `json:"risk,omitempty"`
	Status       Status    `json:"status"`
	ItemIDs      []string  `json:"item_ids,omitempty"`
	CreatedBy    string    `json:"created_by,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    int64     `json:"created_at"`
	UpdatedAt    int64     `json:"updated_at"`
}

// ErrNotFound means no goal is stored under that id.
var ErrNotFound = errors.New("goal not found")

// Repository persists goals.
type Repository interface {
	SaveGoal(ctx context.Context, goal Goal) error
	GetGoal(ctx context.Context, id string) (Goal, error)
	ListGoals(ctx context.Context, limit int) ([]Goal, error)
}

// MemoryRepo is the in-process repository for tests and single-node dev.
type MemoryRepo struct {
	goals map[string]Goal
}

// NewMemoryRepo returns an empty repository.
func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{goals: make(map[string]Goal)}
}

// SaveGoal implements Repository.
func (r *MemoryRepo) SaveGoal(_ context.Context, goal Goal) error {
	r.goals[goal.ID] = cloneGoal(goal)
	return nil
}

// GetGoal implements Repository.
func (r *MemoryRepo) GetGoal(_ context.Context, id string) (Goal, error) {
	goal, ok := r.goals[id]
	if !ok {
		return Goal{}, ErrNotFound
	}
	return cloneGoal(goal), nil
}

// ListGoals implements Repository, newest first.
func (r *MemoryRepo) ListGoals(_ context.Context, limit int) ([]Goal, error) {
	out := make([]Goal, 0, len(r.goals))
	for _, goal := range r.goals {
		out = append(out, cloneGoal(goal))
	}
	sortGoals(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sortGoals(goals []Goal) {
	for i := 1; i < len(goals); i++ {
		for j := i; j > 0 && goals[j].CreatedAt > goals[j-1].CreatedAt; j-- {
			goals[j], goals[j-1] = goals[j-1], goals[j]
		}
	}
}

func cloneGoal(goal Goal) Goal {
	out := goal
	out.ItemIDs = append([]string(nil), goal.ItemIDs...)
	return out
}

// Planner splits a goal into work items. The default is intentionally one
// item: a real planner is a model call, and shipping a bad heuristic is worse
// than shipping an honest single-item plan.
type Planner interface {
	Plan(ctx context.Context, goal Goal) ([]work.WorkItem, error)
}

// SinglePlanner creates one item that carries the whole goal.
type SinglePlanner struct{}

// Plan implements Planner.
func (SinglePlanner) Plan(_ context.Context, goal Goal) ([]work.WorkItem, error) {
	title := strings.TrimSpace(goal.Title)
	if title == "" {
		return nil, errors.New("goal title required")
	}
	risk := goal.Risk
	if risk == "" {
		risk = work.RiskMedium
	}
	if !risk.Valid() {
		return nil, fmt.Errorf("invalid risk %q", risk)
	}
	return []work.WorkItem{{
		ID:          "w_" + goal.ID,
		WorkspaceID: goal.WorkspaceID,
		Title:       title,
		Brief:       strings.TrimSpace(goal.Brief),
		Priority:    0,
		CreatedBy:   goal.CreatedBy,
		Contract: work.Contract{
			Goal:         firstNonEmpty(goal.Brief, title),
			Risk:         risk,
			BudgetTokens: goal.BudgetTokens,
		},
	}}, nil
}

// Service creates goals, plans them and reports progress.
type Service struct {
	Repo    Repository
	Board   *work.Board
	Planner Planner
	Now     func() int64
}

// New returns a service with the single-item planner.
func New(repo Repository, board *work.Board) *Service {
	return &Service{Repo: repo, Board: board, Planner: SinglePlanner{}}
}

// CreateRequest is the accepted input.
type CreateRequest struct {
	Title        string    `json:"title"`
	Brief        string    `json:"brief"`
	WorkspaceID  string    `json:"workspace_id"`
	GroupID      string    `json:"group_id,omitempty"`
	BudgetTokens int64     `json:"budget_tokens,omitempty"`
	Risk         work.Risk `json:"risk,omitempty"`
	CreatedBy    string    `json:"-"`
}

// Create plans synchronously and returns the finished goal.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Goal, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Goal{}, errors.New("goal title required")
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		return Goal{}, errors.New("goal workspace_id required")
	}
	if s.Board == nil {
		return Goal{}, errors.New("work board required")
	}

	now := s.now()
	goal := Goal{
		ID:           newID(),
		Title:        strings.TrimSpace(req.Title),
		Brief:        strings.TrimSpace(req.Brief),
		WorkspaceID:  strings.TrimSpace(req.WorkspaceID),
		GroupID:      strings.TrimSpace(req.GroupID),
		BudgetTokens: req.BudgetTokens,
		Risk:         req.Risk,
		Status:       StatusPlanning,
		CreatedBy:    strings.TrimSpace(req.CreatedBy),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if goal.Risk == "" {
		goal.Risk = work.RiskMedium
	}
	if err := s.Repo.SaveGoal(ctx, goal); err != nil {
		return Goal{}, err
	}
	return s.planAndSave(ctx, goal)
}

// Submit plans in the background and returns immediately, for POST /goals which
// answers 202. Planning survives the request being cancelled.
func (s *Service) Submit(ctx context.Context, req CreateRequest) (Goal, error) {
	if strings.TrimSpace(req.Title) == "" {
		return Goal{}, errors.New("goal title required")
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		return Goal{}, errors.New("goal workspace_id required")
	}

	now := s.now()
	goal := Goal{
		ID:           newID(),
		Title:        strings.TrimSpace(req.Title),
		Brief:        strings.TrimSpace(req.Brief),
		WorkspaceID:  strings.TrimSpace(req.WorkspaceID),
		GroupID:      strings.TrimSpace(req.GroupID),
		BudgetTokens: req.BudgetTokens,
		Risk:         req.Risk,
		Status:       StatusPlanning,
		CreatedBy:    strings.TrimSpace(req.CreatedBy),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if goal.Risk == "" {
		goal.Risk = work.RiskMedium
	}
	if err := s.Repo.SaveGoal(ctx, goal); err != nil {
		return Goal{}, err
	}

	detached := context.WithoutCancel(ctx)
	go func() {
		_, _ = s.planAndSave(detached, goal)
	}()
	return goal, nil
}

func (s *Service) planAndSave(ctx context.Context, goal Goal) (Goal, error) {
	items, err := s.Planner.Plan(ctx, goal)
	if err != nil {
		return s.fail(ctx, goal, "plan: "+err.Error())
	}
	if len(items) == 0 {
		return s.fail(ctx, goal, "planner returned no items")
	}

	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.WorkspaceID == "" {
			item.WorkspaceID = goal.WorkspaceID
		}
		if item.CreatedBy == "" {
			item.CreatedBy = goal.CreatedBy
		}
		if err := s.Board.Add(ctx, item); err != nil {
			return s.fail(ctx, goal, fmt.Sprintf("create item %s: %v", item.ID, err))
		}
		ids = append(ids, item.ID)
	}

	goal.ItemIDs = ids
	goal.Status = StatusActive
	goal.UpdatedAt = s.now()
	if err := s.Repo.SaveGoal(ctx, goal); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

func (s *Service) fail(ctx context.Context, goal Goal, reason string) (Goal, error) {
	goal.Status = StatusFailed
	goal.Error = reason
	goal.UpdatedAt = s.now()
	_ = s.Repo.SaveGoal(ctx, goal)
	return goal, errors.New(reason)
}

// Get returns one goal.
func (s *Service) Get(ctx context.Context, id string) (Goal, error) {
	return s.Repo.GetGoal(ctx, id)
}

// List returns goals, newest first.
func (s *Service) List(ctx context.Context, limit int) ([]Goal, error) {
	return s.Repo.ListGoals(ctx, limit)
}

// Cancel stops a goal. Items already on the board are left alone: they are
// independent work with their own leases.
func (s *Service) Cancel(ctx context.Context, id string) (Goal, error) {
	goal, err := s.Repo.GetGoal(ctx, id)
	if err != nil {
		return Goal{}, err
	}
	switch goal.Status {
	case StatusDone, StatusCancelled:
		return goal, nil
	}
	goal.Status = StatusCancelled
	goal.UpdatedAt = s.now()
	if err := s.Repo.SaveGoal(ctx, goal); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

// Progress is the aggregated view used by GET /goals/{id} and the SSE stream.
type Progress struct {
	GoalID    string         `json:"goal_id"`
	Status    Status         `json:"status"`
	Total     int            `json:"total"`
	Done      int            `json:"done"`
	Blocked   int            `json:"blocked"`
	Counts    map[string]int `json:"counts"`
	Percent   int            `json:"percent"`
	Error     string         `json:"error,omitempty"`
	UpdatedAt int64          `json:"updated_at"`
}

// Progress computes the current aggregate and promotes the goal to done when
// every item is done.
func (s *Service) Progress(ctx context.Context, id string) (Progress, error) {
	goal, err := s.Repo.GetGoal(ctx, id)
	if err != nil {
		return Progress{}, err
	}
	if s.Board == nil {
		return Progress{}, errors.New("work board required")
	}

	out := Progress{
		GoalID: goal.ID,
		Status: goal.Status,
		Counts: make(map[string]int, len(goal.ItemIDs)),
		Error:  goal.Error,
	}
	for _, itemID := range goal.ItemIDs {
		item, ok := s.Board.Get(ctx, itemID)
		if !ok {
			continue
		}
		out.Total++
		out.Counts[string(item.Status)]++
		switch item.Status {
		case work.StatusDone:
			out.Done++
		case work.StatusBlocked:
			out.Blocked++
		}
	}
	if out.Total > 0 {
		out.Percent = out.Done * 100 / out.Total
	}

	switch goal.Status {
	case StatusPlanning, StatusFailed, StatusCancelled, StatusDone:
		out.UpdatedAt = goal.UpdatedAt
		return out, nil
	}

	previous := goal.Status
	switch {
	case out.Total > 0 && out.Done == out.Total:
		goal.Status = StatusDone
	case out.Blocked > 0:
		goal.Status = StatusBlocked
	default:
		goal.Status = StatusActive
	}
	out.Status = goal.Status
	out.UpdatedAt = s.now()
	if goal.Status == previous {
		// A read-only poll must not rewrite the goal on every request.
		out.UpdatedAt = goal.UpdatedAt
		return out, nil
	}
	goal.UpdatedAt = out.UpdatedAt
	if err := s.Repo.SaveGoal(ctx, goal); err != nil {
		return Progress{}, err
	}
	return out, nil
}

func (s *Service) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().Unix()
}

func newID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("goal_%d", time.Now().UnixNano())
	}
	return "goal_" + hex.EncodeToString(buf[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
