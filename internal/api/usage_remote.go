package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"janus/internal/channel/workbuddy"
	"janus/internal/db"
	"janus/internal/httpx"
	"janus/internal/usage"
)

// usage_remote.go —— 远程分渠道用量。
//
// 与本地记账（usage_events）互补：这里按**每份凭据**去它自己的上游账单接口
// 拉余额/积分，多账号多渠道各自独立，不再是全局一份。
//
//	oauth（OpenCode console） → /billing/status + /usage/{summary,models,cost-by-day}
//	workbuddy                 → 积分/套餐余量接口
//
// 每份凭据的结果带 TTL 缓存（含失败负缓存），避免面板刷新时反复打上游。

// remoteUsageItem 是单份凭据的远程用量/余额。
type remoteUsageItem struct {
	ProviderID   string    `json:"provider_id"`
	CredentialID string    `json:"credential_id"`
	Label        string    `json:"label"`
	AuthKind     string    `json:"auth_kind"`
	OK           bool      `json:"ok"`
	Error        string    `json:"error,omitempty"`
	Source       string    `json:"source,omitempty"`
	FetchedAt    time.Time `json:"fetched_at"`

	// OpenCode console 系
	Billing *usage.BillingStatus `json:"billing,omitempty"`
	Totals  *usage.Totals        `json:"totals,omitempty"`
	Models  []usage.ModelUsage   `json:"models,omitempty"`
	ByDay   []usage.DayUsage     `json:"by_day,omitempty"`
	GoPlan  *usage.GoUsage       `json:"go,omitempty"`

	// WorkBuddy 系
	Credits *workbuddy.Credits `json:"credits,omitempty"`
}

// remoteUsageCache 是每凭据的结果缓存（含失败负缓存）。
type remoteUsageCache struct {
	mu    sync.Mutex
	items map[string]cachedRemoteUsage
	ttl   time.Duration
}

type cachedRemoteUsage struct {
	item remoteUsageItem
	at   time.Time
}

func newRemoteUsageCache(ttl time.Duration) *remoteUsageCache {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &remoteUsageCache{items: map[string]cachedRemoteUsage{}, ttl: ttl}
}

func (c *remoteUsageCache) get(id string) (remoteUsageItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[id]
	if !ok || time.Since(it.at) >= c.ttl {
		return remoteUsageItem{}, false
	}
	return it.item, true
}

func (c *remoteUsageCache) put(id string, item remoteUsageItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[id] = cachedRemoteUsage{item: item, at: time.Now()}
}

// handleRemoteUsage 返回按凭据聚合的远程用量：GET /api/v1/usage/remote
//
//	?provider_id=<id>     只看某个 provider
//	&credential_id=<id>   只看某一份凭据
//	&refresh=1            绕过缓存强制重拉
func (s *Server) handleRemoteUsage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	force := q.Get("refresh") == "1"

	var rows []db.Credential
	if id := strings.TrimSpace(q.Get("credential_id")); id != "" {
		c, found := st.GetCredential(id)
		if !found {
			writeAPIError(w, http.StatusNotFound, "not_found", "credential not found", nil)
			return
		}
		rows = []db.Credential{c}
	} else {
		rows = st.ListCredentials(strings.TrimSpace(q.Get("provider_id")))
	}

	items := make([]remoteUsageItem, len(rows))
	sem := make(chan struct{}, 4) // 上游限速：最多 4 路并发
	var wg sync.WaitGroup
	for i, c := range rows {
		wg.Add(1)
		go func(i int, c db.Credential) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			prov, found := st.GetProvider(c.ProviderID)
			items[i] = s.remoteUsageFor(r.Context(), st, prov, found, c, force)
		}(i, c)
	}
	wg.Wait()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"count":      len(items),
		"fetched_at": time.Now().UTC(),
	})
}

