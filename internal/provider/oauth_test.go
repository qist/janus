package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"janus/internal/oauth"
)

func tokenServer(t *testing.T, seen *map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		values := map[string]string{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(raw, &values)
		} else {
			form, _ := url.ParseQuery(string(raw))
			for key := range form {
				values[key] = form.Get(key)
			}
		}
		if seen != nil {
			*seen = values
		}
		io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withOAuth(t *testing.T, svc *Service, tokenURL string) *oauth.Registry {
	t.Helper()
	registry := oauth.DefaultRegistry()
	config, _ := registry.Get("openai")
	config.AuthorizeURL = "https://auth.example.com/authorize"
	config.TokenURL = tokenURL
	registry.Set(config)
	svc.OAuth = registry
	return registry
}

func TestOAuthStartAndComplete(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	var seen map[string]string
	srv := tokenServer(t, &seen)
	withOAuth(t, svc, srv.URL)

	addProvider(t, svc, "https://api.example.com/v1")

	start, err := svc.StartOAuth(ctx, "p1")
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	if start.SessionID == "" || !strings.Contains(start.AuthURL, "code_challenge=") {
		t.Fatalf("start = %+v", start)
	}
	parsed, _ := url.Parse(start.AuthURL)
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("auth url has no state")
	}

	credential, err := svc.CompleteOAuth(ctx, "p1", start.SessionID, "the-code#"+state)
	if err != nil {
		t.Fatalf("CompleteOAuth: %v", err)
	}
	if credential.Kind != CredentialOAuth || credential.Status != StatusOK {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.SecretCiphertext != "" || !credential.HasSecret || credential.Masked == "" {
		t.Fatalf("credential leaked the secret: %+v", credential)
	}
	if credential.ExpiresAt == 0 {
		t.Fatal("expiry not persisted")
	}
	if seen["grant_type"] != "authorization_code" || seen["code"] != "the-code" {
		t.Fatalf("token request = %v", seen)
	}

	stored, err := svc.Store.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(stored) != 1 || stored[0].SecretCiphertext == "" {
		t.Fatalf("stored = %+v", stored)
	}
	plain, err := svc.Box.Open(stored[0].SecretCiphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var tokens oauth.TokenSet
	if err := json.Unmarshal([]byte(plain), &tokens); err != nil {
		t.Fatalf("token json: %v", err)
	}
	if tokens.AccessToken != "at-new" || tokens.RefreshToken != "rt-new" {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestOAuthRejectsBadStateAndReusedSession(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	srv := tokenServer(t, nil)
	withOAuth(t, svc, srv.URL)
	addProvider(t, svc, "https://api.example.com/v1")

	start, err := svc.StartOAuth(ctx, "p1")
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	if _, err := svc.CompleteOAuth(ctx, "p1", start.SessionID, "code#wrong-state"); !errors.Is(err, ErrOAuthState) {
		t.Fatalf("state mismatch = %v", err)
	}

	// The session is one-shot even when the state was wrong.
	if _, err := svc.CompleteOAuth(ctx, "p1", start.SessionID, "code"); !errors.Is(err, ErrOAuthSession) {
		t.Fatalf("reused session = %v", err)
	}
}

func TestOAuthUnsupportedKind(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	withOAuth(t, svc, "https://example.com/token")

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "custom", Kind: KindCustom, BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.StartOAuth(ctx, "custom"); !errors.Is(err, ErrOAuthUnsupported) {
		t.Fatalf("StartOAuth = %v, want ErrOAuthUnsupported", err)
	}
}

func TestOAuthSessionUnknown(t *testing.T) {
	svc := testService(t)
	withOAuth(t, svc, "https://example.com/token")
	addProvider(t, svc, "https://api.example.com/v1")

	if _, err := svc.CompleteOAuth(context.Background(), "p1", "oauth_missing", "code"); !errors.Is(err, ErrOAuthSession) {
		t.Fatalf("err = %v, want ErrOAuthSession", err)
	}
}

func TestOAuthRefreshBeforeUse(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	var seen map[string]string
	srv := tokenServer(t, &seen)
	withOAuth(t, svc, srv.URL)

	addProvider(t, svc, "https://api.example.com/v1")
	// Seed an already-expired token set, as if it had been stored days ago.
	if _, err := svc.storeTokens(ctx, "p1", "oauth", oauth.TokenSet{
		AccessToken: "at-stale", RefreshToken: "rt-1", ExpiresAt: 1,
	}); err != nil {
		t.Fatalf("storeTokens: %v", err)
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
	credential, err := clients["fast"].Credentials.Next(ctx, "p1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if credential.APIKey != "at-new" {
		t.Fatalf("key = %q, want the refreshed token", credential.APIKey)
	}
	if seen["grant_type"] != "refresh_token" || seen["refresh_token"] != "rt-1" {
		t.Fatalf("refresh request = %v", seen)
	}

	// The refreshed set is written back, so the next reload does not refresh again.
	stored, _ := svc.Store.ListCredentials(ctx, "p1")
	plain, err := svc.Box.Open(stored[0].SecretCiphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var tokens oauth.TokenSet
	if err := json.Unmarshal([]byte(plain), &tokens); err != nil {
		t.Fatalf("token json: %v", err)
	}
	if tokens.AccessToken != "at-new" || tokens.ExpiresAt <= 1 {
		t.Fatalf("persisted tokens = %+v", tokens)
	}
}

func TestParseOAuthInput(t *testing.T) {
	cases := []struct {
		input string
		code  string
		state string
	}{
		{"abc", "abc", ""},
		{"abc#xyz", "abc", "xyz"},
		{"http://localhost:1455/auth/callback?code=c1&state=s1", "c1", "s1"},
		{"  spaced  ", "spaced", ""},
	}
	for _, tc := range cases {
		code, state := parseOAuthInput(tc.input)
		if code != tc.code || state != tc.state {
			t.Fatalf("%q = %q/%q, want %q/%q", tc.input, code, state, tc.code, tc.state)
		}
	}
}
