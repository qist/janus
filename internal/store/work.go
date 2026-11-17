package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"janus/internal/work"
)

// CreateWorkItem inserts a work item.
func (s *Store) CreateWorkItem(ctx context.Context, item work.WorkItem) error {
	m, err := itemToModel(item)
	if err != nil {
		return err
	}
	if err := s.DB.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("create work item %q: %w", item.ID, err)
	}
	return nil
}

// GetWorkItem loads one work item.
func (s *Store) GetWorkItem(ctx context.Context, id string) (work.WorkItem, error) {
	var m WorkItemModel
	err := s.DB.WithContext(ctx).First(&m, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return work.WorkItem{}, work.ErrNotFound
	}
	if err != nil {
		return work.WorkItem{}, err
	}
	return itemFromModel(m)
}

// UpdateWorkItem writes the full item with optimistic locking. expectedVersion
// is the version the caller read; the stored row becomes expectedVersion+1.
func (s *Store) UpdateWorkItem(ctx context.Context, item work.WorkItem, expectedVersion int64) error {
	m, err := itemToModel(item)
	if err != nil {
		return err
	}
	m.Version = expectedVersion + 1

	res := s.DB.WithContext(ctx).
		Model(&WorkItemModel{}).
		Where("id = ? AND version = ?", item.ID, expectedVersion).
		Select("*").
		Updates(&m)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrVersionConflict
	}
	return nil
}

// ListWorkItems returns items in claim order. Empty filters are ignored.
func (s *Store) ListWorkItems(ctx context.Context, status work.Status, workspaceID string) ([]work.WorkItem, error) {
	q := s.DB.WithContext(ctx).Model(&WorkItemModel{})
	if status != "" {
		q = q.Where("status = ?", string(status))
	}
	if workspaceID != "" {
		q = q.Where("workspace_id = ?", workspaceID)
	}

	var rows []WorkItemModel
	if err := q.Order("priority DESC, created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]work.WorkItem, 0, len(rows))
	for _, m := range rows {
		item, err := itemFromModel(m)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// SaveClaim upserts a claim.
func (s *Store) SaveClaim(ctx context.Context, claim work.Claim) error {
	m, err := claimToModel(claim)
	if err != nil {
		return err
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save claim %q: %w", claim.ID, err)
	}
	return nil
}

// GetActiveClaim returns the current active lease for an item, if any.
func (s *Store) GetActiveClaim(ctx context.Context, itemID string) (work.Claim, bool, error) {
	var m WorkClaimModel
	err := s.DB.WithContext(ctx).
		Where("work_item_id = ? AND status = ?", itemID, string(work.ClaimActive)).
		Order("lease_epoch DESC").
		First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return work.Claim{}, false, nil
	}
	if err != nil {
		return work.Claim{}, false, err
	}
	claim, err := claimFromModel(m)
	if err != nil {
		return work.Claim{}, false, err
	}
	return claim, true, nil
}

// ListClaims returns the lease history for an item.
func (s *Store) ListClaims(ctx context.Context, itemID string) ([]work.Claim, error) {
	var rows []WorkClaimModel
	if err := s.DB.WithContext(ctx).
		Where("work_item_id = ?", itemID).
		Order("lease_epoch ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]work.Claim, 0, len(rows))
	for _, m := range rows {
		claim, err := claimFromModel(m)
		if err != nil {
			return nil, err
		}
		out = append(out, claim)
	}
	return out, nil
}

// CreateArtifact inserts an evidence artifact.
func (s *Store) CreateArtifact(ctx context.Context, artifact work.Artifact) error {
	m := artifactToModel(artifact)
	if err := s.DB.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("create artifact %q: %w", artifact.ID, err)
	}
	return nil
}

// ListArtifacts returns artifacts for an item.
func (s *Store) ListArtifacts(ctx context.Context, itemID string) ([]work.Artifact, error) {
	var rows []WorkArtifactModel
	if err := s.DB.WithContext(ctx).
		Where("work_item_id = ?", itemID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]work.Artifact, 0, len(rows))
	for _, m := range rows {
		out = append(out, artifactFromModel(m))
	}
	return out, nil
}

// CreateReview inserts a verifier decision.
func (s *Store) CreateReview(ctx context.Context, review work.Review) error {
	m, err := reviewToModel(review)
	if err != nil {
		return err
	}
	if err := s.DB.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("create review %q: %w", review.ID, err)
	}
	return nil
}

