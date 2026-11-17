// Package gateway is the model plane. This first slice is deliberately small:
// one OpenAI-compatible chat client that any provider exposing
// POST /chat/completions can serve (OpenAI, DeepSeek, Moonshot, vLLM, Ollama).
// Routing, fallback and account pools come later; everything here is one model
// per client so callers can compose several for independence.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config describes one provider endpoint.
type Config struct {
	// Name labels this client in audit records, e.g. "openai".
	Name string `json:"name"`
	// BaseURL is the API root, e.g. https://api.openai.com/v1.
	BaseURL string `json:"base_url"`
	// APIKey is the bearer token. Prefer APIKeyEnv so secrets stay out of
	// config files and process listings.
	APIKey string `json:"api_key,omitempty"`
	// APIKeyEnv names the environment variable holding the key.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// APIKeyEnvs lists several keys for the same model. The client rotates to
	// the next one only on quota, rate-limit or auth failures, never for load
	// balancing: switching costs a cold prompt cache.
	APIKeyEnvs []string `json:"api_key_envs,omitempty"`
	// Model is the default model for requests that do not override it.
	Model string `json:"model"`
	// TimeoutSeconds bounds one HTTP call; default 30.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Headers adds provider-specific headers (e.g. anthropic-version).
	Headers map[string]string `json:"headers,omitempty"`
	// MaxTokens caps the completion. Zero means the provider default.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature defaults to 0: audits should be deterministic.
	Temperature *float64 `json:"temperature,omitempty"`
}

// Request is one chat completion.
type Request struct {
	System      string
	User        string
	Model       string
	MaxTokens   int
	Temperature *float64
}

// Response is the text plus what the provider reported.
type Response struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// Usage is what one successful call consumed.
type Usage struct {
	ProviderID       string
	CredentialID     string
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// Credential is one upstream key.
type Credential struct {
	ID     string
	APIKey string
}

// CredentialSource hands out keys and records what happened to them. The pool
// package implements the policy; this interface only keeps the client honest.
type CredentialSource interface {
	Next(ctx context.Context, provider, model string) (Credential, error)
	// Report receives the HTTP status (0 for a transport failure), a short body
	// excerpt, and any Retry-After hint.
	Report(ctx context.Context, credentialID string, status int, body string, retryAfter time.Duration)
}

// maxCredentialAttempts bounds failover so one request cannot walk the entire
// pool in a hot loop.
const maxCredentialAttempts = 3

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	Name        string
	BaseURL     string
	APIKey      string
	Model       string
	Headers     map[string]string
	MaxTokens   int
	Temperature *float64
	HTTP        *http.Client
	// Credentials, when set, overrides APIKey and enables failover.
	Credentials CredentialSource
	// OnUsage receives every successful call for the usage ledger.
	OnUsage func(Usage)
}

// New validates cfg and returns a client.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("gateway base_url required")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("gateway base_url must be http(s): %q", cfg.BaseURL)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("gateway model required")
	}

	key := strings.TrimSpace(cfg.APIKey)
	if env := strings.TrimSpace(cfg.APIKeyEnv); env != "" {
		value := strings.TrimSpace(os.Getenv(env))
		if value == "" {
			return nil, fmt.Errorf("gateway api_key_env %q is empty", env)
		}
		key = value
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = model
	}

	return &Client{
		Name:        name,
		BaseURL:     base,
		APIKey:      key,
		Model:       model,
		Headers:     cfg.Headers,
		MaxTokens:   cfg.MaxTokens,
		Temperature: cfg.Temperature,
		HTTP:        &http.Client{Timeout: timeout},
	}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete sends one chat completion. With Credentials set it may try up to
// three keys, but only when the failure is specific to the account.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.Model
	}

	payload := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if payload.MaxTokens == 0 {
		payload.MaxTokens = c.MaxTokens
	}
	if payload.Temperature == nil {
		payload.Temperature = c.Temperature
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("encode chat request: %w", err)
	}

	attempts := 1
	if c.Credentials != nil {
		attempts = maxCredentialAttempts
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		key := c.APIKey
		credentialID := ""
		if c.Credentials != nil {
			credential, err := c.Credentials.Next(ctx, c.Name, model)
			if err != nil {
				if lastErr != nil {
					return Response{}, fmt.Errorf("gateway %s: %w (last error: %v)", c.Name, err, lastErr)
				}
				return Response{}, fmt.Errorf("gateway %s: %w", c.Name, err)
			}
			credentialID = credential.ID
			if credential.APIKey != "" {
				key = credential.APIKey
			}
		}

		status, retryAfter, raw, err := c.roundTrip(ctx, body, key)
		if err != nil {
			c.report(credentialID, 0, err.Error(), 0)
			return Response{}, fmt.Errorf("gateway %s: %w", c.Name, err)
		}

		if status < 200 || status > 299 {
			c.report(credentialID, status, string(raw), retryAfter)
			lastErr = fmt.Errorf("HTTP %d: %s", status, excerpt(raw))
			if switchable(status) && attempt+1 < attempts {
				continue
			}
			return Response{}, fmt.Errorf("gateway %s: %w", c.Name, lastErr)
		}

		c.report(credentialID, status, "", 0)
		response, err := parseResponse(raw, model, c.Name)
		if err == nil && c.OnUsage != nil {
			c.OnUsage(Usage{
				ProviderID:       c.Name,
				CredentialID:     credentialID,
				Model:            response.Model,
				PromptTokens:     response.PromptTokens,
				CompletionTokens: response.CompletionTokens,
			})
		}
		return response, err
	}

	return Response{}, fmt.Errorf("gateway %s: %w", c.Name, lastErr)
}

// switchable reports whether another account could plausibly succeed where this
// one failed. 5xx and 4xx client errors are not account problems.
func switchable(status int) bool {
	switch status {
	case 401, 402, 403, 429:
		return true
	}
	return false
}

func (c *Client) report(credentialID string, status int, body string, retryAfter time.Duration) {
	if c.Credentials == nil || credentialID == "" {
		return
	}
	c.Credentials.Report(context.Background(), credentialID, status, body, retryAfter)
}

// roundTrip performs one HTTP call and returns status, body and any Retry-After.
func (c *Client) roundTrip(ctx context.Context, body []byte, key string) (int, time.Duration, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range c.Headers {
		httpReq.Header.Set(name, value)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return 0, 0, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, 0, nil, fmt.Errorf("read response: %w", err)
	}

	var retryAfter time.Duration
	if value := strings.TrimSpace(resp.Header.Get("Retry-After")); value != "" {
		if seconds, err := time.ParseDuration(value + "s"); err == nil {
			retryAfter = seconds
		}
	}
	return resp.StatusCode, retryAfter, raw, nil
}

func parseResponse(raw []byte, model, name string) (Response, error) {
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("gateway %s: decode response: %w", name, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return Response{}, fmt.Errorf("gateway %s: %s: %s", name, parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("gateway %s: response has no choices", name)
	}
	text := parsed.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("gateway %s: empty completion", name)
	}

	modelName := parsed.Model
	if modelName == "" {
		modelName = model
	}
	return Response{
		Text:             text,
		Model:            modelName,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
	}, nil
}

// excerpt keeps error bodies readable without dumping a megabyte into logs.
func excerpt(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}
