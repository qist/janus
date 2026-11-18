package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"janus/internal/gateway"
	"janus/internal/oauth"
)

func encodeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestCustomOAuthFlowAndIdentity(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at-custom","refresh_token":"rt-custom","expires_in":3600}`))
	}))
	t.Cleanup(tokenSrv.Close)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID:      "workbuddy",
		Kind:    KindOpenAI,
		BaseURL: "https://copilot.tencent.com/v2",
		Channel: "workbuddy",
		OAuth: &OAuthConfig{
			ClientID:     "third-party-client",
			AuthorizeURL: "https://login.example.com/authorize",
			TokenURL:     tokenSrv.URL,
			RedirectURI:  "https://login.example.com/callback",
			Scopes:       []string{"openid", "offline_access"},
		},
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	start, err := svc.StartOAuth(ctx, "workbuddy", "")
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	if !strings.HasPrefix(start.AuthURL, "https://login.example.com/authorize") {
		t.Fatalf("auth url = %s", start.AuthURL)
	}
	if !strings.Contains(start.AuthURL, "client_id=third-party-client") {
		t.Fatalf("custom client id missing: %s", start.AuthURL)
	}

	idToken := encodeJWT(t, map[string]any{
		"email": "dev@example.com",
		"exp":   float64(svc.now() + 3600),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": "plus",
		},
	})
	tokenSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at-custom","refresh_token":"rt-custom","id_token":"` + idToken + `","expires_in":3600}`))
	})

	credential, err := svc.CompleteOAuth(ctx, "workbuddy", start.SessionID, "code", "")
	if err != nil {
		t.Fatalf("CompleteOAuth: %v", err)
	}
	if credential.Account != "dev@example.com" {
		t.Fatalf("account = %q", credential.Account)
	}
	if credential.Plan != "plus" {
		t.Fatalf("plan = %q", credential.Plan)
	}
	if credential.Label != "dev@example.com" {
		t.Fatalf("label = %q", credential.Label)
	}
}

func TestReauthorizeUpdatesExistingAccount(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	srv := tokenServer(t, nil)
	withOAuth(t, svc, srv.URL)
	addOAuthProvider(t, svc, "https://api.example.com/v1")

	first, err := svc.storeTokens(ctx, "p1", "work", "work@example.com", "pro", oauth.TokenSet{
		AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: svc.now() + 3600,
	})
	if err != nil {
		t.Fatalf("storeTokens: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "second", CredentialAPIKey, "sk-2"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	start, err := svc.StartOAuth(ctx, "p1", first.ID)
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	if start.CredentialID != first.ID {
		t.Fatalf("start = %+v", start)
	}
	updated, err := svc.CompleteOAuth(ctx, "p1", start.SessionID, "code", "")
	if err != nil {
		t.Fatalf("CompleteOAuth: %v", err)
	}
	if updated.ID != first.ID {
		t.Fatalf("re-authorization created a new account: %s != %s", updated.ID, first.ID)
	}
	if updated.Priority != first.Priority {
		t.Fatalf("priority changed: %d != %d", updated.Priority, first.Priority)
	}
	list, _ := svc.ListCredentials(ctx, "p1")
	if len(list) != 2 {
		t.Fatalf("credential count = %d, want 2", len(list))
	}
}

func TestImportCodexAndOpenCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))

	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idToken := encodeJWT(t, map[string]any{"email": "codex@example.com", "exp": float64(time.Now().Unix() + 3600)})
	codexAuth := map[string]any{
		"tokens": map[string]any{
			"id_token":      idToken,
			"access_token":  "at-codex",
			"refresh_token": "rt-codex",
			"account_id":    "acct-1",
		},
	}
	raw, _ := json.Marshal(codexAuth)
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	openDir := filepath.Join(home, "xdg", "opencode")
	if err := os.MkdirAll(openDir, 0o700); err != nil {
		t.Fatal(err)
	}
	openAuth := map[string]any{
		"anthropic": map[string]any{
			"type":    "oauth",
			"access":  "at-open",
			"refresh": "rt-open",
			"expires": (time.Now().Unix() + 3600) * 1000,
		},
		"github-copilot": map[string]any{"type": "api", "access": "ghp-token"},
	}
	raw, _ = json.Marshal(openAuth)
	if err := os.WriteFile(filepath.Join(openDir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "codex", Kind: KindOpenAI, BaseURL: "https://chatgpt.com/backend-api/codex", Channel: "openai-codex",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "claude", Kind: KindAnthropic, BaseURL: "https://api.anthropic.com", Channel: "claude-code",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	result, err := svc.ImportLocal(ctx, "codex", ImportCodex, "", "")
	if err != nil {
		t.Fatalf("ImportLocal codex: %v", err)
	}
	if len(result.Imported) != 1 || result.Imported[0].Kind != CredentialOAuth {
		t.Fatalf("codex import = %+v", result)
	}
	if result.Imported[0].Account != "codex@example.com" {
		t.Fatalf("codex account = %q", result.Imported[0].Account)
	}

	result, err = svc.ImportLocal(ctx, "claude", ImportOpenCode, "", "")
	if err != nil {
		t.Fatalf("ImportLocal opencode: %v", err)
	}
	if len(result.Imported) != 1 || result.Imported[0].Account != "anthropic account" {
		t.Fatalf("opencode import = %+v", result)
	}

	// Importing again updates in place instead of duplicating the account.
	result, err = svc.ImportLocal(ctx, "codex", ImportCodex, "", "")
	if err != nil {
		t.Fatalf("re-import codex: %v", err)
	}
	list, _ := svc.ListCredentials(ctx, "codex")
	if len(list) != 1 {
		t.Fatalf("re-import duplicated the account: %d rows", len(list))
	}
}

func TestCredentialOutcomeSurvivesReload(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	addProvider(t, svc, "https://api.example.com/v1")
	first, err := svc.CreateCredential(ctx, "p1", "first", CredentialAPIKey, "sk-first")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	second, err := svc.CreateCredential(ctx, "p1", "second", CredentialAPIKey, "sk-second")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini",
	}); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	if err := svc.RecordCredentialOutcome(ctx, first.ID, StatusExhausted, 0, svc.now()+3600, "quota"); err != nil {
		t.Fatalf("RecordCredentialOutcome: %v", err)
	}

	clients, err := svc.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	credential, err := clients["fast"].Credentials.Next(ctx, "p1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if credential.ID != second.ID {
		t.Fatalf("picked %s, want the healthy account %s", credential.ID, second.ID)
	}

	// A cooldown with no reset time would otherwise be forgotten on restart.
	stored, _ := svc.ListCredentials(ctx, "p1")
	if stored[0].Status != StatusExhausted || stored[0].ResetsAt == 0 {
		t.Fatalf("exhausted state not persisted: %+v", stored[0])
	}
}

