package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"gorm.io/gorm"

	"janus/internal/usage"
)

var _ usage.Ledger = (*Store)(nil)

// UsageEntryModel is one model call. Rows are append-only; there is no update
// path on purpose, so a price change or a config edit cannot rewrite history.
type UsageEntryModel struct {
	ID               string `gorm:"primaryKey;size:32"`
	RequestID        string `gorm:"size:64;index"`
	KeyID            string `gorm:"size:64;index"`
	GroupID          string `gorm:"size:64;index"`
	WorkItemID       string `gorm:"size:64;index"`
	ProviderID       string `gorm:"size:64;index"`
	CredentialID     string `gorm:"size:64;index"`
	Model            string `gorm:"size:128;index"`
	UpstreamModel    string `gorm:"size:128"`
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	CostUSD          float64
	Status           int
	ErrorCode        string `gorm:"size:32"`
	Stream           bool
	DurationMS       int64
	CreatedAt        int64 `gorm:"index"`
}

func (UsageEntryModel) TableName() string { return "usage_entries" }

// Append implements usage.Ledger.
func (s *Store) Append(ctx context.Context, entry usage.Entry) error {
	if entry.Model == "" {
		return fmt.Errorf("usage entry requires a model")
	}
	if entry.ID == "" {
		entry.ID = newUsageID()
	}
	m := UsageEntryModel{
		ID:               entry.ID,
		RequestID:        entry.RequestID,
		KeyID:            entry.KeyID,
		GroupID:          entry.GroupID,
		WorkItemID:       entry.WorkItemID,
		ProviderID:       entry.ProviderID,
		CredentialID:     entry.CredentialID,
		Model:            entry.Model,
		UpstreamModel:    entry.UpstreamModel,
		PromptTokens:     entry.PromptTokens,
		CompletionTokens: entry.CompletionTokens,
		CachedTokens:     entry.CachedTokens,
		CostUSD:          entry.CostUSD,
		Status:           entry.Status,
		ErrorCode:        entry.ErrorCode,
		Stream:           entry.Stream,
		DurationMS:       entry.DurationMS,
		CreatedAt:        entry.CreatedAt,
	}
	if err := s.DB.WithContext(ctx).Create(&m).Error; err != nil {
		return fmt.Errorf("append usage: %w", err)
	}
	return nil
}

// Totals implements usage.Ledger.
func (s *Store) Totals(ctx context.Context, filter usage.Filter) (usage.Totals, error) {
	var row struct {
		Entries          int
		Failed           int
		PromptTokens     int
		CompletionTokens int
		CachedTokens     int
		CostUSD          float64
		DurationMS       int64
	}
	err := usageSelect(s.DB.WithContext(ctx).Model(&UsageEntryModel{}), filter).
		Select(usageTotalsColumns).
		Scan(&row).Error
	if err != nil {
		return usage.Totals{}, err
	}
	return usage.Totals{
		Entries:          row.Entries,
		Failed:           row.Failed,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		CachedTokens:     row.CachedTokens,
		CostUSD:          row.CostUSD,
		DurationMS:       row.DurationMS,
	}, nil
}

// Stats implements usage.Ledger. Every dimension is one grouped query; the
// trend interval comes from the same helpers the in-memory ledger uses so both
// backends return identical shapes.
func (s *Store) Stats(ctx context.Context, query usage.StatsQuery) (usage.Stats, error) {
	base := usageSelect(s.DB.WithContext(ctx).Model(&UsageEntryModel{}), query.Filter)

	totals, err := s.Totals(ctx, query.Filter)
	if err != nil {
		return usage.Stats{}, err
	}
	bucket := usage.NormalizeBucket(query.Bucket, query.Filter)
	out := usage.Stats{
		Totals:     totals,
		ByModel:    []usage.Group{},
		ByProvider: []usage.Group{},
		ByKey:      []usage.Group{},
		Trend:      []usage.Bucket{},
		Bucket:     bucket,
	}

	limit := query.TopN
	if limit == 0 {
		limit = 10
	}
	grouped := func(column string) ([]usage.Group, error) {
		rows := []usage.Group{}
		q := usageSelect(s.DB.WithContext(ctx).Model(&UsageEntryModel{}), query.Filter).
			Select(column + " AS name, " + usageGroupColumns).
			Group(column).
			Order("entries DESC, name ASC")
		if limit > 0 {
			q = q.Limit(limit)
		}
		if err := q.Scan(&rows).Error; err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []usage.Group{}
		}
		return rows, nil
	}

	if out.ByModel, err = grouped("model"); err != nil {
		return usage.Stats{}, err
	}
	if out.ByProvider, err = grouped("provider_id"); err != nil {
		return usage.Stats{}, err
	}
	if out.ByKey, err = grouped("key_id"); err != nil {
		return usage.Stats{}, err
	}

	size := usage.BucketSeconds(bucket)
	if err := base.
		Select(fmt.Sprintf("(created_at / %d) * %d AS start, %s", size, size, usageGroupColumns)).
		Group("start").
		Order("start ASC").
		Scan(&out.Trend).Error; err != nil {
		return usage.Stats{}, err
	}
	if out.Trend == nil {
		out.Trend = []usage.Bucket{}
	}
	return out, nil
}