// ListReviews returns reviews for an item.
func (s *Store) ListReviews(ctx context.Context, itemID string) ([]work.Review, error) {
	var rows []WorkReviewModel
	if err := s.DB.WithContext(ctx).
		Where("work_item_id = ?", itemID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]work.Review, 0, len(rows))
	for _, m := range rows {
		review, err := reviewFromModel(m)
		if err != nil {
			return nil, err
		}
		out = append(out, review)
	}
	return out, nil
}

// AppendEvent adds an immutable audit event.
func (s *Store) AppendEvent(ctx context.Context, event work.Event) error {
	m := WorkEventModel{
		WorkItemID: event.WorkItemID,
		Type:       event.Type,
		Actor:      event.Actor,
		Payload:    event.Payload,
		CreatedAt:  event.CreatedAt,
	}
	if err := s.DB.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// ListEvents returns events for an item in append order.
func (s *Store) ListEvents(ctx context.Context, itemID string) ([]work.Event, error) {
	var rows []WorkEventModel
	if err := s.DB.WithContext(ctx).
		Where("work_item_id = ?", itemID).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]work.Event, 0, len(rows))
	for _, m := range rows {
		out = append(out, work.Event{
			ID:         uint64(m.ID),
			WorkItemID: m.WorkItemID,
			Type:       m.Type,
			Actor:      m.Actor,
			Payload:    m.Payload,
			CreatedAt:  m.CreatedAt,
		})
	}
	return out, nil
}

func itemToModel(item work.WorkItem) (WorkItemModel, error) {
	contract, err := json.Marshal(item.Contract)
	if err != nil {
		return WorkItemModel{}, fmt.Errorf("encode contract: %w", err)
	}
	capabilities, err := json.Marshal(item.Capabilities)
	if err != nil {
		return WorkItemModel{}, fmt.Errorf("encode capabilities: %w", err)
	}
	modes, err := json.Marshal(item.RequiredModes)
	if err != nil {
		return WorkItemModel{}, fmt.Errorf("encode required modes: %w", err)
	}
	dependsOn, err := json.Marshal(item.DependsOn)
	if err != nil {
		return WorkItemModel{}, fmt.Errorf("encode dependencies: %w", err)
	}

	return WorkItemModel{
		ID:                 item.ID,
		WorkspaceID:        item.WorkspaceID,
		ParentID:           item.ParentID,
		Title:              item.Title,
		Brief:              item.Brief,
		ContractJSON:       contract,
		CapabilitiesJSON:   capabilities,
		ExecutionSite:      item.ExecutionSite,
		RequiredModesJSON:  modes,
		DependsOnJSON:      dependsOn,
		Priority:           item.Priority,
		Status:             string(item.Status),
		Attempts:           item.Attempts,
		LeaseExpiries:      item.LeaseExpiries,
		Replicas:           item.Replicas,
		CreatedBy:          item.CreatedBy,
		SubmittedBy:        item.SubmittedBy,
		LastIdempotencyKey: item.LastIdempotencyKey,
		Version:            item.Version,
		CreatedAt:          item.CreatedAt,
		UpdatedAt:          item.UpdatedAt,
	}, nil
}

func itemFromModel(m WorkItemModel) (work.WorkItem, error) {
	item := work.WorkItem{
		ID:                 m.ID,
		WorkspaceID:        m.WorkspaceID,
		ParentID:           m.ParentID,
		Title:              m.Title,
		Brief:              m.Brief,
		ExecutionSite:      m.ExecutionSite,
		Priority:           m.Priority,
		Status:             work.Status(m.Status),
		Attempts:           m.Attempts,
		LeaseExpiries:      m.LeaseExpiries,
		Replicas:           m.Replicas,
		CreatedBy:          m.CreatedBy,
		SubmittedBy:        m.SubmittedBy,
		LastIdempotencyKey: m.LastIdempotencyKey,
		Version:            m.Version,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}

	if err := decodeJSON(m.ContractJSON, &item.Contract); err != nil {
		return work.WorkItem{}, fmt.Errorf("decode contract: %w", err)
	}
	if err := decodeJSON(m.CapabilitiesJSON, &item.Capabilities); err != nil {
		return work.WorkItem{}, fmt.Errorf("decode capabilities: %w", err)
	}
	if err := decodeJSON(m.RequiredModesJSON, &item.RequiredModes); err != nil {
		return work.WorkItem{}, fmt.Errorf("decode required modes: %w", err)
	}
	if err := decodeJSON(m.DependsOnJSON, &item.DependsOn); err != nil {
		return work.WorkItem{}, fmt.Errorf("decode dependencies: %w", err)
	}
	return item, nil
}

