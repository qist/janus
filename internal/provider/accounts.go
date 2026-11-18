package provider

import (
	"context"
	"fmt"
	"strings"
)

// CredentialPatch changes how an account is used without touching its secret.
type CredentialPatch struct {
	Label    *string
	Account  *string
	Priority *int
	Disabled *bool
}

// CredentialHealth is the subset of a credential the request path may change.
// Keeping it separate from Credential means a health write cannot clobber a
// token that a concurrent refresh just stored.
type CredentialHealth struct {
	ID            string
	Status        Status
	CooldownUntil int64
	ResetsAt      int64
	LastUsedAt    int64
	LastError     string
}

// GetCredential returns one credential, masked.
func (s *Service) GetCredential(ctx context.Context, id string) (Credential, error) {
	list, err := s.Store.ListCredentials(ctx, "")
	if err != nil {
		return Credential{}, err
	}
	for _, credential := range list {
		if credential.ID == id {
			return publicCredential(credential), nil
		}
	}
	return Credential{}, ErrCredentialNotFound
}

// UpdateCredential applies a patch to one account. Disabling keeps the row and
// its history; it only leaves rotation.
func (s *Service) UpdateCredential(ctx context.Context, id string, patch CredentialPatch) (Credential, error) {
	credential, err := s.credential(ctx, id)
	if err != nil {
		return Credential{}, err
	}
	if patch.Label != nil {
		credential.Label = strings.TrimSpace(*patch.Label)
	}
	if patch.Account != nil {
		credential.Account = strings.TrimSpace(*patch.Account)
	}
	if patch.Priority != nil {
		if *patch.Priority < 0 {
			return Credential{}, fmt.Errorf("%w: priority must be >= 0", ErrInvalidInput)
		}
		credential.Priority = *patch.Priority
	}
	if patch.Disabled != nil {
		credential.Disabled = *patch.Disabled
		// Re-enabling clears a terminal invalid state: the operator has
		// presumably fixed or re-authorized the account.
		if !credential.Disabled && credential.Status == StatusInvalid {
			credential.Status = StatusUnknown
			credential.LastError = ""
		}
	}
	credential.UpdatedAt = s.now()
	if err := s.saveCredential(ctx, credential); err != nil {
		return Credential{}, err
	}
	return publicCredential(credential), nil
}

// ResetCredential clears cooldown and exhausted state, e.g. after a top-up or
// after the operator re-authorized the account elsewhere.
func (s *Service) ResetCredential(ctx context.Context, id string) (Credential, error) {
	credential, err := s.credential(ctx, id)
	if err != nil {
		return Credential{}, err
	}
	credential.Status = StatusUnknown
	credential.Disabled = false
	credential.CooldownUntil = 0
	credential.ResetsAt = 0
	credential.LastError = ""
	credential.UpdatedAt = s.now()
	if err := s.saveCredential(ctx, credential); err != nil {
		return Credential{}, err
	}
	return publicCredential(credential), nil
}

// RecordCredentialOutcome persists what one upstream call taught the pool.
// status is the credential state after the call; cooldownUntil and resetsAt are
// Unix seconds, zero when unknown.
func (s *Service) RecordCredentialOutcome(
	ctx context.Context,
	id string,
	status Status,
	cooldownUntil, resetsAt int64,
	reason string,
) error {
	if !status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	health := CredentialHealth{
		ID:            id,
		Status:        status,
		LastUsedAt:    s.now(),
		CooldownUntil: cooldownUntil,
		ResetsAt:      resetsAt,
		LastError:     strings.TrimSpace(reason),
	}
	if len(health.LastError) > 512 {
		health.LastError = health.LastError[:512]
	}
	if status == StatusOK {
		health.CooldownUntil = 0
		health.ResetsAt = 0
		health.LastError = ""
	}
	return s.Store.UpdateCredentialHealth(ctx, health)
}

// FindCredentialByAccount looks an account up by its label or account string.
// Import uses it to avoid adding the same sign-in twice.
func (s *Service) FindCredentialByAccount(ctx context.Context, providerID, account string) (Credential, bool, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return Credential{}, false, nil
	}
	list, err := s.Store.ListCredentials(ctx, providerID)
	if err != nil {
		return Credential{}, false, err
	}
	for _, credential := range list {
		if strings.EqualFold(credential.Account, account) || strings.EqualFold(credential.Label, account) {
			return credential, true, nil
		}
	}
	return Credential{}, false, nil
}

func (s *Service) credential(ctx context.Context, id string) (Credential, error) {
	list, err := s.Store.ListCredentials(ctx, "")
	if err != nil {
		return Credential{}, err
	}
	for _, credential := range list {
		if credential.ID == id {
			return credential, nil
		}
	}
	return Credential{}, ErrCredentialNotFound
}

func (s *Service) saveCredential(ctx context.Context, credential Credential) error {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	return s.Store.SaveCredential(ctx, credential)
}
