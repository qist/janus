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

// Complete sends one chat completion. Errors carry the status code and a short
// body excerpt, never the API key.
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for name, value := range c.Headers {
		httpReq.Header.Set(name, value)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("gateway %s: %w", c.Name, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("gateway %s: read response: %w", c.Name, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, fmt.Errorf("gateway %s: HTTP %d: %s", c.Name, resp.StatusCode, excerpt(raw))
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("gateway %s: decode response: %w", c.Name, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return Response{}, fmt.Errorf("gateway %s: %s: %s", c.Name, parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("gateway %s: response has no choices", c.Name)
	}
	text := parsed.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("gateway %s: empty completion", c.Name)
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
