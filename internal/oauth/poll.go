package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PollConfig models a vendor sign-in that has no standard OAuth endpoints: the
// console asks for a state, opens the returned URL, and polls until a token
// appears. Some vendors use this instead of a device code, so the flow stays
// declarative and does not need code per vendor.
type PollConfig struct {
	Kind string

	StateURL   string
	TokenURL   string
	RefreshURL string

	// Field names vary by vendor; empty means the common defaults.
	StateField    string // "state"
	AuthURLField  string // "auth_url"
	TokenField    string // "access_token"
	RefreshField  string // "refresh_token"
	ExpiresField  string // "expires_at"
	StateQuery    string // "state"
	RefreshHeader string // "X-Refresh-Token"

	Client *http.Client
}

func (c PollConfig) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func firstField(payload map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := payload[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func expiryFrom(payload map[string]any, field string) int64 {
	names := []string{"expires_at"}
	if field != "" {
		names = append([]string{field}, names...)
	}
	for _, name := range names {
		switch typed := payload[name].(type) {
		case float64:
			// Milliseconds from some vendors.
			if typed > 1e12 {
				return int64(typed) / 1000
			}
			return int64(typed)
		case string:
			if parsed, err := time.Parse(time.RFC3339, typed); err == nil {
				return parsed.Unix()
			}
		}
	}
	if seconds, ok := payload["expires_in"].(float64); ok && seconds > 0 {
		return time.Now().Unix() + int64(seconds)
	}
	return 0
}

func decodeObject(body []byte) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	// Some vendors wrap the result in "data".
	if data, ok := payload["data"].(map[string]any); ok {
		for key, value := range payload {
			if _, exists := data[key]; !exists && key != "data" {
				data[key] = value
			}
		}
		return data
	}
	return payload
}

// Begin asks the vendor for a sign-in URL and state.
func (c PollConfig) Begin(ctx context.Context) (PollStart, error) {
	if c.StateURL == "" || c.TokenURL == "" {
		return PollStart{}, ErrUnsupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.StateURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return PollStart{}, fmt.Errorf("build sign-in request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return PollStart{}, fmt.Errorf("sign-in request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return PollStart{}, fmt.Errorf("read sign-in response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return PollStart{}, fmt.Errorf("sign-in request: HTTP %d: %s", resp.StatusCode, excerpt(body))
	}

	payload := decodeObject(body)
	if payload == nil {
		return PollStart{}, fmt.Errorf("decode sign-in response: %s", excerpt(body))
	}
	stateField := c.StateField
	if stateField == "" {
		stateField = "state"
	}
	authField := c.AuthURLField
	if authField == "" {
		authField = "auth_url"
	}
	state := firstField(payload, stateField, "state", "sid")
	if state == "" {
		return PollStart{}, fmt.Errorf("sign-in response has no state: %s", excerpt(body))
	}
	authURL := firstField(payload, authField, "auth_url", "url", "login_url", "link")
	if authURL == "" {
		return PollStart{}, fmt.Errorf("sign-in response has no url: %s", excerpt(body))
	}
	return PollStart{State: state, AuthURL: authURL}, nil
}

// Poll asks once whether the operator finished. Pending is not an error.
func (c PollConfig) Poll(ctx context.Context, state string) (TokenSet, bool, error) {
	stateQuery := c.StateQuery
	if stateQuery == "" {
		stateQuery = "state"
	}
	parsed, err := url.Parse(c.TokenURL)
	if err != nil {
		return TokenSet{}, false, fmt.Errorf("parse token url: %w", err)
	}
	query := parsed.Query()
	query.Set(stateQuery, state)
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return TokenSet{}, false, fmt.Errorf("build poll request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return TokenSet{}, false, fmt.Errorf("poll request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return TokenSet{}, false, fmt.Errorf("read poll response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A 4xx here usually means "not approved yet" for these vendors.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return TokenSet{}, true, nil
		}
		return TokenSet{}, false, fmt.Errorf("poll request: HTTP %d: %s", resp.StatusCode, excerpt(body))
	}

	payload := decodeObject(body)
	if payload == nil {
		return TokenSet{}, true, nil
	}
	tokenField := c.TokenField
	if tokenField == "" {
		tokenField = "access_token"
	}
	access := firstField(payload, tokenField, "access_token", "token", "access")
	if access == "" {
		return TokenSet{}, true, nil
	}
	refreshField := c.RefreshField
	if refreshField == "" {
		refreshField = "refresh_token"
	}
	return TokenSet{
		AccessToken:  access,
		RefreshToken: firstField(payload, refreshField, "refresh_token", "refresh"),
		TokenType:    "Bearer",
		ExpiresAt:    expiryFrom(payload, c.ExpiresField),
	}, false, nil
}

// Refresh renews a poll token.
func (c PollConfig) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	if c.RefreshURL == "" {
		return TokenSet{}, ErrUnsupported
	}
	header := c.RefreshHeader
	if header == "" {
		header = "X-Refresh-Token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RefreshURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return TokenSet{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(header, refreshToken)

	resp, err := c.client().Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return TokenSet{}, fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return TokenSet{}, fmt.Errorf("%w: HTTP %d: %s", ErrExchange, resp.StatusCode, excerpt(body))
	}
	payload := decodeObject(body)
	if payload == nil {
		return TokenSet{}, fmt.Errorf("%w: decode refresh response", ErrExchange)
	}
	tokenField := c.TokenField
	if tokenField == "" {
		tokenField = "access_token"
	}
	access := firstField(payload, tokenField, "access_token", "token", "access")
	if access == "" {
		return TokenSet{}, fmt.Errorf("%w: refresh returned no token", ErrExchange)
	}
	refreshField := c.RefreshField
	if refreshField == "" {
		refreshField = "refresh_token"
	}
	refreshed := firstField(payload, refreshField, "refresh_token", "refresh")
	if refreshed == "" {
		refreshed = refreshToken
	}
	return TokenSet{
		AccessToken:  access,
		RefreshToken: refreshed,
		TokenType:    "Bearer",
		ExpiresAt:    expiryFrom(payload, c.ExpiresField),
	}, nil
}

// PollStart is what the console opens.
type PollStart struct {
	State   string
	AuthURL string
}

// PollRegistry holds the poll flows janus knows.
type PollRegistry struct {
	configs map[string]PollConfig
}

// NewPollRegistry returns an empty registry.
func NewPollRegistry() *PollRegistry {
	return &PollRegistry{configs: map[string]PollConfig{}}
}

// Get looks up a flow by channel id.
func (r *PollRegistry) Get(kind string) (PollConfig, bool) {
	if r == nil {
		return PollConfig{}, false
	}
	config, ok := r.configs[strings.ToLower(strings.TrimSpace(kind))]
	return config, ok
}

// Set registers or replaces a flow.
func (r *PollRegistry) Set(config PollConfig) {
	if r.configs == nil {
		r.configs = map[string]PollConfig{}
	}
	r.configs[strings.ToLower(strings.TrimSpace(config.Kind))] = config
}