// Recent implements usage.Ledger.
func (s *Store) Recent(ctx context.Context, filter usage.Filter, limit int) ([]usage.Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows := []UsageEntryModel{}
	if err := usageSelect(s.DB.WithContext(ctx).Model(&UsageEntryModel{}), filter).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]usage.Entry, 0, len(rows))
	for _, m := range rows {
		out = append(out, usage.Entry{
			ID:               m.ID,
			RequestID:        m.RequestID,
			KeyID:            m.KeyID,
			GroupID:          m.GroupID,
			WorkItemID:       m.WorkItemID,
			ProviderID:       m.ProviderID,
			CredentialID:     m.CredentialID,
			Model:            m.Model,
			UpstreamModel:    m.UpstreamModel,
			PromptTokens:     m.PromptTokens,
			CompletionTokens: m.CompletionTokens,
			CachedTokens:     m.CachedTokens,
			CostUSD:          m.CostUSD,
			Status:           m.Status,
			ErrorCode:        m.ErrorCode,
			Stream:           m.Stream,
			DurationMS:       m.DurationMS,
			CreatedAt:        m.CreatedAt,
		})
	}
	return out, nil
}

const usageTotalsColumns = `COUNT(*) AS entries,
	COALESCE(SUM(CASE WHEN error_code <> '' OR (status <> 0 AND status >= 400) THEN 1 ELSE 0 END), 0) AS failed,
	COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
	COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
	COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
	COALESCE(SUM(cost_usd), 0) AS cost_usd,
	COALESCE(SUM(duration_ms), 0) AS duration_ms`

const usageGroupColumns = `COUNT(*) AS entries,
	COALESCE(SUM(CASE WHEN error_code <> '' OR (status <> 0 AND status >= 400) THEN 1 ELSE 0 END), 0) AS failed,
	COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
	COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
	COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
	COALESCE(SUM(cost_usd), 0) AS cost_usd,
	COALESCE(SUM(duration_ms), 0) AS duration_ms`

func usageSelect(db *gorm.DB, filter usage.Filter) *gorm.DB {
	if filter.KeyID != "" {
		db = db.Where("key_id = ?", filter.KeyID)
	}
	if filter.GroupID != "" {
		db = db.Where("group_id = ?", filter.GroupID)
	}
	if filter.WorkItemID != "" {
		db = db.Where("work_item_id = ?", filter.WorkItemID)
	}
	if filter.CredentialID != "" {
		db = db.Where("credential_id = ?", filter.CredentialID)
	}
	if filter.ProviderID != "" {
		db = db.Where("provider_id = ?", filter.ProviderID)
	}
	if filter.Model != "" {
		db = db.Where("model = ?", filter.Model)
	}
	if filter.Since > 0 {
		db = db.Where("created_at >= ?", filter.Since)
	}
	if filter.Until > 0 {
		db = db.Where("created_at <= ?", filter.Until)
	}
	return db
}

func newUsageID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("usg_%d", nowUnixNano())
	}
	return "usg_" + hex.EncodeToString(buf[:])
}

func nowUnixNano() int64 { return time.Now().UnixNano() }
