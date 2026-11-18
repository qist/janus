package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultQuotaTimeout bounds one upstream quota read. These endpoints are
// informational; a slow vendor must not hold the console request open.
const DefaultQuotaTimeout = 12 * time.Second

// QuotaWindow is one allowance window reported by a vendor. Vendors differ in
// what they expose: some give a percentage, some give absolute amounts, some
// only a reset time. One shape covers all three; empty fields mean "not
// reported", never zero.
type QuotaWindow struct {
	Name        string  `json:"name"`
	UsedPercent float64 `json:"used_percent,omitempty"`
	Used        float64 `json:"used,omitempty"`
	Limit       float64 `json:"limit,omitempty"`
	Unit        string  `json:"unit,omitempty"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
	Text        string  `json:"text,omitempty"`
}

// Quota is the last observed allowance or balance of one credential.
type Quota struct {
	Balance   string        `json:"balance,omitempty"`
	Currency  string        `json:"currency,omitempty"`
	Plan      string        `json:"plan,omitempty"`
	Windows   []QuotaWindow `json:"windows,omitempty"`
	Source    string        `json:"source,omitempty"`
	CheckedAt int64         `json:"checked_at"`
	Error     string        `json:"error,omitempty"`
}

// QuotaResult pairs a quota snapshot with the credential it belongs to.
type QuotaResult struct {
	ProviderID   string `json:"provider_id"`
	CredentialID string `json:"credential_id"`
	Masked       string `json:"masked,omitempty"`
	Quota        Quota  `json:"quota"`
}

// RefreshQuota reads the allowance/balance of every credential of one provider
// and persists the snapshot on the credential. A vendor that has no quota
// endpoint is not an error: the result carries an explanatory message instead.
func (s *Service) RefreshQuota(ctx context.Context, providerID string) ([]QuotaResult, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	credentials, err := s.Store.ListCredentials(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if s.Box == nil {
		return nil, ErrSecretBox
	}

	timeout := s.QuotaTimeout
	if timeout <= 0 {
		timeout = DefaultQuotaTimeout
	}

	results := make([]QuotaResult, 0, len(credentials))
	for _, credential := range credentials {
		quota := Quota{CheckedAt: s.now()}
		secret, err := s.Box.Open(credential.SecretCiphertext)
		if err != nil {
			quota.Error = "credential cannot be decrypted"
		} else {
			quota = s.readQuota(ctx, p, credential, secret, timeout)
			quota.CheckedAt = s.now()
		}

		credential.Quota = &quota
		credential.UpdatedAt = s.now()
		if err := s.Store.SaveCredential(ctx, credential); err != nil {
			return results, err
		}
		results = append(results, QuotaResult{
			ProviderID:   p.ID,
			CredentialID: credential.ID,
			Masked:       credential.Masked,
			Quota:        quota,
		})
	}
	return results, nil
}

// ListQuotas returns the stored snapshots without contacting any vendor.
func (s *Service) ListQuotas(ctx context.Context, providerID string) ([]QuotaResult, error) {
	credentials, err := s.Store.ListCredentials(ctx, strings.TrimSpace(providerID))
	if err != nil {
		return nil, err
	}
	out := make([]QuotaResult, 0, len(credentials))
	for _, credential := range credentials {
		if credential.Quota == nil {
			continue
		}
		out = append(out, QuotaResult{
			ProviderID:   credential.ProviderID,
			CredentialID: credential.ID,
			Masked:       credential.Masked,
			Quota:        *credential.Quota,
		})
	}
	return out, nil
}

// readQuota dispatches on the catalogue channel first and on the host second,
// so providers created before channels existed still work.
func (s *Service) readQuota(ctx context.Context, p Provider, credential Credential, secret string, timeout time.Duration) Quota {
	channel := p.Channel
	if channel == "" {
		channel = channelForHost(p.BaseURL)
	}

	switch channel {
	case "deepseek":
		return readDeepSeek(ctx, p.BaseURL, secret, timeout)
	case "moonshot", "kimi-code":
		return readMoonshot(ctx, p.BaseURL, secret, timeout)
	case "openrouter":
		return readOpenRouter(ctx, p.BaseURL, secret, timeout)
	case "claude-code", "anthropic":
		return readClaude(ctx, p.BaseURL, secret, credential.Kind == CredentialOAuth, timeout)
	case "openai-codex":
		return readCodex(ctx, secret, timeout)
	}

	return Quota{Source: "none", Error: fmt.Sprintf("no quota endpoint for channel %q", channel)}
}

func channelForHost(baseURL string) string {
	host := strings.ToLower(baseURL)
	switch {
	case strings.Contains(host, "deepseek"):
		return "deepseek"
	case strings.Contains(host, "moonshot"), strings.Contains(host, "kimi"):
		return "moonshot"
	case strings.Contains(host, "openrouter"):
		return "openrouter"
	case strings.Contains(host, "anthropic"):
		return "anthropic"
	case strings.Contains(host, "chatgpt.com"), strings.Contains(host, "openai.com"):
		return "openai-codex"
	}
	return ""
}

// rootURL strips the protocol suffixes vendors append to one shared host.
func rootURL(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/anthropic", "/compatible-mode/v1", "/v1"} {
		root = strings.TrimSuffix(root, suffix)
	}
	return root
}

func readDeepSeek(ctx context.Context, baseURL, secret string, timeout time.Duration) Quota {
	var parsed struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	url := rootURL(baseURL) + "/user/balance"
	if err := httpJSON(ctx, url, secret, nil, timeout, &parsed); err != nil {
		return Quota{Source: "deepseek", Error: err.Error()}
	}
	out := Quota{Source: "deepseek"}
	if len(parsed.BalanceInfos) > 0 {
		info := parsed.BalanceInfos[0]
		out.Balance = info.TotalBalance
		out.Currency = info.Currency
	}
	if !parsed.IsAvailable {
		out.Error = "account reports insufficient balance"
	}
	return out
}

func readMoonshot(ctx context.Context, baseURL, secret string, timeout time.Duration) Quota {
	var parsed struct {
		Data struct {
			AvailableBalance float64 `json:"available_balance"`
			VoucherBalance   float64 `json:"voucher_balance"`
			CashBalance      float64 `json:"cash_balance"`
		} `json:"data"`
	}
	url := rootURL(baseURL) + "/v1/users/me/balance"
	if err := httpJSON(ctx, url, secret, nil, timeout, &parsed); err != nil {
		return Quota{Source: "moonshot", Error: err.Error()}
	}
	return Quota{
		Source:   "moonshot",
		Balance:  strconv.FormatFloat(parsed.Data.AvailableBalance, 'f', 2, 64),
		Currency: "CNY",
		Windows: []QuotaWindow{{
			Name: "voucher",
			Used: parsed.Data.VoucherBalance,
			Unit: "CNY",
		}, {
			Name: "cash",
			Used: parsed.Data.CashBalance,
			Unit: "CNY",
		}},
	}
}

func readOpenRouter(ctx context.Context, baseURL, secret string, timeout time.Duration) Quota {
	var parsed struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	url := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/credits"
	if err := httpJSON(ctx, url, secret, nil, timeout, &parsed); err != nil {
		return Quota{Source: "openrouter", Error: err.Error()}
	}
	remaining := parsed.Data.TotalCredits - parsed.Data.TotalUsage
	return Quota{
		Source:   "openrouter",
		Balance:  strconv.FormatFloat(remaining, 'f', 4, 64),
		Currency: "USD",
		Windows: []QuotaWindow{{
			Name:  "credits",
			Used:  parsed.Data.TotalUsage,
			Limit: parsed.Data.TotalCredits,
			Unit:  "USD",
		}},
	}
}

// readClaude uses the same endpoint the Claude Code CLI uses. The OAuth variant
// requires the beta header; an API key would get a 401 there, so keyed
// providers are skipped instead of pretending.
func readClaude(ctx context.Context, baseURL, secret string, oauth bool, timeout time.Duration) Quota {
	if !oauth {
		return Quota{Source: "anthropic", Error: "Anthropic API keys have no quota endpoint"}
	}
	var parsed struct {
		FiveHour *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
		SevenDaySonnet *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day_sonnet"`
		SevenDayOpus *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day_opus"`
	}
	headers := map[string]string{"anthropic-beta": "oauth-2025-04-20"}
	url := "https://api.anthropic.com/api/oauth/usage"
	if err := httpJSON(ctx, url, secret, headers, timeout, &parsed); err != nil {
		return Quota{Source: "anthropic", Error: err.Error()}
	}
	out := Quota{Source: "anthropic"}
	appendWindow := func(name string, used float64, reset string) {
		out.Windows = append(out.Windows, QuotaWindow{
			Name:        name,
			UsedPercent: normalizePercent(used),
			ResetsAt:    parseTime(reset),
		})
	}
	if parsed.FiveHour != nil {
		appendWindow("five_hour", parsed.FiveHour.Utilization, parsed.FiveHour.ResetsAt)
	}
	if parsed.SevenDay != nil {
		appendWindow("seven_day", parsed.SevenDay.Utilization, parsed.SevenDay.ResetsAt)
	}
	if parsed.SevenDaySonnet != nil {
		appendWindow("seven_day_sonnet", parsed.SevenDaySonnet.Utilization, parsed.SevenDaySonnet.ResetsAt)
	}
	if parsed.SevenDayOpus != nil {
		appendWindow("seven_day_opus", parsed.SevenDayOpus.Utilization, parsed.SevenDayOpus.ResetsAt)
	}
	return out
}