func TestStickyAffinityKeepsAccount(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	addProvider(t, svc, "https://api.example.com/v1")
	first, err := svc.CreateCredential(ctx, "p1", "first", CredentialAPIKey, "sk-first")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	second, err := svc.CreateCredential(ctx, "p1", "second", CredentialAPIKey, "sk-second")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini",
	}); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}
	clients, err := svc.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}

	conversation := gateway.WithAffinity(ctx, "conv-1")
	picked, err := clients["fast"].Credentials.Next(conversation, "p1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if picked.ID != first.ID {
		t.Fatalf("first pick = %s", picked.ID)
	}

	// The first account dies; the same conversation must move and then stay.
	clients["fast"].Credentials.Report(conversation, first.ID, http.StatusUnauthorized, "revoked", 0)
	next, err := clients["fast"].Credentials.Next(conversation, "p1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Next after failure: %v", err)
	}
	if next.ID != second.ID {
		t.Fatalf("after failure picked %s, want %s", next.ID, second.ID)
	}
	again, err := clients["fast"].Credentials.Next(conversation, "p1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Next again: %v", err)
	}
	if again.ID != second.ID {
		t.Fatalf("affinity did not stick: %s != %s", again.ID, second.ID)
	}
}

func TestUpdateAndResetCredential(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	addProvider(t, svc, "https://api.example.com/v1")
	credential, err := svc.CreateCredential(ctx, "p1", "old", CredentialAPIKey, "sk-test")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	label := "notes"
	priority := 7
	disabled := true
	updated, err := svc.UpdateCredential(ctx, credential.ID, CredentialPatch{
		Label: &label, Priority: &priority, Disabled: &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	if updated.Label != "notes" || updated.Priority != 7 || !updated.Disabled {
		t.Fatalf("updated = %+v", updated)
	}

	if err := svc.RecordCredentialOutcome(ctx, credential.ID, StatusRateLimited, svc.now()+60, 0, "429"); err != nil {
		t.Fatalf("RecordCredentialOutcome: %v", err)
	}
	reset, err := svc.ResetCredential(ctx, credential.ID)
	if err != nil {
		t.Fatalf("ResetCredential: %v", err)
	}
	if reset.Status != StatusUnknown || reset.CooldownUntil != 0 || reset.LastError != "" || reset.Disabled {
		t.Fatalf("reset = %+v", reset)
	}
}
