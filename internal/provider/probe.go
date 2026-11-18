package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultProbeTimeout bounds one connectivity probe. A probe is a cheap
// GET /models: it validates the endpoint and the key without spending tokens.
const DefaultProbeTimeout = 10 * time.Second

// maxProbeModels caps how many model ids a probe result carries back.
const maxProbeModels = 50

// ProbeResult is the outcome of checking one provider's endpoint and keys.
//
// OK=false is a normal answer, not a transport error: the probe ran, the
// upstream is simply not usable. The result names the credential that answered
// (masked) and the classified status so the console can update its view.
type ProbeResult struct {
	OK           bool     `json:"ok"`
	ProviderID   string   `json:"provider_id"`
	BaseURL      string   `json:"base_url"`
	CredentialID string   `json:"credential_id,omitempty"`
	Masked       string   `json:"masked,omitempty"`
	LatencyMS    int64    `json:"latency_ms"`
	ModelCount   int      `json:"model_count"`
	Models       []string `json:"models,omitempty"`
	Status       Status   `json:"status"`
	Error        string   `json:"error,omitempty"`
}

// Probe checks GET {base_url}/models with each stored credential until one
// answers. Side effect: the credential status is updated so the pool and the
// console immediately reflect what the probe learned.
func (s *Service) Probe(ctx context.Context, providerID string) (ProbeResult, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return ProbeResult{}, err
	}

	result := ProbeResult{
		ProviderID: p.ID,
		BaseURL:    p.BaseURL,
		Status:     StatusUnknown,
	}
	if s.Box == nil {
		result.Error = "secret storage is not configured"
		return result, nil
	}

	credentials, err := s.Store.ListCredentials(ctx, p.ID)
	if err != nil {
		return ProbeResult{}, err
	}
	if len(credentials) == 0 {
		result.Error = "provider has no credentials"
		return result, nil
	}

	timeout := s.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	var last string
	for _, credential := range credentials {
		secret, err := s.Box.Open(credential.SecretCiphertext)
		if err != nil {
			last = fmt.Sprintf("credential %s: cannot decrypt", credential.ID)
			continue
		}

		started := time.Now()
		outcome := probeModels(ctx, p.BaseURL, secret, timeout)
		latency := time.Since(started)

		result.CredentialID = credential.ID
		result.Masked = credential.Masked
		result.LatencyMS = latency.Milliseconds()

		if outcome.err == "" {
			result.OK = true
			result.Status = StatusOK
			result.Models = outcome.models
			result.ModelCount = outcome.count
			s.markCredential(ctx, credential, StatusOK)
			return result, nil
		}

		status := probeStatus(outcome.status)
		result.Status = status
		result.Error = outcome.err
		last = fmt.Sprintf("%s (HTTP %d)", outcome.err, outcome.status)
		s.markCredential(ctx, credential, status)
	}

	if result.Error == "" {
		result.Error = last
	}
	return result, nil
}

// markCredential persists a status change. Best effort: a probe must not fail
// because bookkeeping did.
func (s *Service) markCredential(ctx context.Context, credential Credential, status Status) {
	if status == "" || credential.Status == status {
		return
	}
	credential.Status = status
	credential.UpdatedAt = s.now()
	_ = s.Store.SaveCredential(ctx, credential)
}

// probeStatus maps an HTTP status to a credential status. Empty means "not the
// credential's fault" (5xx, timeout, DNS); those leave the stored status alone.
func probeStatus(httpStatus int) Status {
	switch {
	case httpStatus >= 200 && httpStatus <= 299:
		return StatusOK
	case httpStatus == 401, httpStatus == 403:
		return StatusInvalid
	case httpStatus == 402:
		return StatusExhausted
	case httpStatus == 429:
		return StatusRateLimited
	default:
		return ""
	}
}

type probeOutcome struct {
	status int
	count  int
	models []string
	err    string
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeModels performs the actual request. It never logs or returns the key.
func probeModels(ctx context.Context, baseURL, secret string, timeout time.Duration) probeOutcome {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"
	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, url, nil)
	if err != nil {
		return probeOutcome{err: "build request: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return probeOutcome{err: sanitizeProbeError(err, secret)}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if readErr != nil {
		return probeOutcome{status: resp.StatusCode, err: "read response: " + readErr.Error()}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return probeOutcome{
			status: resp.StatusCode,
			err:    fmt.Sprintf("HTTP %d: %s", resp.StatusCode, probeExcerpt(body)),
		}
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return probeOutcome{status: resp.StatusCode, err: "decode /models: " + err.Error()}
	}

	out := probeOutcome{status: resp.StatusCode, count: len(parsed.Data)}
	for _, model := range parsed.Data {
		if len(out.models) >= maxProbeModels {
			break
		}
		if id := strings.TrimSpace(model.ID); id != "" {
			out.models = append(out.models, id)
		}
	}
	return out
}

// sanitizeProbeError strips the key from transport errors. http.Client errors
// normally do not include headers, but a wrapped URL might, and a probe error is
// shown in the console and written to logs.
func sanitizeProbeError(err error, secret string) string {
	message := err.Error()
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "***")
	}
	return message
}

func probeExcerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}
