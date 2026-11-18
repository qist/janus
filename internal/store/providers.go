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
	ID      string `gorm:"primaryKey;size:64"`
	Name    string `gorm:"size:128"`
	Kind    string `gorm:"size:16"`
	BaseURL string `gorm:"size:512"`
	Channel string `gorm:"size:64"`
	// Protocol is the upstream wire format; empty means derived from kind.
	Protocol string `gorm:"size:16"`
	// OAuthJSON is the operator-supplied public-client flow, opaque here.
	OAuthJSON []byte `gorm:"column:oauth_json;type:blob"`
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
	Account    string `gorm:"size:191"`
	Plan       string `gorm:"size:32"`
	Kind       string `gorm:"size:16"`
	Status     string `gorm:"index;size:16"`
	Priority   int    `gorm:"index"`
	Disabled   bool   `gorm:"index"`
	Masked     string `gorm:"size:32"`
	Secret     string `gorm:"type:blob"`
	Remaining  int64
	Total      int64
	ExpiresAt  int64
	// Health is persisted so a restart does not resurrect an exhausted or
	// rate-limited account.
	LastUsedAt    int64
	CooldownUntil int64
	ResetsAt      int64
	LastError     string `gorm:"size:512"`
	// QuotaJSON is the last vendor allowance snapshot, opaque to the store.
	QuotaJSON []byte `gorm:"type:blob"`
	CreatedAt int64
	UpdatedAt int64
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
	var oauthJSON []byte
	if p.OAuth != nil {
		encoded, err := json.Marshal(p.OAuth)
		if err != nil {
			return fmt.Errorf("encode provider oauth: %w", err)
		}
		oauthJSON = encoded
	}
	m := ProviderModel{
		ID:        p.ID,
		Name:      p.Name,
		Kind:      string(p.Kind),
		BaseURL:   p.BaseURL,
		Channel:   p.Channel,
		Protocol:  string(p.Protocol),
		OAuthJSON: oauthJSON,
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
	var quotaJSON []byte
	if c.Quota != nil {
		encoded, err := json.Marshal(c.Quota)
		if err != nil {
			return fmt.Errorf("encode credential quota: %w", err)
		}
		quotaJSON = encoded
	}
	m := ProviderCredentialModel{
		ID:            c.ID,
		ProviderID:    c.ProviderID,
		Label:         c.Label,
		Account:       c.Account,
		Plan:          c.Plan,
		Kind:          string(c.Kind),
		Status:        string(c.Status),
		Priority:      c.Priority,
		Disabled:      c.Disabled,
		Masked:        c.Masked,
		Secret:        c.SecretCiphertext,
		Remaining:     c.Remaining,
		Total:         c.Total,
		ExpiresAt:     c.ExpiresAt,
		LastUsedAt:    c.LastUsedAt,
		CooldownUntil: c.CooldownUntil,
		ResetsAt:      c.ResetsAt,
		LastError:     c.LastError,
		QuotaJSON:     quotaJSON,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
	if err := s.DB.WithContext(ctx).Save(&m).Error; err != nil {
		return fmt.Errorf("save credential %q: %w", c.ID, err)
	}
	return nil
}

// ListCredentials returns credentials, optionally filtered by provider.
func (s *Store) ListCredentials(ctx context.Context, providerID string) ([]provider.Credential, error) {
	query := s.DB.WithContext(ctx).Order("priority ASC, created_at ASC, id ASC")
	if providerID != "" {
		query = query.Where("provider_id = ?", providerID)
	}

	var rows []ProviderCredentialModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]provider.Credential, 0, len(rows))
	for _, m := range rows {
		credential := provider.Credential{
			ID:               m.ID,
			ProviderID:       m.ProviderID,
			Label:            m.Label,
			Account:          m.Account,
			Plan:             m.Plan,
			Kind:             provider.CredentialKind(m.Kind),
			Status:           provider.Status(m.Status),
			Priority:         m.Priority,
			Disabled:         m.Disabled,
			Masked:           m.Masked,
			HasSecret:        m.Secret != "",
			Remaining:        m.Remaining,
			Total:            m.Total,
			ExpiresAt:        m.ExpiresAt,
			LastUsedAt:       m.LastUsedAt,
			CooldownUntil:    m.CooldownUntil,
			ResetsAt:         m.ResetsAt,
			LastError:        m.LastError,
			CreatedAt:        m.CreatedAt,
			UpdatedAt:        m.UpdatedAt,
			SecretCiphertext: m.Secret,
		}
		if len(m.QuotaJSON) > 0 {
			var quota provider.Quota
			if err := json.Unmarshal(m.QuotaJSON, &quota); err != nil {
				return nil, fmt.Errorf("decode credential quota: %w", err)
			}
			credential.Quota = &quota
		}
		out = append(out, credential)
	}
	return out, nil
}

// UpdateCredentialHealth writes only the health columns, leaving the sealed
// secret untouched.
func (s *Store) UpdateCredentialHealth(ctx context.Context, health provider.CredentialHealth) error {
	updates := map[string]any{
		"status":         string(health.Status),
		"cooldown_until": health.CooldownUntil,
		"resets_at":      health.ResetsAt,
		"last_used_at":   health.LastUsedAt,
		"last_error":     health.LastError,
		"updated_at":     health.LastUsedAt,
	}
	if health.Status == provider.StatusExhausted {
		updates["remaining"] = 0
	}
	result := s.DB.WithContext(ctx).
		Model(&ProviderCredentialModel{}).
		Where("id = ?", health.ID).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update credential health %q: %w", health.ID, result.Error)
	}
	if result.RowsAffected == 0 {
		return provider.ErrCredentialNotFound
	}
	return nil
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
	out := provider.Provider{
		ID:        m.ID,
		Name:      m.Name,
		Kind:      provider.Kind(m.Kind),
		BaseURL:   m.BaseURL,
		Channel:   m.Channel,
		Protocol:  provider.Protocol(m.Protocol),
		Enabled:   m.Enabled,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
	if len(m.OAuthJSON) > 0 {
		var oauth provider.OAuthConfig
		if err := json.Unmarshal(m.OAuthJSON, &oauth); err == nil {
			out.OAuth = &oauth
		}
	}
	return out
}