// remoteUsageFor 拉一份凭据的远程用量（带缓存）。
func (s *Server) remoteUsageFor(ctx context.Context, st *db.Store, prov db.Provider, provFound bool, cred db.Credential, force bool) remoteUsageItem {
	item := remoteUsageItem{
		ProviderID:   cred.ProviderID,
		CredentialID: cred.ID,
		Label:        cred.Label,
		FetchedAt:    time.Now().UTC(),
	}
	if provFound {
		item.AuthKind = prov.AuthKind
	}
	if !force && s.remoteUsage != nil {
		if cached, ok := s.remoteUsage.get(cred.ID); ok {
			return cached
		}
	}
	if !provFound {
		item.Error = "provider not found"
	} else {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		s.fillRemoteUsage(cctx, st, prov, cred, &item)
		cancel()
	}
	if s.remoteUsage != nil {
		s.remoteUsage.put(cred.ID, item)
	}
	return item
}

// fillRemoteUsage 按鉴权方式分派到对应的上游账单接口。
func (s *Server) fillRemoteUsage(ctx context.Context, st *db.Store, prov db.Provider, cred db.Credential, item *remoteUsageItem) {
	plain, err := db.DecryptBlob(cred.Secret)
	if err != nil {
		item.Error = "decrypt credential: " + err.Error()
		return
	}
	var secret map[string]any
	if err := json.Unmarshal(plain, &secret); err != nil {
		item.Error = "credential secret is not a JSON object"
		return
	}
	str := func(k string) string {
		if v, ok := secret[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}

	switch prov.AuthKind {
	case "workbuddy":
		wbCred, err := workbuddy.CredentialFromSecret(secret)
		if err != nil {
			item.Error = err.Error()
			return
		}
		client := workbuddy.NewClient(wbCred, "")
		credits, err := client.FetchCredits(ctx)
		if err != nil {
			item.Error = err.Error()
			return
		}
		item.Credits = credits
		item.Source = "workbuddy"
		item.OK = true

	case "oauth":
		access := str("access_token")
		// access_token 缺失或已过期：先尝试续期。续期不可用但手上还有 token 时，
		// 仍然拿旧 token 打一次（上游 401 会判定），比直接报错更不容易误伤。
		if access == "" || tokenExpired(secret) {
			next, rerr := refreshOAuthSecret(ctx, secret)
			switch {
			case rerr == nil:
				if _, uerr := st.UpdateCredential(cred.ID, nil, next, nil, nil); uerr != nil {
					s.log.Warnf("usage remote: persist refreshed credential %s failed: %v", cred.ID, uerr)
				}
				secret = next
				access = str("access_token")
			case access == "":
				item.Error = rerr.Error()
				return
			default:
				s.log.Warnf("usage remote: refresh credential %s failed, trying stale token: %v", cred.ID, rerr)
			}
		}
		base := str("console_url")
		if base == "" {
			base = strings.TrimRight(strings.TrimSpace(s.cfg.ConsoleURL), "/")
		}
		rep, err := s.usage.ReportFor(ctx, access, str("org_id"), base, strings.TrimSpace(s.cfg.GoUsageURL))
		if err != nil {
			item.Error = err.Error()
			return
		}
		item.Billing = &rep.Billing
		item.Totals = &rep.Totals
		item.Models = rep.Models
		item.ByDay = rep.ByDay
		item.GoPlan = rep.Go
		item.Source = rep.Source
		item.FetchedAt = rep.FetchedAt
		item.OK = true

	default:
		item.Error = "auth_kind " + prov.AuthKind + " has no remote usage source"
	}
}

// tokenExpired 判断凭据里的 access_token 是否（即将）过期。
// 没有 expires_at 时按「未知」处理，交给上游 401 判定。
func tokenExpired(secret map[string]any) bool {
	v, ok := secret["expires_at"]
	if !ok {
		return false
	}
	var exp int64
	switch n := v.(type) {
	case float64:
		exp = int64(n)
	case json.Number:
		exp, _ = n.Int64()
	case string:
		if t, err := time.Parse(time.RFC3339, n); err == nil {
			exp = t.Unix()
		}
	}
	if exp == 0 {
		return false
	}
	return time.Now().Add(60*time.Second).Unix() >= exp
}
