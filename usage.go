package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 用量与余额查询。
//
// OpenCode 的本地 server 只暴露「已消耗」（/api/experimental/session/stats），
// 真正的「余额」在 console API 上，需要 OAuth 凭据。
// 凭据存在 OpenCode 的 SQLite 库里（credential 表，integration_id='opencode'），
// 这里用只读方式取出来，调 console 的 billing / usage 接口。
//
// 实测端点（2026-10，OpenCode v2.0.22）：
//
//	GET {console}/billing/status      → 余额、可用额度、计费模式
//	GET {console}/usage/summary       → 总消耗（tokens + cost）
//	GET {console}/usage/models        → 按模型分组的消耗
//	GET {console}/usage/cost-by-day   → 按天分组的消耗
//
// 注意：`*MicroCents` 单位是「微美分」，1 USD = 1e8 micro-cents。

// microCents 是 console API 的金额单位。1 USD = 100 分 = 1e8 微分。
type microCents int64

func (m microCents) USD() float64 { return float64(m) / 1e8 }

// UnmarshalJSON 兼容字符串与数字两种形式（console 金额用字符串）。
func (m *microCents) UnmarshalJSON(b []byte) error {
	var f flexInt
	if err := f.UnmarshalJSON(b); err != nil {
		return err
	}
	*m = microCents(f)
	return nil
}

// flexInt 兼容 JSON 里数字以字符串或数字两种形式出现（console 用字符串）。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	// 可能是 "342653290" 也可能是 342653290，甚至带小数
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(v)
		return nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("not a number: %q", s)
	}
	*f = flexInt(int64(fl))
	return nil
}

type UsageReport struct {
	Billing   BillingStatus `json:"billing"`
	Totals    UsageTotals   `json:"totals"`
	Models    []ModelUsage  `json:"models"`
	ByDay     []DayUsage    `json:"by_day"`
	Go        *GoUsage      `json:"go,omitempty"`
	FetchedAt time.Time     `json:"fetched_at"`
	Source    string        `json:"source"`
}

// GoUsage 是 OpenCode Go / Go Plus 套餐的三窗口限额用量：
// 5 小时滚动 / 每周 / 每月。它是独立于 Zen 余额的数据模型，
// 只有 inference 主机（而非 console）暴露该端点，且为非公开接口。
type GoUsage struct {
	Rolling GoWindow `json:"rolling"`
	Weekly  GoWindow `json:"weekly"`
	Monthly GoWindow `json:"monthly"`
}

// GoWindow 单个限额窗口。Percent 是已用百分比（0–100），
// ResetsInSec 是拉取时距重置的秒数（服务端只给绝对时间，这里换算好方便展示）。
type GoWindow struct {
	Status      string    `json:"status"`
	Percent     float64   `json:"percent"`
	ResetsAt    time.Time `json:"resets_at"`
	ResetsInSec int64     `json:"resets_in_sec"`
}

type BillingStatus struct {
	Mode            string   `json:"mode"`         // 如 pay-as-you-go
	BillingMode     string   `json:"billing_mode"` // 如 prepaid
	BalanceUSD      float64  `json:"balance_usd"`
	AvailableUSD    float64  `json:"available_usd"`
	CreditLimitUSD  *float64 `json:"credit_limit_usd"`
	CanPurchase     bool     `json:"can_purchase_credits"`
	CanAutoRecharge bool     `json:"can_enable_auto_recharge"`
}

type UsageTotals struct {
	Requests           int64   `json:"requests"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheWrite5mTokens int64   `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64   `json:"cache_write_1h_tokens"`
	CostUSD            float64 `json:"cost_usd"`
}

type ModelUsage struct {
	Provider        string  `json:"provider"`
	Model           string  `json:"model"`
	Requests        int64   `json:"requests"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	CostUSD         float64 `json:"cost_usd"`
}

type DayUsage struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

// ---------- 凭据读取 ----------

type consoleCredential struct {
	Access  string
	OrgID   string
	Email   string
	Expires int64
}

// readCredential 只读打开 OpenCode 的 SQLite 库取 OAuth 凭据。
//
// 用 sqlite3 CLI 而不是引入 Go 的 sqlite 驱动：本桥坚持零第三方依赖，
// 而 sqlite3 在目标环境是常装的。拿不到就返回可读的错误，端点降级为 503。
func readCredential(dbPath string) (*consoleCredential, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("opencode db path is empty (set OPENCODE_DB)")
	}
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, fmt.Errorf("sqlite3 CLI not found in PATH; usage endpoint unavailable")
	}
	const q = `select value from credential where integration_id='opencode' and active=1 limit 1;`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-readonly", dbPath, q)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("read credential from %s: %s", filepath.Base(dbPath), msg)
	}

	var raw struct {
		Type     string `json:"type"`
		Access   string `json:"access"`
		Expires  int64  `json:"expires"`
		Metadata struct {
			OrgID string `json:"orgID"`
			Email string `json:"email"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse credential JSON: %w", err)
	}
	if raw.Access == "" {
		return nil, fmt.Errorf("no active opencode credential found in %s", filepath.Base(dbPath))
	}
	if raw.Type != "" && raw.Type != "oauth" {
		return nil, fmt.Errorf("unsupported credential type %q (want oauth)", raw.Type)
	}
	if raw.Expires > 0 && raw.Expires < time.Now().UnixMilli() {
		return nil, fmt.Errorf("opencode credential expired at %s; re-login in OpenCode",
			time.UnixMilli(raw.Expires).Format(time.RFC3339))
	}
	return &consoleCredential{
		Access:  raw.Access,
		OrgID:   raw.Metadata.OrgID,
		Email:   raw.Metadata.Email,
		Expires: raw.Expires,
	}, nil
}

