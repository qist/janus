// Package gateway is the model plane. This slice is deliberately small: one
// OpenAI-compatible chat client that any provider exposing
// POST /chat/completions can serve (OpenAI, DeepSeek, Moonshot, vLLM, Ollama).
// Routing groups, fallback models and account pools come later.
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
	"sync"
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
	// Model is the upstream model name.
	Model string `json:"model"`
	// Aliases are extra names the proxy accepts for this model.
	Aliases []string `json:"aliases,omitempty"`
	// TimeoutSeconds bounds one HTTP call; default 30.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Headers adds provider-specific headers (e.g. anthropic-version).
	Headers map[string]string `json:"headers,omitempty"`
	// Protocol is what the upstream speaks: "chat" (default), "responses"
	// (OpenAI Responses / Codex) or "anthropic" (Messages). Callers always use
	// chat completions; the translation happens in this package.
	Protocol string `json:"protocol,omitempty"`
	// MaxTokens caps the completion. Zero means the provider default.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature defaults to 0: audits should be deterministic.
	Temperature *float64 `json:"temperature,omitempty"`
}

// Request is one chat completion built from parts.
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
	// CachedTokens are prompt tokens the upstream served from a cache. They
	// still count toward the prompt total but are billed differently.
	CachedTokens int
}

