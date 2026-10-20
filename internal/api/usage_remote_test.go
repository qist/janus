package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/logx"
	"janus/internal/usage"
)

// remoteUsageConsole 是一个假的 OpenCode console：记录鉴权头并返回四个账单端点。
func remoteUsageConsole(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("Authorization = %q, want Bearer %s", got, wantToken)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/status":
			_, _ = w.Write([]byte(`{"mode":"pay-as-you-go","balanceMicroCents":"250000000","availableMicroCents":"200000000"}`))
		case "/usage/summary":
			_, _ = w.Write([]byte(`{"totalRequests":"7","totalInputTokens":"100","totalOutputTokens":"20","totalCostMicroCents":"123"}`))
		case "/usage/models":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "/usage/cost-by-day":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// oauth 凭据：远程用量按凭据各自的 console 拉，不再依赖全局 OpenCode DB。
func TestHandleRemoteUsageOAuth(t *testing.T) {
	console := remoteUsageConsole(t, "tok-up")

	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SaveProvider(db.Provider{
		ID: "oc", Type: "openai-chat", BaseURL: "https://example.invalid", AuthKind: "oauth", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if _, err := st.CreateCredential("oc", db.CredentialInput{
		Label:  "personal",
		Secret: map[string]any{"access_token": "tok-up"},
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	cfg := config.Config{ConsoleURL: console.URL, MaxBodyBytes: 1 << 20}
	srv := &Server{cfg: cfg, log: logx.NewLogger("error"), db: st, usage: usage.New(cfg, logx.NewLogger("error"))}

	rec := httptest.NewRecorder()
	srv.handleRemoteUsage(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/remote", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Count int               `json:"count"`
		Items []remoteUsageItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Count != 1 || len(out.Items) != 1 {
		t.Fatalf("count = %d items = %d", out.Count, len(out.Items))
	}
	it := out.Items[0]
	if !it.OK {
		t.Fatalf("item not ok: %+v", it)
	}
	if it.AuthKind != "oauth" || it.Source != console.URL {
		t.Fatalf("item = %+v", it)
	}
	if it.Billing == nil || it.Billing.BalanceUSD != 2.5 {
		t.Fatalf("billing = %+v", it.Billing)
	}
	if it.Totals == nil || it.Totals.Requests != 7 {
		t.Fatalf("totals = %+v", it.Totals)
	}
}

// 不支持远程账单的鉴权方式：逐条返回失败原因而不是整接口报错。
func TestHandleRemoteUsageUnsupportedKind(t *testing.T) {
	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SaveProvider(db.Provider{
		ID: "vend", Type: "openai-chat", BaseURL: "https://example.invalid", AuthKind: "api_key", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if _, err := st.CreateCredential("vend", db.CredentialInput{
		Label:  "key-1",
		Secret: map[string]any{"api_key": "sk-1"},
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}

	cfg := config.Config{MaxBodyBytes: 1 << 20}
	srv := &Server{cfg: cfg, log: logx.NewLogger("error"), db: st, usage: usage.New(cfg, logx.NewLogger("error"))}

	rec := httptest.NewRecorder()
	srv.handleRemoteUsage(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/remote?provider_id=vend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []remoteUsageItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].OK {
		t.Fatalf("items = %+v", out.Items)
	}
	if !strings.Contains(out.Items[0].Error, "api_key") {
		t.Fatalf("error = %q", out.Items[0].Error)
	}
}
