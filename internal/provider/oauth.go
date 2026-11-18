package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	// TargetCredentialID, when set, means "re-authorize this account" rather
	// than "add another account".
	TargetCredentialID string
}

// OAuthStart is what the console needs to open the vendor's page.
type OAuthStart struct {
	SessionID    string `json:"session_id"`
	AuthURL      string `json:"auth_url"`
	RedirectURI  string `json:"redirect_uri"`
	ExpiresIn    int64  `json:"expires_in"`
	CredentialID string `json:"credential_id,omitempty"`
}

type oauthSessions struct {
	mu   sync.Mutex
	data map[string]OAuthSession
}

// StartOAuth creates a PKCE session for a provider and returns the vendor URL.
// A non-empty credentialID starts a re-authorization of that account.
func (s *Service) StartOAuth(ctx context.Context, providerID, credentialID string) (OAuthStart, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return OAuthStart{}, err
	}
	cfg, ok := s.oauthConfig(p)
	if !ok {
		return OAuthStart{}, fmt.Errorf("%w: %s", ErrOAuthUnsupported, p.ChannelOrKind())
	}
	if credentialID != "" {
		existing, err := s.credential(ctx, credentialID)
		if err != nil {
			return OAuthStart{}, err
		}
		if existing.ProviderID != p.ID {
			return OAuthStart{}, fmt.Errorf("%w: credential belongs to another provider", ErrInvalidInput)
		}
		if existing.Kind != CredentialOAuth {
			return OAuthStart{}, fmt.Errorf("%w: only oauth credentials can be re-authorized", ErrInvalidInput)
		}
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
		ID:                 newOAuthSessionID(),
		ProviderID:         p.ID,
		Kind:               cfg.Kind,
		Verifier:           verifier,
		State:              state,
		CreatedAt:          s.now(),
		TargetCredentialID: strings.TrimSpace(credentialID),
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
		SessionID:    session.ID,
		AuthURL:      authURL,
		RedirectURI:  cfg.RedirectURI,
		ExpiresIn:    int64(OAuthSessionTTL / time.Second),
		CredentialID: session.TargetCredentialID,
	}, nil
}

// CompleteOAuth finishes a session with the code (or the whole callback URL)
// the vendor showed the operator. label is an optional operator note; when it
// is empty the identity from the token decides.
func (s *Service) CompleteOAuth(ctx context.Context, providerID, sessionID, input, label string) (Credential, error) {
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

	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return Credential{}, err
	}
	cfg, ok := s.oauthConfig(p)
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

	account, plan := tokenIdentity(tokens)
	label = strings.TrimSpace(label)
	if label == "" {
		label = account
	}

	if session.TargetCredentialID != "" {
		updated, err := s.replaceTokens(ctx, session.TargetCredentialID, label, account, plan, tokens)
		if err != nil {
			return Credential{}, err
		}
		return publicCredential(updated), nil
	}
	credential, err := s.storeTokens(ctx, providerID, label, account, plan, tokens)
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

