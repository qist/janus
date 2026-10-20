package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/logx"
)

func TestRefreshOAuthSecret(t *testing.T) {
	var gotForm url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-token", "refresh_token": "r2", "expires_in": 3600,
		})
	}))
	t.Cleanup(ts.Close)

	out, err := refreshOAuthSecret(t.Context(), map[string]any{
		"access_token": "old", "refresh_token": "r1", "token_url": ts.URL,
		"client_id": "cid", "client_secret": "sec",
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if out["access_token"] != "new-token" || out["refresh_token"] != "r2" {
		t.Fatalf("out = %+v", out)
	}
	if _, ok := out["expires_at"].(int64); !ok {
		t.Fatalf("expires_at missing: %+v", out)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "r1" ||
		gotForm.Get("client_id") != "cid" || gotForm.Get("client_secret") != "sec" {
		t.Fatalf("form = %v", gotForm)
	}
}

func TestRefreshOAuthSecretRejectsIncomplete(t *testing.T) {
	if _, err := refreshOAuthSecret(t.Context(), map[string]any{"refresh_token": "r1"}); err == nil {
		t.Fatal("want error when token_url is missing")
	}
}

// 端到端：oauth 凭据续期后新 token 落库，且只返回键名不回显明文。
func TestHandleRefreshCredentialPersistsSecret(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 60})
	}))
	t.Cleanup(ts.Close)

	st, err := db.Open(filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.SaveProvider(db.Provider{
		ID: "oc", Type: "openai-chat", BaseURL: "https://example.invalid", AuthKind: "oauth", Enabled: true,
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	cred, err := st.CreateCredential("oc", db.CredentialInput{
		Label:  "personal",
		Secret: map[string]any{"access_token": "old", "refresh_token": "r1", "token_url": ts.URL},
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}

	srv := &Server{cfg: config.Config{MaxBodyBytes: 1 << 20}, log: logx.NewLogger("error"), db: st}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/credentials/"+cred.ID+"/refresh", nil)
	req.SetPathValue("id", cred.ID)
	rec := httptest.NewRecorder()
	srv.handleRefreshCredential(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var view struct {
		ID         string   `json:"id"`
		AuthKind   string   `json:"auth_kind"`
		SecretKeys []string `json:"secret_keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	if view.AuthKind != "oauth" {
		t.Fatalf("auth_kind = %q", view.AuthKind)
	}
	for _, k := range view.SecretKeys {
		if k == "access_token" {
			break
		}
	}

	stored, ok := st.GetCredential(cred.ID)
	if !ok {
		t.Fatal("credential disappeared")
	}
	plain, err := db.DecryptBlob(stored.Secret)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var sec map[string]any
	if err := json.Unmarshal(plain, &sec); err != nil {
		t.Fatalf("unmarshal secret: %v", err)
	}
	if sec["access_token"] != "fresh" {
		t.Fatalf("access_token not refreshed: %+v", sec)
	}
	if sec["refresh_token"] != "r1" {
		t.Fatalf("refresh_token should be preserved: %+v", sec)
	}
}