// ---------- console 客户端 ----------

type UsageClient struct {
	base   string
	goURL  string
	dbPath string
	hc     *http.Client
	log    *Logger
	ttl    time.Duration

	mu       sync.Mutex
	cached   *UsageReport
	cachedAt time.Time
}

func NewUsageClient(cfg Config, log *Logger) *UsageClient {
	return &UsageClient{
		base:   cfg.ConsoleURL,
		goURL:  cfg.GoUsageURL,
		dbPath: cfg.OpencodeDB,
		log:    log,
		ttl:    cfg.UsageTTL,
		hc:     &http.Client{Timeout: 15 * time.Second},
	}
}

// Report 返回缓存的用量报告。
func (c *UsageClient) Report(ctx context.Context) (*UsageReport, error) {
	c.mu.Lock()
	if c.cached != nil && time.Since(c.cachedAt) < c.ttl {
		r := c.cached
		c.mu.Unlock()
		return r, nil
	}
	c.mu.Unlock()

	rep, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cached, c.cachedAt = rep, time.Now()
	c.mu.Unlock()
	return rep, nil
}

func (c *UsageClient) fetch(ctx context.Context) (*UsageReport, error) {
	cred, err := readCredential(c.dbPath)
	if err != nil {
		return nil, err
	}

	// console 的四个端点并发拉，任一失败整体失败（余额最关键）。
	// Go 套餐用量走另一个域，属可选数据：失败只降级为不展示。
	var (
		wg         sync.WaitGroup
		billing    billingRaw
		summary    summaryRaw
		models     modelsRaw
		byDay      []dayRaw
		goRaw      goUsageRaw
		errB, errS error
		errM, errD error
		errG       error
	)
	wg.Add(5)
	go func() {
		defer wg.Done()
		errB = c.get(ctx, cred, "/billing/status", &billing)
	}()
	go func() {
		defer wg.Done()
		errS = c.get(ctx, cred, "/usage/summary", &summary)
	}()
	go func() {
		defer wg.Done()
		errM = c.get(ctx, cred, "/usage/models", &models)
	}()
	go func() {
		defer wg.Done()
		errD = c.get(ctx, cred, "/usage/cost-by-day", &byDay)
	}()
	go func() {
		defer wg.Done()
		if c.goURL == "" {
			errG = fmt.Errorf("go usage endpoint not configured")
			return
		}
		errG = c.getURL(ctx, cred, c.goURL, &goRaw)
	}()
	wg.Wait()

	if errB != nil {
		return nil, fmt.Errorf("billing/status: %w", errB)
	}
	if errS != nil {
		return nil, fmt.Errorf("usage/summary: %w", errS)
	}
	if errM != nil {
		return nil, fmt.Errorf("usage/models: %w", errM)
	}
	if errD != nil {
		return nil, fmt.Errorf("usage/cost-by-day: %w", errD)
	}

	rep := &UsageReport{
		FetchedAt: time.Now().UTC(),
		Source:    c.base,
		Billing: BillingStatus{
			Mode:            billing.Mode,
			BillingMode:     billing.BillingMode,
			BalanceUSD:      billing.BalanceMicroCents.USD(),
			AvailableUSD:    billing.AvailableMicroCents.USD(),
			CanPurchase:     billing.CanPurchaseCredits,
			CanAutoRecharge: billing.CanEnableAutoRecharge,
		},
		Totals: UsageTotals{
			Requests:           int64(summary.TotalRequests),
			InputTokens:        int64(summary.TotalInputTokens),
			OutputTokens:       int64(summary.TotalOutputTokens),
			CacheReadTokens:    int64(summary.TotalCacheReadTokens),
			CacheWrite5mTokens: int64(summary.TotalCacheWrite5mTokens),
			CacheWrite1hTokens: int64(summary.TotalCacheWrite1hTokens),
			CostUSD:            summary.TotalCostMicroCents.USD(),
		},
	}
	if billing.CreditLimitMicroCents != nil {
		v := billing.CreditLimitMicroCents.USD()
		rep.Billing.CreditLimitUSD = &v
	}

	for _, m := range models.Items {
		rep.Models = append(rep.Models, ModelUsage{
			Provider:        m.Provider,
			Model:           m.Model,
			Requests:        int64(m.TotalRequests),
			InputTokens:     int64(m.TotalInputTokens),
			OutputTokens:    int64(m.TotalOutputTokens),
			CacheReadTokens: int64(m.TotalCacheReadTokens),
			CostUSD:         m.TotalCostMicroCents.USD(),
		})
	}
	for _, d := range byDay {
		rep.ByDay = append(rep.ByDay, DayUsage{
			Date:     d.Date,
			Requests: int64(d.TotalRequests),
			Tokens:   int64(d.TotalTokens),
			CostUSD:  d.TotalCostMicroCents.USD(),
		})
	}
	if errG != nil {
		c.log.Debugf("go plan usage unavailable: %v", errG)
	} else {
		rep.Go = goRaw.toUsage()
	}
	return rep, nil
}

