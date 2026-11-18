package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"janus/internal/auth"
)

// APIKeyModel stores a hashed API key. The plaintext exists only in the
// response to the create call; a database dump cannot be replayed.
type APIKeyModel struct {
	ID         string `gorm:"primaryKey;size:64"`
	Kind       string `gorm:"index;size:16"`
	Name       string `gorm:"size:120"`
	Prefix     string `gorm:"index;size:24"`
	Hash       string `gorm:"uniqueIndex;size:64"`
	Scopes     []byte `gorm:"type:blob"`
	Notes      string `gorm:"size:512"`
	ExpiresAt  int64  `gorm:"index"`
	Revoked    bool   `gorm:"index"`
	LastUsedAt int64
	CreatedAt  int64
}

func (APIKeyModel) TableName() string { return "api_keys" }

// APIKey is the public view of a key. Hash is deliberately absent.
type APIKey struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	Name       string       `json:"name"`
	Prefix     string       `json:"prefix"`
	Scopes     []auth.Scope `json:"scopes"`
	Notes      string       `json:"notes,omitempty"`
	ExpiresAt  int64        `json:"expires_at,omitempty"`
	Revoked    bool         `json:"revoked"`
	LastUsedAt int64        `json:"last_used_at,omitempty"`
	CreatedAt  int64        `json:"created_at"`
}

func (m APIKeyModel) public() APIKey {
	out := APIKey{
		ID:         m.ID,
		Kind:       m.Kind,
		Name:       m.Name,
		Prefix:     m.Prefix,
		Notes:      m.Notes,
		ExpiresAt:  m.ExpiresAt,
		Revoked:    m.Revoked,
		LastUsedAt: m.LastUsedAt,
		CreatedAt:  m.CreatedAt,
	}
	if len(m.Scopes) > 0 {
		_ = json.Unmarshal(m.Scopes, &out.Scopes)
	}
	return out
}

// SaveAPIKey inserts a key. The caller has already hashed the plaintext.
func (s *Store) SaveAPIKey(ctx context.Context, key auth.KeyRecord) error {
	scopes, err := json.Marshal(key.Scopes)
	if err != nil {
		return fmt.Errorf("encode key scopes: %w", err)
	}
	model := APIKeyModel{
		ID:         key.ID,
		Kind:       key.Kind,
		Name:       key.Name,
		Prefix:     key.Prefix,
		Hash:       key.Hash,
		Scopes:     scopes,
		Notes:      key.Notes,
		ExpiresAt:  key.ExpiresAt,
		Revoked:    key.Revoked,
		LastUsedAt: key.LastUsedAt,
		CreatedAt:  key.CreatedAt,
	}
	if err := s.DB.WithContext(ctx).Create(&model).Error; err != nil {
		return fmt.Errorf("save api key %q: %w", key.ID, err)
	}
	return nil
}

// ListAPIKeys returns every key, newest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var rows []APIKeyModel
	if err := s.DB.WithContext(ctx).Order("created_at desc, id asc").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	out := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.public())
	}
	return out, nil
}

// GetAPIKey returns one key by id.
func (s *Store) GetAPIKey(ctx context.Context, id string) (APIKey, error) {
	var row APIKeyModel
	if err := s.DB.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return APIKey{}, auth.ErrKeyNotFound
		}
		return APIKey{}, fmt.Errorf("get api key %q: %w", id, err)
	}
	return row.public(), nil
}

// RevokeAPIKey marks a key unusable. The row stays so usage history keeps
// pointing at something an operator can recognise.
func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	result := s.DB.WithContext(ctx).
		Model(&APIKeyModel{}).
		Where("id = ?", id).
		Updates(map[string]any{"revoked": true})
	if result.Error != nil {
		return fmt.Errorf("revoke api key %q: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return auth.ErrKeyNotFound
	}
	return nil
}

// LookupAPIKey resolves a presented key by its digest. The auth package owns
// the digest so the plaintext never leaves that boundary.
func (s *Store) LookupAPIKey(ctx context.Context, digest string) (auth.KeyRecord, error) {
	var row APIKeyModel
	err := s.DB.WithContext(ctx).Where("hash = ?", digest).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return auth.KeyRecord{}, auth.ErrKeyNotFound
	}
	if err != nil {
		return auth.KeyRecord{}, fmt.Errorf("lookup api key: %w", err)
	}
	record := auth.KeyRecord{
		ID:         row.ID,
		Kind:       row.Kind,
		Name:       row.Name,
		Prefix:     row.Prefix,
		Hash:       row.Hash,
		Notes:      row.Notes,
		ExpiresAt:  row.ExpiresAt,
		Revoked:    row.Revoked,
		LastUsedAt: row.LastUsedAt,
		CreatedAt:  row.CreatedAt,
	}
	if len(row.Scopes) > 0 {
		_ = json.Unmarshal(row.Scopes, &record.Scopes)
	}
	return record, nil
}

// TouchAPIKey records that a key was used. Best effort by design: a failed
// write must not reject a request that was already authorised.
func (s *Store) TouchAPIKey(ctx context.Context, id string) {
	now := time.Now().Unix()
	_ = s.DB.WithContext(ctx).
		Model(&APIKeyModel{}).
		Where("id = ?", id).
		Update("last_used_at", now).Error
}