// readCodex reads the ChatGPT plan usage. The endpoint is the one the Codex
// backend exposes; it reports windows as percentages plus optional credits.
func readCodex(ctx context.Context, secret string, timeout time.Duration) Quota {
	var parsed struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			PrimaryWindow *struct {
				UsedPercent       float64 `json:"used_percent"`
				LimitWindowSecond int64   `json:"limit_window_seconds"`
				ResetAt           int64   `json:"reset_at"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent       float64 `json:"used_percent"`
				LimitWindowSecond int64   `json:"limit_window_seconds"`
				ResetAt           int64   `json:"reset_at"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		Credits *struct {
			HasCredits bool   `json:"has_credits"`
			Unlimited  bool   `json:"unlimited"`
			Balance    string `json:"balance"`
		} `json:"credits"`
	}
	url := "https://chatgpt.com/backend-api/wham/usage"
	if err := httpJSON(ctx, url, secret, nil, timeout, &parsed); err != nil {
		return Quota{Source: "codex", Error: err.Error()}
	}
	out := Quota{Source: "codex", Plan: parsed.PlanType}
	if parsed.Credits != nil && parsed.Credits.Balance != "" {
		out.Balance = parsed.Credits.Balance
		out.Currency = "credits"
	}
	addWindow := func(name string, window *struct {
		UsedPercent       float64 `json:"used_percent"`
		LimitWindowSecond int64   `json:"limit_window_seconds"`
		ResetAt           int64   `json:"reset_at"`
	}) {
		if window == nil {
			return
		}
		out.Windows = append(out.Windows, QuotaWindow{
			Name:        name,
			UsedPercent: normalizePercent(window.UsedPercent),
			ResetsAt:    window.ResetAt,
			Text:        windowLabel(window.LimitWindowSecond),
		})
	}
	if parsed.RateLimit != nil {
		addWindow("primary", parsed.RateLimit.PrimaryWindow)
		addWindow("secondary", parsed.RateLimit.SecondaryWindow)
	}
	return out
}

// normalizePercent accepts both 0..1 fractions and 0..100 percentages. Values
// above 1 are already percentages; a vendor returning "150" for overspend
// stays 150 instead of being multiplied.
func normalizePercent(value float64) float64 {
	if value > 0 && value <= 1 {
		return value * 100
	}
	return value
}

func windowLabel(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds%(7*86400) == 0:
		return fmt.Sprintf("%dd", seconds/(7*86400)*7)
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dm", seconds/60)
	}
}

func parseTime(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.Unix()
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05.999999", value); err == nil {
		return parsed.Unix()
	}
	return 0
}

// httpJSON performs one authenticated GET and decodes a bounded body. Errors
// never include the secret.
func httpJSON(ctx context.Context, url, secret string, headers map[string]string, timeout time.Duration, out any) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s", sanitizeProbeError(err, secret))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, probeExcerpt(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
