// Package oauth implements the authorization-code + PKCE flows used by coding
// subscriptions (ChatGPT/Codex, Claude). It deliberately stops at tokens: how a
// token is used upstream belongs to the gateway, and how it is stored belongs to
// the provider service.
//
// Both flows finish by pasting a code back into the console. That is not a
// shortcut: Claude redirects to a hosted page that shows a code, and OpenAI
// redirects to localhost on the machine running the flow, which a remote janus
// cannot receive. Paste works identically for a local and a remote deployment.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSet is what a token endpoint returns, in the form janus stores.
type TokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

// Valid reports whether the set carries something usable.
func (t TokenSet) Valid() bool { return strings.TrimSpace(t.AccessToken) != "" }

// Expired reports whether the access token needs refreshing. A set without an
// expiry is treated as long-lived.
func (t TokenSet) Expired(now int64, skew int64) bool {
	return t.ExpiresAt > 0 && now >= t.ExpiresAt-skew
}

// Config describes one vendor's OAuth client.
type Config struct {
	// Kind names the flow, e.g. "openai-codex" or "claude-code". It is also
	// the catalogue channel the registry is keyed by.
	Kind         string
	ClientID     string
	AuthorizeURL string
	TokenURL     string
	RedirectURI  string
	Scopes       []string
	// VerifierBytes is the raw randomness before encoding.
	VerifierBytes int
	// VerifierEncoding is "hex" (OpenAI/Codex) or "base64url" (RFC default).
	VerifierEncoding string
	// TokenStyle is "form" (OpenAI) or "json" (Claude).
	TokenStyle string
	// ExtraAuthParams are appended to the authorize URL.
	ExtraAuthParams map[string]string
}

// Registry holds the supported flows. Tests replace entries to point at fakes.
type Registry struct {
	configs map[string]Config
}

// DefaultRegistry returns the clients used by the official CLIs.
//
// Two presets only. Other coding subscriptions (Cursor, Kiro, Qoder,
// WorkBuddy, ZCode, …) are served by third-party OpenCode plugins that carry
// their own client ids; shipping those here would mean shipping someone else's
// client and taking on their account-ban risk. janus instead exposes a custom
// PKCE flow per provider (Provider.OAuth), so an operator can bring their own.
func DefaultRegistry() *Registry {
	codex := Config{
		Kind:             "openai-codex",
		ClientID:         "app_EMoamEEZ73f0CkXaXp7hrann", // Codex CLI
		AuthorizeURL:     "https://auth.openai.com/oauth/authorize",
		TokenURL:         "https://auth.openai.com/oauth/token",
		RedirectURI:      "http://localhost:1455/auth/callback",
		Scopes:           []string{"openid", "profile", "email", "offline_access"},
		VerifierBytes:    64,
		VerifierEncoding: "hex",
		TokenStyle:       "form",
	}
	claude := Config{
		Kind:         "claude-code",
		ClientID:     "9d1c250a-e61b-44d9-88ed-5944d1962f5e", // Claude Code
		AuthorizeURL: "https://claude.com/cai/oauth/authorize",
		TokenURL:     "https://platform.claude.com/v1/oauth/token",
		RedirectURI:  "https://platform.claude.com/oauth/code/callback",
		Scopes: []string{
			"org:create_api_key",
			"user:profile",
			"user:inference",
			"user:sessions:claude_code",
			"user:mcp_servers",
			"user:file_upload",
		},
		VerifierBytes:    32,
		VerifierEncoding: "base64url",
		TokenStyle:       "json",
	}
	// The bare protocol names are aliases for providers created before the
	// catalogue existed. They are separate entries, not a second lookup, so
	// Registry.Set on an alias replaces only that alias.
	openaiAlias := codex
	openaiAlias.Kind = "openai"
	anthropicAlias := claude
	anthropicAlias.Kind = "anthropic"
	return &Registry{configs: map[string]Config{
		"openai-codex": codex,
		"openai":       openaiAlias,
		"claude-code":  claude,
		"anthropic":    anthropicAlias,
	}}
}