// Credential is one upstream key or OAuth grant. OAuth changes both the header
// shape and, for Anthropic, the accepted token type.
type Credential struct {
	ID     string
	APIKey string
	OAuth  bool
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

// Client talks to one upstream endpoint. Callers always send chat completions;
// Protocol says how the upstream wants them translated.
type Client struct {
	Name        string
	BaseURL     string
	APIKey      string
	Model       string
	Protocol    Protocol
	Headers     map[string]string
	MaxTokens   int
	Temperature *float64
	HTTP        *http.Client
	// Credentials, when set, overrides APIKey and enables failover.
	Credentials CredentialSource
	// OnUsage receives every successful call for the usage ledger.
	OnUsage func(Usage)

	// projects caches the Code Assist project id per credential.
	projectMu sync.Mutex
	projects  map[string]cachedProject
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
	protocol, ok := ParseProtocol(cfg.Protocol)
	if !ok {
		return nil, fmt.Errorf("gateway protocol %q must be chat, responses or anthropic", cfg.Protocol)
	}

	return &Client{
		Name:        name,
		BaseURL:     base,
		APIKey:      key,
		Model:       model,
		Protocol:    protocol,
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
	Usage usageWire `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type usageWire struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// Complete sends one structured chat completion, used by auditors. It builds
// the wire body and delegates to Chat.
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

	raw, used, err := c.Chat(ctx, model, body)
	if err != nil {
		return Response{}, err
	}
	response, err := parseResponse(raw, model, c.Name)
	if err != nil {
		return Response{}, err
	}
	used.Model = response.Model
	c.fireUsage(used)
	return response, nil
}

// Chat forwards a raw OpenAI chat body and returns the raw response bytes plus
// the usage the provider reported. Failover only happens for account-specific
// failures.
func (c *Client) Chat(ctx context.Context, model string, body []byte) ([]byte, Usage, error) {
	switch c.Protocol {
	case ProtocolResponses:
		return c.chatResponses(ctx, model, body)
	case ProtocolAnthropic:
		return c.chatAnthropic(ctx, model, body)
	case ProtocolGemini:
		return c.chatGemini(ctx, model, body)
	}
	attempts := 1
	if c.Credentials != nil {
		attempts = maxCredentialAttempts
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		credential, err := c.pick(ctx, model)
		if err != nil {
			if lastErr != nil {
				return nil, Usage{}, fmt.Errorf("gateway %s: %w (last error: %v)", c.Name, err, lastErr)
			}
			return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
		}
		credentialID := credential.ID

		status, retryAfter, raw, err := c.roundTrip(ctx, body, credential)
		if err != nil {
			c.report(credentialID, 0, err.Error(), 0)
			return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
		}

		if status < 200 || status > 299 {
			c.report(credentialID, status, string(raw), retryAfter)
			lastErr = fmt.Errorf("HTTP %d: %s", status, excerpt(raw))
			if switchable(status) && attempt+1 < attempts {
				continue
			}
			return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, lastErr)
		}

		c.report(credentialID, status, "", 0)
		used := parseUsage(raw)
		used.ProviderID = c.Name
		used.CredentialID = credentialID
		if used.Model == "" {
			used.Model = model
		}
		return raw, used, nil
	}

	return nil, Usage{}, fmt.Errorf("gateway %s: %w", c.Name, lastErr)
}

// ChatStream forwards a raw streaming chat body, copying the response to dst as
// it arrives and extracting usage from the SSE stream when the provider sends
// it. Nothing is written until the upstream accepted the request, so callers can
// still send an error status on failure.
func (c *Client) ChatStream(ctx context.Context, model string, body []byte, dst io.Writer) (Usage, error) {
	switch c.Protocol {
	case ProtocolResponses:
		return c.streamResponses(ctx, model, body, dst)
	case ProtocolAnthropic:
		return c.streamAnthropic(ctx, model, body, dst)
	case ProtocolGemini:
		return c.streamGemini(ctx, model, body, dst)
	}
	attempts := 1
	if c.Credentials != nil {
		attempts = maxCredentialAttempts
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		credential, err := c.pick(ctx, model)
		if err != nil {
			if lastErr != nil {
				return Usage{}, fmt.Errorf("gateway %s: %w (last error: %v)", c.Name, err, lastErr)
			}
			return Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
		}
		credentialID := credential.ID

		resp, err := c.openStream(ctx, body, credential)
		if err != nil {
			c.report(credentialID, 0, err.Error(), 0)
			return Usage{}, fmt.Errorf("gateway %s: %w", c.Name, err)
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			c.report(credentialID, resp.StatusCode, string(raw), retryAfter(resp))
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, excerpt(raw))
			if switchable(resp.StatusCode) && attempt+1 < attempts {
				continue
			}
			return Usage{}, fmt.Errorf("gateway %s: %w", c.Name, lastErr)
		}

		c.report(credentialID, resp.StatusCode, "", 0)
		used := Usage{ProviderID: c.Name, CredentialID: credentialID, Model: model}
		writer := &sseUsageWriter{dst: dst, usage: &used}
		_, copyErr := io.Copy(writer, io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		writer.flushLine()
		if copyErr != nil {
			return used, fmt.Errorf("gateway %s: stream copy: %w", c.Name, copyErr)
		}
		if used.Model == "" {
			used.Model = model
		}
		return used, nil
	}

	return Usage{}, fmt.Errorf("gateway %s: %w", c.Name, lastErr)
}

func (c *Client) pick(ctx context.Context, model string) (Credential, error) {
	if c.Credentials == nil {
		return Credential{APIKey: c.APIKey}, nil
	}
	credential, err := c.Credentials.Next(ctx, c.Name, model)
	if err != nil {
		return Credential{}, err
	}
	if credential.APIKey == "" {
		credential.APIKey = c.APIKey
	}
	return credential, nil
}

// send performs one upstream call with account failover. Adapters use it so the
// retry and reporting policy is identical for every protocol.
func (c *Client) send(ctx context.Context, model, endpoint string, body []byte, stream bool) (*http.Response, string, error) {
	return c.sendBuild(ctx, model, endpoint, stream, func(Credential) ([]byte, error) { return body, nil })
}

// sendBuild is send with a body that depends on the chosen account. Code Assist
// needs the account's project id inside the payload, and it must come from the
// same account that answers the request.
func (c *Client) sendBuild(ctx context.Context, model, endpoint string, stream bool, build func(Credential) ([]byte, error)) (*http.Response, string, error) {
	attempts := 1
	if c.Credentials != nil {
		attempts = maxCredentialAttempts
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		credential, err := c.pick(ctx, model)
		if err != nil {
			if lastErr != nil {
				return nil, "", fmt.Errorf("gateway %s: %w (last error: %v)", c.Name, err, lastErr)
			}
			return nil, "", fmt.Errorf("gateway %s: %w", c.Name, err)
		}

		body, err := build(credential)
		if err != nil {
			return nil, "", fmt.Errorf("gateway %s: %w", c.Name, err)
		}
		httpReq, err := c.protoRequest(ctx, endpoint, body, credential, stream)
		if err != nil {
			return nil, "", fmt.Errorf("gateway %s: %w", c.Name, err)
		}
		resp, err := c.HTTP.Do(httpReq)
		if err != nil {
			c.report(credential.ID, 0, err.Error(), 0)
			return nil, "", fmt.Errorf("gateway %s: %w", c.Name, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			c.report(credential.ID, resp.StatusCode, string(raw), retryAfter(resp))
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, excerpt(raw))
			if switchable(resp.StatusCode) && attempt+1 < attempts {
				continue
			}
			return nil, "", fmt.Errorf("gateway %s: %w", c.Name, lastErr)
		}
		c.report(credential.ID, resp.StatusCode, "", 0)
		return resp, credential.ID, nil
	}
	return nil, "", fmt.Errorf("gateway %s: %w", c.Name, lastErr)
}

func (c *Client) fireUsage(used Usage) {
	if c.OnUsage != nil {
		c.OnUsage(used)
	}
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

func (c *Client) newRequest(ctx context.Context, body []byte, credential Credential) (*http.Request, error) {
	return c.protoRequest(ctx, c.BaseURL+"/chat/completions", body, credential, false)
}

// protoRequest builds one JSON POST for any protocol. Headers configured on the
// client are applied last so an operator can override a vendor default.
func (c *Client) protoRequest(ctx context.Context, endpoint string, body []byte, credential Credential, stream bool) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	c.applyAuth(httpReq, credential)
	for name, value := range c.Headers {
		httpReq.Header.Set(name, value)
	}
	return httpReq, nil
}

// roundTrip performs one non-streaming HTTP call.
func (c *Client) roundTrip(ctx context.Context, body []byte, credential Credential) (int, time.Duration, []byte, error) {
	httpReq, err := c.newRequest(ctx, body, credential)
	if err != nil {
		return 0, 0, nil, err
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
	return resp.StatusCode, retryAfter(resp), raw, nil
}

// openStream starts one streaming HTTP call.
func (c *Client) openStream(ctx context.Context, body []byte, credential Credential) (*http.Response, error) {
	httpReq, err := c.newRequest(ctx, body, credential)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	return c.HTTP.Do(httpReq)
}

func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		return seconds
	}
	return 0
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

func parseUsage(raw []byte) Usage {
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Usage{}
	}
	used := Usage{
		Model:            parsed.Model,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
	}
	if parsed.Usage.PromptTokensDetails != nil {
		used.CachedTokens = parsed.Usage.PromptTokensDetails.CachedTokens
	}
	return used
}

// sseUsageWriter passes bytes through untouched and watches complete `data:`
// lines for a usage object. Providers only send usage at the end, and only when
// stream_options.include_usage is requested.
type sseUsageWriter struct {
	dst   io.Writer
	buf   []byte
	usage *Usage
}

func (w *sseUsageWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		index := bytes.IndexByte(w.buf, '\n')
		if index < 0 {
			break
		}
		w.parseLine(w.buf[:index+1])
		w.buf = w.buf[index+1:]
	}
	return w.dst.Write(p)
}

func (w *sseUsageWriter) flushLine() {
	if len(w.buf) > 0 {
		w.parseLine(w.buf)
		w.buf = nil
	}
}

func (w *sseUsageWriter) parseLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var chunk struct {
		Model string    `json:"model"`
		Usage usageWire `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	if chunk.Model != "" {
		w.usage.Model = chunk.Model
	}
	if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
		w.usage.PromptTokens = chunk.Usage.PromptTokens
		w.usage.CompletionTokens = chunk.Usage.CompletionTokens
		if chunk.Usage.PromptTokensDetails != nil {
			w.usage.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
	}
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
