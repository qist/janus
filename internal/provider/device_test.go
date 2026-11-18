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

	start, err := svc.StartDevice(ctx, "gh", "", "")
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
	if _, err := svc.StartDevice(ctx, "plain", "", ""); err == nil {
		t.Fatal("device flow started on a non-device channel")
	}

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "gh", Kind: KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider gh: %v", err)
	}
	start, err := svc.StartDevice(ctx, "gh", "", "")
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

func TestDeviceReauthorizationUpdatesInPlace(t *testing.T) {
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
	first, err := svc.storeTokens(ctx, "gh", "work", "octocat", "", oauth.TokenSet{
		AccessToken: "gh-token", DerivedToken: "old", DerivedExpiresAt: 1,
	})
	if err != nil {
		t.Fatalf("storeTokens: %v", err)
	}
	label := "work"
	priority := 4
	if _, err := svc.UpdateCredential(ctx, first.ID, CredentialPatch{Label: &label, Priority: &priority}); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}

	start, err := svc.StartDevice(ctx, "gh", "", first.ID)
	if err != nil {
		t.Fatalf("StartDevice reauth: %v", err)
	}
	// One pending poll keeps the session; the second completes.
	if _, err := svc.PollDevice(ctx, "gh", start.SessionID); err != nil {
		t.Fatalf("PollDevice pending: %v", err)
	}
	poll, err := svc.PollDevice(ctx, "gh", start.SessionID)
	if err != nil {
		t.Fatalf("PollDevice complete: %v", err)
	}
	if poll.Status != "complete" || poll.Credential == nil {
		t.Fatalf("poll = %+v", poll)
	}
	if poll.Credential.ID != first.ID {
		t.Fatalf("re-authorization created a new account: %s != %s", poll.Credential.ID, first.ID)
	}
	if poll.Credential.Priority != 4 || poll.Credential.Label != "work" {
		t.Fatalf("fields not preserved: %+v", poll.Credential)
	}

	stored, _ := svc.Store.ListCredentials(ctx, "gh")
	if len(stored) != 1 {
		t.Fatalf("credential rows = %d, want 1", len(stored))
	}
	provider, _ := svc.Store.GetProvider(ctx, "gh")
	key, err := svc.credentialKey(ctx, provider, stored[0])
	if err != nil {
		t.Fatalf("credentialKey: %v", err)
	}
	if key != "copilot-token" {
		t.Fatalf("key = %q", key)
	}
}

func TestDeviceReauthorizationRejectsForeignKey(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	polls := 0
	svc.Devices = deviceRegistry(t, &polls)

	for _, id := range []string{"gh", "gh2"} {
		if _, err := svc.CreateProvider(ctx, Provider{
			ID: id, Kind: KindOpenAI, Channel: "github-copilot",
			BaseURL: "https://api.githubcopilot.com",
		}); err != nil {
			t.Fatalf("CreateProvider %s: %v", id, err)
		}
	}
	other, err := svc.storeTokens(ctx, "gh2", "x", "x", "", oauth.TokenSet{AccessToken: "t"})
	if err != nil {
		t.Fatalf("storeTokens: %v", err)
	}
	if _, err := svc.StartDevice(ctx, "gh", "", other.ID); err == nil {
		t.Fatal("re-authorized a credential from another provider")
	}
}

func TestCapabilitiesTellWhichLoginActuallyWorks(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)

	cases := []struct {
		id           string
		channel      string
		kind         Kind
		wantAuth     string
		wantOAuth    bool
		wantDevice   bool
		customConfig bool
	}{
		{"codex", "openai-codex", KindOpenAI, "oauth", true, false, false},
		{"claude", "claude-code", KindAnthropic, "oauth", true, false, false},
		{"copilot", "github-copilot", KindOpenAI, "device_code", false, true, false},
		{"deepseek", "deepseek", KindOpenAI, "api_key", false, false, false},
		// The catalogue documents OAuth, but janus has no client for it; the
		// console must not offer a button that opens nothing.
		{"google", "google-code-assist", KindCustom, "oauth", false, false, false},
		{"antigravity", "antigravity", KindCustom, "oauth", false, false, false},
		{"custom", "custom", KindOpenAI, "oauth", true, false, true},
	}
	for _, tc := range cases {
		provider := Provider{ID: tc.id, Kind: tc.kind, Channel: tc.channel, BaseURL: "https://api.example.com/v1"}
		if tc.customConfig {
			provider.OAuth = &OAuthConfig{
				ClientID: "cid", AuthorizeURL: "https://login.example.com/a",
				TokenURL: "https://login.example.com/t", RedirectURI: "https://login.example.com/cb",
			}
		}
		if _, err := svc.CreateProvider(ctx, provider); err != nil {
			t.Fatalf("%s: CreateProvider: %v", tc.id, err)
		}
		stored, err := svc.Store.GetProvider(ctx, tc.id)
		if err != nil {
			t.Fatalf("%s: GetProvider: %v", tc.id, err)
		}
		filled := svc.WithCapabilities(stored)
		if len(filled.Auth) == 0 || filled.Auth[0] != tc.wantAuth {
			t.Fatalf("%s: auth = %v, want %s", tc.id, filled.Auth, tc.wantAuth)
		}
		if filled.OAuthReady != tc.wantOAuth {
			t.Fatalf("%s: oauth_ready = %v", tc.id, filled.OAuthReady)
		}
		if filled.DeviceReady != tc.wantDevice {
			t.Fatalf("%s: device_ready = %v", tc.id, filled.DeviceReady)
		}
	}
}