// Get returns the flow for a channel, or for a protocol kind when no channel
// preset exists.
func (r *Registry) Get(kind string) (Config, bool) {
	if r == nil {
		return Config{}, false
	}
	config, ok := r.configs[strings.ToLower(strings.TrimSpace(kind))]
	return config, ok
}

// Set replaces one entry; tests use it to point at a local token server.
func (r *Registry) Set(config Config) {
	if r == nil {
		return
	}
	if r.configs == nil {
		r.configs = make(map[string]Config)
	}
	r.configs[config.Kind] = config
}

// ErrUnsupported means the provider kind has no OAuth client.
var ErrUnsupported = errors.New("oauth is not supported for this provider kind")

// ErrExchange means the vendor rejected the code or refresh.
var ErrExchange = errors.New("oauth token exchange failed")

// NewVerifier returns a code verifier for cfg.
func NewVerifier(cfg Config) (string, error) {
	size := cfg.VerifierBytes
	if size <= 0 {
		size = 32
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate verifier: %w", err)
	}
	switch cfg.VerifierEncoding {
	case "hex":
		return hex.EncodeToString(buf), nil
	default:
		return base64.RawURLEncoding.EncodeToString(buf), nil
	}
}

// Challenge is S256(verifier), independent of how the verifier was encoded.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState returns a random state value.
func NewState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// AuthURL builds the vendor's authorize URL for a state and verifier.
func (c Config) AuthURL(state, verifier string) (string, error) {
	if c.AuthorizeURL == "" || c.ClientID == "" {
		return "", ErrUnsupported
	}
	parsed, err := url.Parse(c.AuthorizeURL)
	if err != nil {
		return "", fmt.Errorf("parse authorize url: %w", err)
	}
	query := parsed.Query()
	query.Set("client_id", c.ClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", c.RedirectURI)
	query.Set("scope", strings.Join(c.Scopes, " "))
	query.Set("state", state)
	query.Set("code_challenge", Challenge(verifier))
	query.Set("code_challenge_method", "S256")
	for key, value := range c.ExtraAuthParams {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	ExpiresIn        int64  `json:"expires_in"`
	IDToken          string `json:"id_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange trades an authorization code for tokens.
func (c Config) Exchange(ctx context.Context, code, verifier string) (TokenSet, error) {
	values := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     c.ClientID,
		"code":          strings.TrimSpace(code),
		"redirect_uri":  c.RedirectURI,
		"code_verifier": verifier,
	}
	return c.postToken(ctx, values)
}

// Refresh trades a refresh token for a new access token.
func (c Config) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	values := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     c.ClientID,
		"refresh_token": strings.TrimSpace(refreshToken),
	}
	return c.postToken(ctx, values)
}

func (c Config) postToken(ctx context.Context, values map[string]string) (TokenSet, error) {
	if c.TokenURL == "" {
		return TokenSet{}, ErrUnsupported
	}

	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	if c.TokenStyle == "json" {
		encoded, err := json.Marshal(values)
		if err != nil {
			return TokenSet{}, fmt.Errorf("encode token request: %w", err)
		}
		body = strings.NewReader(string(encoded))
		contentType = "application/json"
	} else {
		form := url.Values{}
		for key, value := range values {
			form.Set(key, value)
		}
		body = strings.NewReader(form.Encode())
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.TokenURL, body)
	if err != nil {
		return TokenSet{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("%w: %v", ErrExchange, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed tokenResponse
	_ = json.Unmarshal(raw, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := parsed.ErrorDescription
		if detail == "" {
			detail = parsed.Error
		}
		if detail == "" {
			detail = excerpt(raw)
		}
		return TokenSet{}, fmt.Errorf("%w: HTTP %d: %s", ErrExchange, resp.StatusCode, detail)
	}
	if parsed.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("%w: no access_token in response", ErrExchange)
	}

	set := TokenSet{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		TokenType:    parsed.TokenType,
		Scope:        parsed.Scope,
		IDToken:      parsed.IDToken,
	}
	if parsed.ExpiresIn > 0 {
		set.ExpiresAt = time.Now().Unix() + parsed.ExpiresIn
	}
	return set, nil
}

func excerpt(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}
