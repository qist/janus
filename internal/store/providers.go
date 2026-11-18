package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"janus/internal/provider"
)

var _ provider.Store = (*Store)(nil)

// ProviderModel is the persisted upstream endpoint.
type ProviderModel struct {
	ID        string `gorm:"primaryKey;size:64"`
	Name      string `gorm:"size:128"`
	Kind      string `gorm:"size:16"`
	BaseURL   string `gorm:"size:512"`
	Enabled   bool   `gorm:"index"`
	CreatedAt int64
	UpdatedAt int64
}

func (ProviderModel) TableName() string { return "model_providers" }

// ProviderCredentialModel stores only ciphertext. There is deliberately no
// plaintext column: a migration cannot resurrect what was never written.
type ProviderCredentialModel struct {
	ID         string `gorm:"primaryKey;size:64"`
	ProviderID string `gorm:"index;size:64"`
	Label      string `gorm:"size:128"`
	Kind       string `gorm:"size:16"`
	Status     string `gorm:"index;size:16"`
	Masked     string `gorm:"size:32"`
	Secret     string `gorm:"type:blob"`
	Remaining  int64
	Total      int64
	ExpiresAt  int64
	CreatedAt  int64
	UpdatedAt  int64
}

func (ProviderCredentialModel) TableName() string { return "provider_credentials" }

// ModelRouteModel maps an exposed model to one provider's upstream model.
type ModelRouteModel struct {
	ID            string `gorm:"primaryKey;size:64"`
	ExposedModel  string `gorm:"uniqueIndex;size:128"`
	ProviderID    string `gorm:"index;size:64"`
	UpstreamModel string `gorm:"size:128"`
	AliasesJSON   []byte `gorm:"type:blob"`
	Priority      int
	Enabled       bool
	CreatedAt     int64
	UpdatedAt     int64
}

func (ModelRouteModel) TableName() string { return "model_routes" }

// SaveProvider upserts a provider.
func (s *Store) SaveProvider(ctx context.Context, p provider.Provider) error {
	m := ProviderModel{
		ID:        p.ID,
		Name:      p.Name,
		Kind:      string(p.Kind),
		BaseURL:   p.BaseURL,
		Enabled:   p.Enabled,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save provider %q: %w", p.ID, err)
	}
	return nil
}

// GetProvider loads one provider.
func (s *Store) GetProvider(ctx context.Context, id string) (provider.Provider, error) {
	var m ProviderModel
	err := s.DB.WithContext(ctx).First(&m, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return provider.Provider{}, provider.ErrNotFound
	}
	if err != nil {
		return provider.Provider{}, err
	}
	return providerFromModel(m), nil
}

// ListProviders returns all providers.
func (s *Store) ListProviders(ctx context.Context) ([]provider.Provider, error) {
	var rows []ProviderModel
	if err := s.DB.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]provider.Provider, 0, len(rows))
	for _, m := range rows {
		out = append(out, providerFromModel(m))
	}
	return out, nil
}

// DeleteProvider removes one provider row.
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	result := s.DB.WithContext(ctx).Delete(&ProviderModel{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return provider.ErrNotFound
	}
	return nil
}

// SaveCredential upserts a credential including its sealed secret.
func (s *Store) SaveCredential(ctx context.Context, c provider.Credential) error {
	m := ProviderCredentialModel{
		ID:         c.ID,
		ProviderID: c.ProviderID,
		Label:      c.Label,
		Kind:       string(c.Kind),
		Status:     string(c.Status),
		Masked:     c.Masked,
		Secret:     c.SecretCiphertext,
		Remaining:  c.Remaining,
		Total:      c.Total,
		ExpiresAt:  c.ExpiresAt,
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.UpdatedAt,
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save credential %q: %w", c.ID, err)
	}
	return nil
}

// ListCredentials returns credentials, optionally filtered by provider.
func (s *Store) ListCredentials(ctx context.Context, providerID string) ([]provider.Credential, error) {
	query := s.DB.WithContext(ctx).Order("id ASC")
	if providerID != "" {
		query = query.Where("provider_id = ?", providerID)
	}

	var rows []ProviderCredentialModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]provider.Credential, 0, len(rows))
	for _, m := range rows {
		out = append(out, provider.Credential{
			ID:               m.ID,
			ProviderID:       m.ProviderID,
			Label:            m.Label,
			Kind:             provider.CredentialKind(m.Kind),
			Status:           provider.Status(m.Status),
			Masked:           m.Masked,
			HasSecret:        m.Secret != "",
			Remaining:        m.Remaining,
			Total:            m.Total,
			ExpiresAt:        m.ExpiresAt,
			CreatedAt:        m.CreatedAt,
			UpdatedAt:        m.UpdatedAt,
			SecretCiphertext: m.Secret,
		})
	}
	return out, nil
}

// DeleteCredential removes one credential row.
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	result := s.DB.WithContext(ctx).Delete(&ProviderCredentialModel{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return provider.ErrNotFound
	}
	return nil
}

// SaveRoute upserts a model route.
func (s *Store) SaveRoute(ctx context.Context, r provider.Route) error {
	aliases, err := json.Marshal(r.Aliases)
	if err != nil {
		return fmt.Errorf("encode route aliases: %w", err)
	}
	m := ModelRouteModel{
		ID:            r.ID,
		ExposedModel:  r.ExposedModel,
		ProviderID:    r.ProviderID,
		UpstreamModel: r.UpstreamModel,
		AliasesJSON:   aliases,
		Priority:      r.Priority,
		Enabled:       r.Enabled,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save model route %q: %w", r.ExposedModel, err)
	}
	return nil
}

// ListRoutes returns every route.
func (s *Store) ListRoutes(ctx context.Context) ([]provider.Route, error) {
	var rows []ModelRouteModel
	if err := s.DB.WithContext(ctx).Order("exposed_model ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]provider.Route, 0, len(rows))
	for _, m := range rows {
		route := provider.Route{
			ID:            m.ID,
			ExposedModel:  m.ExposedModel,
			ProviderID:    m.ProviderID,
			UpstreamModel: m.UpstreamModel,
			Priority:      m.Priority,
			Enabled:       m.Enabled,
			CreatedAt:     m.CreatedAt,
			UpdatedAt:     m.UpdatedAt,
		}
		if len(m.AliasesJSON) > 0 {
			if err := json.Unmarshal(m.AliasesJSON, &route.Aliases); err != nil {
				return nil, fmt.Errorf("decode route aliases: %w", err)
			}
		}
		out = append(out, route)
	}
	return out, nil
}

// DeleteRoute removes one route row.
func (s *Store) DeleteRoute(ctx context.Context, id string) error {
	result := s.DB.WithContext(ctx).Delete(&ModelRouteModel{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return provider.ErrNotFound
	}
	return nil
}

func providerFromModel(m ProviderModel) provider.Provider {
	return provider.Provider{
		ID:        m.ID,
		Name:      m.Name,
		Kind:      provider.Kind(m.Kind),
		BaseURL:   m.BaseURL,
		Enabled:   m.Enabled,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}