func claimToModel(c work.Claim) (WorkClaimModel, error) {
	var evidence []byte
	if c.Evidence != nil {
		encoded, err := json.Marshal(c.Evidence)
		if err != nil {
			return WorkClaimModel{}, fmt.Errorf("encode claim evidence: %w", err)
		}
		evidence = encoded
	}
	return WorkClaimModel{
		ID:             c.ID,
		WorkItemID:     c.WorkItemID,
		AgentID:        c.AgentID,
		Role:           string(c.Role),
		LeaseEpoch:     c.LeaseEpoch,
		LeaseExpiresAt: c.LeaseExpiresAt,
		HeartbeatAt:    c.HeartbeatAt,
		Forced:         c.Forced,
		Plan:           c.Plan,
		Note:           c.Note,
		Status:         string(c.Status),
		IdempotencyKey: c.IdempotencyKey,
		EvidenceJSON:   evidence,
		CreatedAt:      c.CreatedAt,
	}, nil
}

func claimFromModel(m WorkClaimModel) (work.Claim, error) {
	c := work.Claim{
		ID:             m.ID,
		WorkItemID:     m.WorkItemID,
		AgentID:        m.AgentID,
		Role:           work.Role(m.Role),
		LeaseEpoch:     m.LeaseEpoch,
		LeaseExpiresAt: m.LeaseExpiresAt,
		HeartbeatAt:    m.HeartbeatAt,
		Forced:         m.Forced,
		Plan:           m.Plan,
		Note:           m.Note,
		Status:         work.ClaimStatus(m.Status),
		IdempotencyKey: m.IdempotencyKey,
		CreatedAt:      m.CreatedAt,
	}
	if len(m.EvidenceJSON) > 0 {
		var evidence work.Evidence
		if err := json.Unmarshal(m.EvidenceJSON, &evidence); err != nil {
			return work.Claim{}, fmt.Errorf("decode claim evidence: %w", err)
		}
		c.Evidence = &evidence
	}
	return c, nil
}

func artifactToModel(a work.Artifact) WorkArtifactModel {
	return WorkArtifactModel{
		ID:         a.ID,
		WorkItemID: a.WorkItemID,
		Kind:       a.Kind,
		URI:        a.URI,
		SHA256:     a.SHA256,
		Bytes:      a.Bytes,
		Summary:    a.Summary,
		CreatedBy:  a.CreatedBy,
		CreatedAt:  a.CreatedAt,
	}
}

func artifactFromModel(m WorkArtifactModel) work.Artifact {
	return work.Artifact{
		ID:         m.ID,
		WorkItemID: m.WorkItemID,
		Kind:       m.Kind,
		URI:        m.URI,
		SHA256:     m.SHA256,
		Bytes:      m.Bytes,
		Summary:    m.Summary,
		CreatedBy:  m.CreatedBy,
		CreatedAt:  m.CreatedAt,
	}
}

func reviewToModel(r work.Review) (WorkReviewModel, error) {
	evidence, err := json.Marshal(r.Evidence)
	if err != nil {
		return WorkReviewModel{}, fmt.Errorf("encode review evidence: %w", err)
	}
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		return WorkReviewModel{}, fmt.Errorf("encode review checks: %w", err)
	}
	return WorkReviewModel{
		ID:           r.ID,
		WorkItemID:   r.WorkItemID,
		ReviewerID:   r.ReviewerID,
		Verdict:      string(r.Verdict),
		Reasons:      r.Reasons,
		EvidenceJSON: evidence,
		ChecksJSON:   checks,
		CreatedAt:    r.CreatedAt,
	}, nil
}

func reviewFromModel(m WorkReviewModel) (work.Review, error) {
	r := work.Review{
		ID:         m.ID,
		WorkItemID: m.WorkItemID,
		ReviewerID: m.ReviewerID,
		Verdict:    work.Verdict(m.Verdict),
		Reasons:    m.Reasons,
		CreatedAt:  m.CreatedAt,
	}
	if err := decodeJSON(m.EvidenceJSON, &r.Evidence); err != nil {
		return work.Review{}, fmt.Errorf("decode review evidence: %w", err)
	}
	if err := decodeJSON(m.ChecksJSON, &r.Checks); err != nil {
		return work.Review{}, fmt.Errorf("decode review checks: %w", err)
	}
	return r, nil
}

func decodeJSON(data []byte, dst any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}
