package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"janus/internal/oauth"
)

// OAuthSessionTTL is how long a started sign-in may be finished.
const OAuthSessionTTL = 10 * time.Minute

// Errors surfaced by the OAuth flow; the API maps them to stable codes.
var (
	ErrOAuthUnsupported = oauth.ErrUnsupported
	ErrOAuthSession     = errors.New("oauth session not found or expired")
	ErrOAuthState       = errors.New("oauth state mismatch")
)

// OAuthSession is one sign-in started from the console. Sessions live in memory:
// they are short-lived and losing them on restart is preferable to persisting a
// code verifier.
type OAuthSession struct {
	ID         string
	ProviderID string
	Kind       string
	Verifier   string
	State      string
	CreatedAt  int64
}

// OAuthStart is what the console needs to open the vendor's page.
type OAuthStart struct {
	SessionID   string `json:"session_id"`
	AuthURL     string `json:"auth_url"`
	RedirectURI string `json:"redirect_uri"`
	ExpiresIn   int64  `json:"expires_in"`
}

type oauthSessions struct {
	mu   sync.Mutex
	data map[string]OAuthSession
}

// StartOAuth creates a PKCE session for a provider and returns the vendor URL.
func (s *Service) StartOAuth(ctx context.Context, providerID string) (OAuthStart, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return OAuthStart{}, err
	}
	cfg, ok := s.oauthRegistry().Get(string(p.Kind))
	if !ok {
		return OAuthStart{}, fmt.Errorf("%w: %s", ErrOAuthUnsupported, p.Kind)
	}

	verifier, err := oauth.NewVerifier(cfg)
	if err != nil {
		return OAuthStart{}, err
	}
	state, err := oauth.NewState()
	if err != nil {
		return OAuthStart{}, err
	}
	authURL, err := cfg.AuthURL(state, verifier)
	if err != nil {
		return OAuthStart{}, err
	}

	session := OAuthSession{
		ID:         newOAuthSessionID(),
		ProviderID: p.ID,
		Kind:       string(p.Kind),
		Verifier:   verifier,
		State:      state,
		CreatedAt:  s.now(),
	}

	s.oauth.mu.Lock()
	if s.oauth.data == nil {
		s.oauth.data = make(map[string]OAuthSession)
	}
	// Drop what has expired; a console left open overnight should not pile up.
	for id, existing := range s.oauth.data {
		if s.now()-existing.CreatedAt > int64(OAuthSessionTTL/time.Second) {
			delete(s.oauth.data, id)
		}
	}
	s.oauth.data[session.ID] = session
	s.oauth.mu.Unlock()

	return OAuthStart{
		SessionID:   session.ID,
		AuthURL:     authURL,
		RedirectURI: cfg.RedirectURI,
		ExpiresIn:   int64(OAuthSessionTTL / time.Second),
	}, nil
}

// CompleteOAuth finishes a session with the code (or the whole callback URL)
// the vendor showed the operator, and stores the tokens as a credential.
func (s *Service) CompleteOAuth(ctx context.Context, providerID, sessionID, input string) (Credential, error) {
	session, ok := s.takeSession(sessionID)
	if !ok || session.ProviderID != providerID {
		return Credential{}, ErrOAuthSession
	}
	if s.now()-session.CreatedAt > int64(OAuthSessionTTL/time.Second) {
		return Credential{}, ErrOAuthSession
	}

	code, state := parseOAuthInput(input)
	if strings.TrimSpace(code) == "" {
		return Credential{}, fmt.Errorf("%w: authorization code required", ErrInvalidInput)
	}
	if state != "" && state != session.State {
		return Credential{}, ErrOAuthState
	}

	cfg, ok := s.oauthRegistry().Get(session.Kind)
	if !ok {
		return Credential{}, fmt.Errorf("%w: %s", ErrOAuthUnsupported, session.Kind)
	}
	tokens, err := cfg.Exchange(ctx, code, session.Verifier)
	if err != nil {
		return Credential{}, err
	}
	if !tokens.Valid() {
		return Credential{}, fmt.Errorf("%w: empty access token", oauth.ErrExchange)
	}

	credential, err := s.storeTokens(ctx, providerID, "oauth", tokens)
	if err != nil {
		return Credential{}, err
	}
	return publicCredential(credential), nil
}