// storeTokens seals a token set and writes it as a new oauth credential.
func (s *Service) storeTokens(ctx context.Context, providerID, label, account, plan string, tokens oauth.TokenSet) (Credential, error) {
	sealed, err := s.sealTokens(tokens)
	if err != nil {
		return Credential{}, err
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
		Label:            strings.TrimSpace(label),
		Kind:             CredentialOAuth,
		Status:           StatusOK,
		Masked:           Mask(tokens.AccessToken),
		HasSecret:        true,
		Priority:         priority,
		Account:          account,
		Plan:             plan,
		ExpiresAt:        tokens.ExpiresAt,
		SecretCiphertext: sealed,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.saveCredential(ctx, credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// replaceTokens rewrites an existing account after a re-authorization. The
// priority and label the operator set are kept unless a new label is given.
func (s *Service) replaceTokens(ctx context.Context, id, label, account, plan string, tokens oauth.TokenSet) (Credential, error) {
	credential, err := s.credential(ctx, id)
	if err != nil {
		return Credential{}, err
	}
	sealed, err := s.sealTokens(tokens)
	if err != nil {
		return Credential{}, err
	}
	if strings.TrimSpace(label) != "" {
		credential.Label = strings.TrimSpace(label)
	}
	if account != "" {
		credential.Account = account
	}
	if plan != "" {
		credential.Plan = plan
	}
	credential.SecretCiphertext = sealed
	credential.Masked = Mask(tokens.AccessToken)
	credential.ExpiresAt = tokens.ExpiresAt
	credential.Status = StatusOK
	credential.Disabled = false
	credential.CooldownUntil = 0
	credential.ResetsAt = 0
	credential.LastError = ""
	credential.UpdatedAt = s.now()
	if err := s.saveCredential(ctx, credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// SaveOAuthTokens stores a token set on an existing credential. Import uses
// it; re-authorization goes through replaceTokens so a new account is never
// created by accident.
func (s *Service) SaveOAuthTokens(ctx context.Context, id, account, plan string, tokens oauth.TokenSet) (Credential, error) {
	return s.replaceTokens(ctx, id, "", account, plan, tokens)
}

func (s *Service) sealTokens(tokens oauth.TokenSet) (string, error) {
	if s.Box == nil {
		return "", ErrSecretBox
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return "", fmt.Errorf("encode tokens: %w", err)
	}
	sealed, err := s.Box.Seal(string(encoded))
	if err != nil {
		return "", fmt.Errorf("seal tokens: %w", err)
	}
	return sealed, nil
}

// CreateOAuthCredential stores an imported token set without a sign-in.
func (s *Service) CreateOAuthCredential(ctx context.Context, providerID, label, account, plan string, tokens oauth.TokenSet) (Credential, error) {
	if _, err := s.Store.GetProvider(ctx, providerID); err != nil {
		return Credential{}, err
	}
	if !tokens.Valid() {
		return Credential{}, fmt.Errorf("%w: access token required", ErrInvalidInput)
	}
	credential, err := s.storeTokens(ctx, providerID, label, account, plan, tokens)
	if err != nil {
		return Credential{}, err
	}
	return publicCredential(credential), nil
}

// credentialKey returns the bearer value to use upstream. For OAuth
// credentials it refreshes an expiring access token first and persists the new
// set; plain API keys come back unchanged.
func (s *Service) credentialKey(ctx context.Context, p Provider, credential Credential) (string, error) {
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
		if cfg, ok := s.oauthConfig(p); ok {
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
	sealed, err := s.sealTokens(tokens)
	if err != nil {
		return
	}
	credential.SecretCiphertext = sealed
	credential.ExpiresAt = tokens.ExpiresAt
	credential.Masked = Mask(tokens.AccessToken)
	credential.HasSecret = true
	credential.Status = StatusOK
	credential.UpdatedAt = s.now()
	_ = s.saveCredential(ctx, credential)
}

// oauthConfig prefers an operator-supplied flow, then the catalogue channel,
// then the protocol kind. The channel is what distinguishes Codex from a plain
// OpenAI key provider on the same host.
func (s *Service) oauthConfig(p Provider) (oauth.Config, bool) {
	if p.OAuth != nil {
		return configFromOAuth(*p.OAuth), true
	}
	registry := s.oauthRegistry()
	if cfg, ok := registry.Get(p.Channel); ok {
		return cfg, true
	}
	return registry.Get(string(p.Kind))
}

func configFromOAuth(c OAuthConfig) oauth.Config {
	return oauth.Config{
		Kind:             "custom",
		ClientID:         c.ClientID,
		AuthorizeURL:     c.AuthorizeURL,
		TokenURL:         c.TokenURL,
		RedirectURI:      c.RedirectURI,
		Scopes:           c.Scopes,
		ExtraAuthParams:  c.ExtraAuthParams,
		TokenStyle:       c.TokenStyle,
		VerifierBytes:    c.VerifierBytes,
		VerifierEncoding: c.VerifierEncoding,
	}
}

func (s *Service) oauthRegistry() *oauth.Registry {
	if s.OAuth != nil {
		return s.OAuth
	}
	return oauth.DefaultRegistry()
}

// ChannelOrKind is what an error message should name.
func (p Provider) ChannelOrKind() string {
	if p.Channel != "" {
		return p.Channel
	}
	return string(p.Kind)
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

// tokenIdentity reads the identity claims vendors put in the id_token (or, for
// some, the access token). A token without usable claims is not an error: the
// operator can still label the account by hand.
func tokenIdentity(tokens oauth.TokenSet) (account, plan string) {
	for _, raw := range []string{tokens.IDToken, tokens.AccessToken} {
		claims := decodeClaims(raw)
		if len(claims) == 0 {
			continue
		}
		if account == "" {
			account = firstString(claims, "email", "preferred_username", "upn", "name")
		}
		if plan == "" {
			plan = planFromClaims(claims)
		}
		if account != "" && plan != "" {
			break
		}
	}
	return account, plan
}

func decodeClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some issuers pad the segment; tolerate both.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func firstString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func planFromClaims(claims map[string]any) string {
	for _, key := range []string{"plan_type", "subscription_type", "subscriptionType", "tier"} {
		if value, ok := claims[key].(string); ok && value != "" {
			return value
		}
	}
	// OpenAI nests the plan under its auth namespace.
	for key, value := range claims {
		if !strings.HasSuffix(key, "/auth") {
			continue
		}
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if plan := firstString(nested, "chatgpt_plan_type", "plan_type"); plan != "" {
			return plan
		}
	}
	return ""
}

func newOAuthSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("oauth_%d", time.Now().UnixNano())
	}
	return "oauth_" + hex.EncodeToString(buf[:])
}
