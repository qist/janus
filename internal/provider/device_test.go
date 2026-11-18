package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"janus/internal/oauth"
)

func deviceRegistry(t *testing.T, polls *int) *oauth.DeviceRegistry {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "test-client" {
			t.Errorf("client_id = %q", r.Form.Get("client_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "device-1", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("device_code") != "device-1" {
			t.Errorf("device_code = %q", r.Form.Get("device_code"))
		}
		*polls++
		if *polls < 2 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gh-token", "token_type": "bearer", "scope": "read:user",
		})
	})
	mux.HandleFunc("/exchange", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token gh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "copilot-token", "expires_at": 1_700_003_600,
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "octocat"})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	registry := oauth.NewDeviceRegistry()
	registry.Set(oauth.DeviceConfig{
		Kind:            "github-copilot",
		ClientID:        "test-client",
		DeviceURL:       server.URL + "/device/code",
		TokenURL:        server.URL + "/token",
		ExchangeURL:     server.URL + "/exchange",
		ExchangeHeaders: map[string]string{"Editor-Version": "vscode/1.95.0"},
		UserURL:         server.URL + "/user",
	})
	return registry
}

func TestDeviceFlowCreatesCredentialAndUsesDerivedToken(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	polls := 0
	svc.Devices = deviceRegistry(t, &polls)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "gh", Kind: KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	start, err := svc.StartDevice(ctx, "gh", "")
	if err != nil {
		t.Fatalf("StartDevice: %v", err)
	}
	if start.UserCode != "ABCD-1234" || start.VerificationURI == "" || start.SessionID == "" {
		t.Fatalf("start = %+v", start)
	}

	poll, err := svc.PollDevice(ctx, "gh", start.SessionID)
	if err != nil {
		t.Fatalf("PollDevice pending: %v", err)
	}
	if poll.Status != "pending" {
		t.Fatalf("status = %q", poll.Status)
	}

	poll, err = svc.PollDevice(ctx, "gh", start.SessionID)
	if err != nil {
		t.Fatalf("PollDevice complete: %v", err)
	}
	if poll.Status != "complete" || poll.Credential == nil {
		t.Fatalf("poll = %+v", poll)
	}
	if poll.Credential.Account != "octocat" || poll.Credential.Kind != CredentialOAuth {
		t.Fatalf("credential = %+v", poll.Credential)
	}

	// The device session is one-shot after success.
	if _, err := svc.PollDevice(ctx, "gh", start.SessionID); err != ErrOAuthSession {
		t.Fatalf("reused session = %v", err)
	}

	provider, _ := svc.Store.GetProvider(ctx, "gh")
	// The service list is masked by design; the key path works on stored rows.
	credentials, _ := svc.Store.ListCredentials(ctx, "gh")
	key, err := svc.credentialKey(ctx, provider, credentials[0])
	if err != nil {
		t.Fatalf("credentialKey: %v", err)
	}
	if key != "copilot-token" {
		t.Fatalf("key = %q, want the exchanged token", key)
	}
}

func TestDeviceDerivedTokenIsRenewed(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	polls := 0
	svc.Devices = deviceRegistry(t, &polls)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "gh", Kind: KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	// A stored pair whose derived token already expired, as after a long idle.
	credential, err := svc.storeTokens(ctx, "gh", "octocat", "octocat", "", oauth.TokenSet{
		AccessToken: "gh-token", DerivedToken: "stale", DerivedExpiresAt: 1,
	})
	if err != nil {
		t.Fatalf("storeTokens: %v", err)
	}

	provider, _ := svc.Store.GetProvider(ctx, "gh")
	key, err := svc.credentialKey(ctx, provider, credential)
	if err != nil {
		t.Fatalf("credentialKey: %v", err)
	}
	if key != "copilot-token" {
		t.Fatalf("key = %q", key)
	}

	stored, err := svc.Store.ListCredentials(ctx, "gh")
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored = %+v (%v)", stored, err)
	}
	plain, err := svc.Box.Open(stored[0].SecretCiphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var tokens oauth.TokenSet
	if err := json.Unmarshal([]byte(plain), &tokens); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if tokens.DerivedToken != "copilot-token" || tokens.DerivedExpiresAt != 1_700_003_600 {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestPlainKeyOnDeviceChannelIsExchanged(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	polls := 0
	svc.Devices = deviceRegistry(t, &polls)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "gh", Kind: KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	// A pasted GitHub token is stored as a plain key, not a token set.
	if _, err := svc.CreateCredential(ctx, "gh", "pat", CredentialAPIKey, "gh-token"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	provider, _ := svc.Store.GetProvider(ctx, "gh")
	storedRows, _ := svc.Store.ListCredentials(ctx, "gh")
	key, err := svc.credentialKey(ctx, provider, storedRows[0])
	if err != nil {
		t.Fatalf("credentialKey: %v", err)
	}
	if key != "copilot-token" {
		t.Fatalf("key = %q", key)
	}

	stored, _ := svc.Store.ListCredentials(ctx, "gh")
	if stored[0].Kind != CredentialOAuth {
		t.Fatalf("pasted token was not upgraded to oauth: %+v", stored[0])
	}
}

func TestDeviceFlowErrors(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	polls := 0
	svc.Devices = deviceRegistry(t, &polls)

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "plain", Kind: KindOpenAI, Channel: "deepseek",
		BaseURL: "https://api.deepseek.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.StartDevice(ctx, "plain", ""); err == nil {
		t.Fatal("device flow started on a non-device channel")
	}

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "gh", Kind: KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider gh: %v", err)
	}
	start, err := svc.StartDevice(ctx, "gh", "")
	if err != nil {
		t.Fatalf("StartDevice: %v", err)
	}
	if _, err := svc.PollDevice(ctx, "other", start.SessionID); err != ErrOAuthSession {
		t.Fatalf("cross-provider poll = %v", err)
	}
}

func TestAuthMethodsFollowChannel(t *testing.T) {
	cases := []struct {
		provider Provider
		want     string
	}{
		{Provider{Kind: KindOpenAI, Channel: "deepseek"}, "api_key"},
		{Provider{Kind: KindOpenAI, Channel: "openai-codex"}, "oauth"},
		{Provider{Kind: KindOpenAI, Channel: "claude-code"}, "oauth"},
		{Provider{Kind: KindOpenAI, Channel: "github-copilot"}, "device_code"},
		{Provider{Kind: KindOpenAI, Channel: "ollama"}, "none"},
		{Provider{Kind: KindOpenAI}, "api_key"},
		{Provider{Kind: KindOpenAI, OAuth: &OAuthConfig{ClientID: "x", AuthorizeURL: "https://a", TokenURL: "https://b", RedirectURI: "https://c"}}, "oauth"},
	}
	for _, tc := range cases {
		methods := tc.provider.AuthMethods()
		if len(methods) == 0 || methods[0] != tc.want {
			t.Fatalf("%s/%s = %v, want %s", tc.provider.Channel, tc.provider.Kind, methods, tc.want)
		}
	}
}

func TestOAuthRequiresOAuthChannel(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	withOAuth(t, svc, "https://example.com/token")

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "ds", Kind: KindOpenAI, Channel: "deepseek",
		BaseURL: "https://api.deepseek.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	// Previously this fell through to kind=openai and opened the Codex page.
	if _, err := svc.StartOAuth(ctx, "ds", ""); err == nil {
		t.Fatal("API-key channel offered an OAuth login")
	}
}

var _ = io.Discard