func (c *UsageClient) get(ctx context.Context, cred *consoleCredential, path string, out any) error {
	return c.getURL(ctx, cred, c.base+path, out)
}

// getURL 与 get 相同，但接受完整 URL（Go 套餐用量在另一个域）。
func (c *UsageClient) getURL(ctx context.Context, cred *consoleCredential, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cred.Access)
	if cred.OrgID != "" {
		req.Header.Set("x-opencode-org-id", cred.OrgID)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("console rejected the credential (HTTP %d); re-login in OpenCode", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// ---------- console 原始响应 ----------

type billingRaw struct {
	BillingMode           string      `json:"billingMode"`
	Mode                  string      `json:"mode"`
	BalanceMicroCents     microCents  `json:"balanceMicroCents"`
	CreditLimitMicroCents *microCents `json:"creditLimitMicroCents"`
	AvailableMicroCents   microCents  `json:"availableMicroCents"`
	CanPurchaseCredits    bool        `json:"canPurchaseCredits"`
	CanEnableAutoRecharge bool        `json:"canEnableAutoRecharge"`
}

type summaryRaw struct {
	TotalRequests           flexInt    `json:"totalRequests"`
	TotalInputTokens        flexInt    `json:"totalInputTokens"`
	TotalOutputTokens       flexInt    `json:"totalOutputTokens"`
	TotalCacheReadTokens    flexInt    `json:"totalCacheReadTokens"`
	TotalCacheWrite5mTokens flexInt    `json:"totalCacheWrite5mTokens"`
	TotalCacheWrite1hTokens flexInt    `json:"totalCacheWrite1hTokens"`
	TotalCostMicroCents     microCents `json:"totalCostMicroCents"`
}

type modelsRaw struct {
	Items []struct {
		Model                string     `json:"model"`
		Provider             string     `json:"provider"`
		TotalRequests        flexInt    `json:"totalRequests"`
		TotalInputTokens     flexInt    `json:"totalInputTokens"`
		TotalOutputTokens    flexInt    `json:"totalOutputTokens"`
		TotalCacheReadTokens flexInt    `json:"totalCacheReadTokens"`
		TotalCostMicroCents  microCents `json:"totalCostMicroCents"`
	} `json:"items"`
}

type dayRaw struct {
	Date                string     `json:"date"`
	TotalCostMicroCents microCents `json:"totalCostMicroCents"`
	TotalTokens         flexInt    `json:"totalTokens"`
	TotalRequests       flexInt    `json:"totalRequests"`
}

// goUsageRaw 是 inference 主机 /v1/usage 的响应。
// 注意字段名是 resetsAt（驼峰），与 console 端点的 snake_case 不同。
type goUsageRaw struct {
	Usage struct {
		Rolling goWindowRaw `json:"rolling"`
		Weekly  goWindowRaw `json:"weekly"`
		Monthly goWindowRaw `json:"monthly"`
	} `json:"usage"`
}

type goWindowRaw struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

// toUsage 把原始响应换算成对外结构；三个窗口都没有状态时返回 nil（非 Go 账号）。
func (r goUsageRaw) toUsage() *GoUsage {
	if r.Usage.Rolling.Status == "" && r.Usage.Weekly.Status == "" && r.Usage.Monthly.Status == "" {
		return nil
	}
	now := time.Now()
	conv := func(w goWindowRaw) GoWindow {
		out := GoWindow{Status: w.Status, Percent: w.Percent}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			out.ResetsAt = t
			if d := t.Sub(now); d > 0 {
				out.ResetsInSec = int64(d.Seconds())
			}
		}
		return out
	}
	return &GoUsage{
		Rolling: conv(r.Usage.Rolling),
		Weekly:  conv(r.Usage.Weekly),
		Monthly: conv(r.Usage.Monthly),
	}
}