func (s *Service) takeSession(id string) (OAuthSession, bool) {
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()

	session, ok := s.oauth.data[strings.TrimSpace(id)]
	if !ok {
		return OAuthSession{}, false
	}
	// One-shot: a code cannot be replayed even if the operator pastes twice.
	delete(s.oauth.data, id)
	return session, true
}

// storeTokens seals a token set and writes it as an oauth credential.
func (s *Service) storeTokens(ctx context.Context, providerID, label string, tokens oauth.TokenSet) (Credential, error) {
	if s.Box == nil {
		return Credential{}, ErrSecretBox
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return Credential{}, fmt.Errorf("encode tokens: %w", err)
	}
	sealed, err := s.Box.Seal(string(encoded))
	if err != nil {
		return Credential{}, fmt.Errorf("seal tokens: %w", err)
	}

	existing, err := s.Store.ListCredentials(ctx, providerID)
	if err != nil {
		return Credential{}, err
	}
	priority := 0
	for _, item := range existing {
		if item.Priority >= priority {
			priority = item.Priority + 1
		}
	}

	now := s.now()
	credential := Credential{
		ID:               newResourceID("cred"),
		ProviderID:       providerID,
		Label:            label,
		Kind:             CredentialOAuth,
		Status:           StatusOK,
		Masked:           Mask(tokens.AccessToken),
		HasSecret:        true,
		Priority:         priority,
		ExpiresAt:        tokens.ExpiresAt,
		SecretCiphertext: sealed,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.Store.SaveCredential(ctx, credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// credentialKey returns the bearer value to use upstream. For OAuth
// credentials it refreshes an expiring access token first and persists the new
// set; plain API keys come back unchanged.
func (s *Service) credentialKey(ctx context.Context, providerKind string, credential Credential) (string, error) {
	if s.Box == nil {
		return "", ErrSecretBox
	}
	plain, err := s.Box.Open(credential.SecretCiphertext)
	if err != nil {
		return "", err
	}

	var tokens oauth.TokenSet
	if err := json.Unmarshal([]byte(plain), &tokens); err != nil || tokens.AccessToken == "" {
		return plain, nil
	}

	if tokens.RefreshToken != "" && tokens.Expired(s.now(), 120) {
		if cfg, ok := s.oauthRegistry().Get(providerKind); ok {
			refreshed, err := cfg.Refresh(ctx, tokens.RefreshToken)
			if err == nil && refreshed.Valid() {
				if refreshed.RefreshToken == "" {
					refreshed.RefreshToken = tokens.RefreshToken
				}
				s.persistTokens(ctx, credential, refreshed)
				return refreshed.AccessToken, nil
			}
		}
	}
	return tokens.AccessToken, nil
}

// persistTokens rewrites a credential in place after a refresh. Best effort: a
// failed write only means the next request refreshes again.
func (s *Service) persistTokens(ctx context.Context, credential Credential, tokens oauth.TokenSet) {
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return
	}
	sealed, err := s.Box.Seal(string(encoded))
	if err != nil {
		return
	}
	credential.SecretCiphertext = sealed
	credential.ExpiresAt = tokens.ExpiresAt
	credential.Masked = Mask(tokens.AccessToken)
	credential.HasSecret = true
	credential.Status = StatusOK
	credential.UpdatedAt = s.now()
	_ = s.Store.SaveCredential(ctx, credential)
}

func (s *Service) oauthRegistry() *oauth.Registry {
	if s.OAuth != nil {
		return s.OAuth
	}
	return oauth.DefaultRegistry()
}

// parseOAuthInput accepts a bare code, "code#state" (what Claude's page shows)
// or the full callback URL, and returns the code and state.
func parseOAuthInput(input string) (code, state string) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", ""
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		parsed, err := url.Parse(value)
		if err == nil {
			query := parsed.Query()
			return query.Get("code"), query.Get("state")
		}
	}
	if before, after, found := strings.Cut(value, "#"); found {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	return value, ""
}

func newOAuthSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("oauth_%d", time.Now().UnixNano())
	}
	return "oauth_" + hex.EncodeToString(buf[:])
}
