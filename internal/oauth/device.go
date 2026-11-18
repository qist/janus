package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Device flow errors. They are values, not transport failures: the console polls
// until one of them means "stop".
var (
	ErrDevicePending  = errors.New("authorization pending")
	ErrDeviceSlowDown = errors.New("poll slower")
	ErrDeviceExpired  = errors.New("device code expired")
	ErrDeviceDenied   = errors.New("authorization denied")
	ErrDeviceExchange = errors.New("token exchange failed")
)

// DeviceConfig is one vendor's device-authorization flow plus the exchange that
// turns the long-lived token into the short-lived one the API accepts.
type DeviceConfig struct {
	// Kind is the channel id this flow belongs to.
	Kind string

	ClientID  string
	DeviceURL string
	TokenURL  string
	Scopes    []string

	// ExchangeURL obtains a short-lived API token from the long-lived one.
	// Empty means the access token is used as-is.
	ExchangeURL string
	// ExchangeHeaders are vendor-required (Editor-Version, User-Agent, …).
	ExchangeHeaders map[string]string
	// UserURL, when set, resolves a human account name for the credential.
	UserURL string

	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// DeviceCode is what the console shows the operator.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (c DeviceConfig) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Start asks the vendor for a device code.
func (c DeviceConfig) Start(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(c.ClientID) == "" || c.DeviceURL == "" || c.TokenURL == "" {
		return DeviceCode{}, ErrUnsupported
	}
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	if len(c.Scopes) > 0 {
		form.Set("scope", strings.Join(c.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.DeviceURL, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceCode{}, fmt.Errorf("build device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return DeviceCode{}, fmt.Errorf("device request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return DeviceCode{}, fmt.Errorf("read device response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return DeviceCode{}, fmt.Errorf("device request: HTTP %d: %s", resp.StatusCode, excerpt(body))
	}

	var parsed DeviceCode
	if err := json.Unmarshal(body, &parsed); err != nil {
		return DeviceCode{}, fmt.Errorf("decode device response: %w", err)
	}
	if parsed.DeviceCode == "" || parsed.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("device response missing codes: %s", excerpt(body))
	}
	if parsed.Interval <= 0 {
		parsed.Interval = 5
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 900
	}
	return parsed, nil
}

// Poll asks once whether the operator finished authorizing. Callers repeat on
// ErrDevicePending at the returned interval.
func (c DeviceConfig) Poll(ctx context.Context, deviceCode string) (TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, fmt.Errorf("build poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("poll request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return TokenSet{}, fmt.Errorf("read poll response: %w", err)
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return TokenSet{}, fmt.Errorf("decode poll response: %w", err)
	}
	if parsed.Error != "" {
		switch parsed.Error {
		case "authorization_pending":
			return TokenSet{}, ErrDevicePending
		case "slow_down":
			return TokenSet{}, ErrDeviceSlowDown
		case "expired_token":
			return TokenSet{}, ErrDeviceExpired
		case "access_denied":
			return TokenSet{}, ErrDeviceDenied
		default:
			return TokenSet{}, fmt.Errorf("device authorization: %s", parsed.Error)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || parsed.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("poll request: HTTP %d: %s", resp.StatusCode, excerpt(body))
	}
	set := TokenSet{AccessToken: parsed.AccessToken, TokenType: parsed.TokenType, Scope: parsed.Scope}
	if parsed.ExpiresIn > 0 {
		set.ExpiresAt = time.Now().Unix() + int64(parsed.ExpiresIn)
	}
	return set, nil
}

// Exchange turns the long-lived token into the short-lived API token. Vendors
// that accept the token directly have no ExchangeURL and get the input back.
func (c DeviceConfig) Exchange(ctx context.Context, accessToken string) (string, int64, error) {
	if strings.TrimSpace(c.ExchangeURL) == "" {
		return accessToken, 0, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ExchangeURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build exchange request: %w", err)
	}
	// GitHub expects "token", most others "Bearer".
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/json")
	for name, value := range c.ExchangeHeaders {
		req.Header.Set(name, value)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s", ErrDeviceExchange, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", 0, fmt.Errorf("read exchange response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, fmt.Errorf("%w: HTTP %d: %s", ErrDeviceExchange, resp.StatusCode, excerpt(body))
	}

	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("%w: decode: %s", ErrDeviceExchange, err)
	}
	if parsed.Token == "" {
		return "", 0, fmt.Errorf("%w: empty token", ErrDeviceExchange)
	}
	// Some vendors report seconds, others milliseconds.
	if parsed.ExpiresAt > 1e12 {
		parsed.ExpiresAt /= 1000
	}
	return parsed.Token, parsed.ExpiresAt, nil
}

// Login resolves the account name shown in the console.
func (c DeviceConfig) Login(ctx context.Context, accessToken string) string {
	if c.UserURL == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UserURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "janus")
	resp, err := c.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ""
	}
	var parsed struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Login)
}

// DeviceRegistry holds the device flows janus knows.
type DeviceRegistry struct {
	configs map[string]DeviceConfig
}

// NewDeviceRegistry returns an empty registry.
func NewDeviceRegistry() *DeviceRegistry {
	return &DeviceRegistry{configs: map[string]DeviceConfig{}}
}

// DefaultDeviceRegistry contains the flows that are documented and stable.
func DefaultDeviceRegistry() *DeviceRegistry {
	registry := NewDeviceRegistry()
	registry.Set(DeviceConfig{
		Kind:      "github-copilot",
		ClientID:  "Iv1.b507a08c87ecfe98", // GitHub Copilot plugin (public client)
		DeviceURL: "https://github.com/login/device/code",
		TokenURL:  "https://github.com/login/oauth/access_token",
		Scopes:    []string{"read:user"},
		// The Copilot API rejects a plain GitHub token; this exchange returns a
		// short-lived token bound to the account's subscription.
		ExchangeURL: "https://api.github.com/copilot_internal/v2/token",
		ExchangeHeaders: map[string]string{
			"Editor-Version":         "vscode/1.95.0",
			"Copilot-Integration-Id": "vscode-chat",
			"User-Agent":             "GitHubCopilotChat/0.23.2",
		},
		UserURL: "https://api.github.com/user",
	})
	return registry
}

// Get looks up a flow by channel id.
func (r *DeviceRegistry) Get(kind string) (DeviceConfig, bool) {
	if r == nil {
		return DeviceConfig{}, false
	}
	cfg, ok := r.configs[strings.ToLower(strings.TrimSpace(kind))]
	return cfg, ok
}

// Set registers or replaces a flow.
func (r *DeviceRegistry) Set(cfg DeviceConfig) {
	if r.configs == nil {
		r.configs = map[string]DeviceConfig{}
	}
	r.configs[strings.ToLower(strings.TrimSpace(cfg.Kind))] = cfg
}
