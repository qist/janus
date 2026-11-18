package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"janus/internal/oauth"
	"janus/internal/provider"
)

func (e *testEnv) enableProviders(t *testing.T) *provider.Service {
	t.Helper()
	key, err := provider.ParseMasterKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	box, err := provider.NewSecretBox(key)
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}
	svc := provider.New(provider.NewMemoryStore(), box)
	svc.Now = func() int64 { return 1_700_000_000 }
	e.server.Providers = svc
	return svc
}

func TestProvidersUnavailable(t *testing.T) {
	e := newTestEnv(t)

	resp := e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}, http.StatusNotImplemented)
	if code := errCode(t, resp); code != "providers_unavailable" {
		t.Fatalf("code = %q", code)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/providers", readerKey, nil, http.StatusNotImplemented)
	_ = errCode(t, resp)
}

func TestProviderLifecycle(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)

	resp := e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Name: "openai", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1/",
	}, http.StatusCreated)
	var created provider.Provider
	decodeBody(t, resp, &created)
	if created.ID != "p1" || !created.Enabled || created.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("created = %+v", created)
	}

	// Any read key can see the list; writes need admin.
	resp = e.expect(t, http.MethodGet, "/api/v1/providers", readerKey, nil, http.StatusOK)
	var list struct {
		Providers []provider.Provider `json:"providers"`
	}
	decodeBody(t, resp, &list)
	if len(list.Providers) != 1 || list.Providers[0].Name != "openai" {
		t.Fatalf("list = %+v", list.Providers)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/providers", readerKey, createProviderRequest{
		ID: "p2", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com",
	}, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q", code)
	}

	// Duplicate id.
	resp = e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com",
	}, http.StatusConflict)
	if code := errCode(t, resp); code != "already_exists" {
		t.Fatalf("code = %q", code)
	}

	// Invalid URL is rejected before it reaches storage.
	resp = e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p3", Kind: provider.KindOpenAI, BaseURL: "ftp://example.com",
	}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "invalid_request" {
		t.Fatalf("code = %q", code)
	}

	// Disable it.
	disabled := false
	resp = e.expect(t, http.MethodPatch, "/api/v1/providers/p1", adminKey, updateProviderRequest{
		Enabled: &disabled,
	}, http.StatusOK)
	var updated provider.Provider
	decodeBody(t, resp, &updated)
	if updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}

	e.expect(t, http.MethodDelete, "/api/v1/providers/p1", adminKey, nil, http.StatusNoContent)

	resp = e.expect(t, http.MethodGet, "/api/v1/providers", readerKey, nil, http.StatusOK)
	decodeBody(t, resp, &list)
	if len(list.Providers) != 0 {
		t.Fatalf("providers after delete = %+v", list.Providers)
	}
}

func TestProviderNotFound(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)

	resp := e.expect(t, http.MethodPatch, "/api/v1/providers/missing", adminKey, updateProviderRequest{
		Enabled: boolPtr(true),
	}, http.StatusNotFound)
	if code := errCode(t, resp); code != "provider_not_found" {
		t.Fatalf("code = %q", code)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestCredentialLifecycle(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)

	e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}, http.StatusCreated)

	const secret = "sk-live-super-secret-value"
	resp := e.expect(t, http.MethodPost, "/api/v1/providers/p1/credentials", adminKey, createCredentialRequest{
		Label: "primary", Kind: provider.CredentialAPIKey, Secret: secret,
	}, http.StatusCreated)
	raw := readBody(t, resp)
	if strings.Contains(raw, secret) {
		t.Fatalf("response leaked the secret: %s", raw)
	}
	if !strings.Contains(raw, "…alue") {
		t.Fatalf("response lacks masked hint: %s", raw)
	}

	// Listing is admin-only and still masked.
	resp = e.expect(t, http.MethodGet, "/api/v1/providers/p1/credentials", readerKey, nil, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q", code)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/providers/p1/credentials", adminKey, nil, http.StatusOK)
	var list struct {
		Credentials []provider.Credential `json:"credentials"`
	}
	decodeBody(t, resp, &list)
	if len(list.Credentials) != 1 || list.Credentials[0].HasSecret != true {
		t.Fatalf("credentials = %+v", list.Credentials)
	}

	// A provider with credentials cannot be deleted, to avoid orphaning them.
	resp = e.expect(t, http.MethodDelete, "/api/v1/providers/p1", adminKey, nil, http.StatusConflict)
	if code := errCode(t, resp); code != "provider_in_use" {
		t.Fatalf("code = %q", code)
	}

	e.expect(t, http.MethodDelete, "/api/v1/credentials/"+list.Credentials[0].ID, adminKey, nil, http.StatusNoContent)
	e.expect(t, http.MethodDelete, "/api/v1/providers/p1", adminKey, nil, http.StatusNoContent)
}

func TestRouteLifecycle(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)

	e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}, http.StatusCreated)

	resp := e.expect(t, http.MethodPost, "/api/v1/model-routes", adminKey, createRouteRequest{
		ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini", Aliases: []string{"turbo"},
	}, http.StatusCreated)
	var route provider.Route
	decodeBody(t, resp, &route)
	if route.ExposedModel != "fast" || len(route.Aliases) != 1 {
		t.Fatalf("route = %+v", route)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/model-routes", readerKey, nil, http.StatusOK)
	var routes struct {
		Routes []provider.Route `json:"routes"`
	}
	decodeBody(t, resp, &routes)
	if len(routes.Routes) != 1 || routes.Routes[0].UpstreamModel != "gpt-4o-mini" {
		t.Fatalf("routes = %+v", routes.Routes)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/model-routes", adminKey, createRouteRequest{
		ExposedModel: "turbo", ProviderID: "p1", UpstreamModel: "x",
	}, http.StatusConflict)
	if code := errCode(t, resp); code != "already_exists" {
		t.Fatalf("code = %q", code)
	}

	e.expect(t, http.MethodDelete, "/api/v1/model-routes/"+route.ID, adminKey, nil, http.StatusNoContent)
}

// The console never receives a usable secret, not even right after creating it.
func TestCredentialResponseShape(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)
	ctx := context.Background()

	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	resp := e.expect(t, http.MethodPost, "/api/v1/providers/p1/credentials", adminKey, createCredentialRequest{
		Secret: "abcdefghijklmnop",
	}, http.StatusCreated)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{"abcdefghijklmnop", "secret_ciphertext", "SecretCiphertext"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("response contains %q: %s", forbidden, text)
		}
	}
}

func TestProbeProviderEndpoint(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)
	ctx := context.Background()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"data":[{"id":"opencode-go/deepseek-v4.1-flash"},{"id":"opencode-go/glm-5.3"}]}`)
	}))
	t.Cleanup(upstream.Close)

	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: upstream.URL + "/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "primary", provider.CredentialAPIKey, "sk-live-probe-1234"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	resp := e.expect(t, http.MethodPost, "/api/v1/providers/p1/probe", adminKey, nil, http.StatusOK)
	var result provider.ProbeResult
	decodeBody(t, resp, &result)
	if !result.OK || result.ModelCount != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Masked != "…1234" {
		t.Fatalf("masked = %q", result.Masked)
	}

	// Probing is an admin action: a read key must not trigger upstream traffic.
	resp = e.expect(t, http.MethodPost, "/api/v1/providers/p1/probe", readerKey, nil, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q", code)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/providers/missing/probe", adminKey, nil, http.StatusNotFound)
	if code := errCode(t, resp); code != "provider_not_found" {
		t.Fatalf("code = %q", code)
	}
}

func TestProbeWithoutProviderService(t *testing.T) {
	e := newTestEnv(t)
	resp := e.expect(t, http.MethodPost, "/api/v1/providers/p1/probe", adminKey, nil, http.StatusNotImplemented)
	if code := errCode(t, resp); code != "providers_unavailable" {
		t.Fatalf("code = %q", code)
	}
}

func TestProviderMutationsReloadModels(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)

	calls := 0
	e.server.ReloadModels = func(context.Context) error {
		calls++
		return nil
	}

	e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}, http.StatusCreated)
	if calls != 1 {
		t.Fatalf("reload calls after provider = %d", calls)
	}

	e.expect(t, http.MethodPost, "/api/v1/providers/p1/credentials", adminKey, createCredentialRequest{
		Secret: "sk-live-reload-1234",
	}, http.StatusCreated)
	if calls != 2 {
		t.Fatalf("reload calls after credential = %d", calls)
	}

	resp := e.expect(t, http.MethodPost, "/api/v1/model-routes", adminKey, createRouteRequest{
		ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini",
	}, http.StatusCreated)
	var route provider.Route
	decodeBody(t, resp, &route)
	if calls != 3 {
		t.Fatalf("reload calls after route = %d", calls)
	}

	e.expect(t, http.MethodDelete, "/api/v1/model-routes/"+route.ID, adminKey, nil, http.StatusNoContent)
	if calls != 4 {
		t.Fatalf("reload calls after delete = %d", calls)
	}
}

func TestReloadFailureDoesNotFailMutation(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)
	e.server.ReloadModels = func(context.Context) error {
		return errors.New("decrypt failed")
	}

	// The write is committed; the previous table keeps serving and the failure
	// is logged. Returning 500 here would invite a retry that conflicts.
	e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}, http.StatusCreated)
}

func TestOAuthEndpoints(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}`)
	}))
	t.Cleanup(tokenSrv.Close)

	registry := oauth.DefaultRegistry()
	// The channel id decides the flow; the bare "openai" alias is not consulted.
	config, _ := registry.Get("openai-codex")
	config.TokenURL = tokenSrv.URL
	registry.Set(config)
	svc.OAuth = registry

	ctx := context.Background()
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "p1", Kind: provider.KindOpenAI, Channel: "openai-codex",
		BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "p2", Kind: provider.KindCustom, BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider custom: %v", err)
	}

	resp := e.expect(t, http.MethodPost, "/api/v1/providers/p1/oauth/start", adminKey, nil, http.StatusOK)
	var start provider.OAuthStart
	decodeBody(t, resp, &start)
	if start.SessionID == "" || !strings.Contains(start.AuthURL, "state=") {
		t.Fatalf("start = %+v", start)
	}
	state, _ := url.Parse(start.AuthURL)
	wantState := state.Query().Get("state")

	resp = e.expect(t, http.MethodPost, "/api/v1/providers/p1/oauth/complete", adminKey, map[string]any{
		"session_id": start.SessionID,
		"code":       "oauth-code#" + wantState,
	}, http.StatusOK)
	var credential provider.Credential
	decodeBody(t, resp, &credential)
	if credential.Kind != provider.CredentialOAuth || credential.Masked == "" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.SecretCiphertext != "" {
		t.Fatal("response leaked the sealed secret")
	}

	// Unsupported kind and an unknown session are distinct failures.
	resp = e.expect(t, http.MethodPost, "/api/v1/providers/p2/oauth/start", adminKey, nil, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "oauth_unsupported" {
		t.Fatalf("code = %q", code)
	}
	resp = e.expect(t, http.MethodPost, "/api/v1/providers/p1/oauth/complete", adminKey, map[string]any{
		"session_id": "oauth_missing", "code": "x",
	}, http.StatusConflict)
	if code := errCode(t, resp); code != "oauth_session_invalid" {
		t.Fatalf("code = %q", code)
	}

	// Starting a sign-in is admin work.
	resp = e.expect(t, http.MethodPost, "/api/v1/providers/p1/oauth/start", readerKey, nil, http.StatusForbidden)
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("code = %q", code)
	}
}

func TestCredentialPatchAndReset(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)
	ctx := context.Background()

	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	credential, err := svc.CreateCredential(ctx, "p1", "first", provider.CredentialAPIKey, "sk-live-1234")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	resp := e.expect(t, http.MethodPatch, "/api/v1/credentials/"+credential.ID, adminKey, map[string]any{
		"label":    "work account",
		"priority": 3,
		"disabled": true,
	}, http.StatusOK)
	var updated provider.Credential
	decodeBody(t, resp, &updated)
	if updated.Label != "work account" || updated.Priority != 3 || !updated.Disabled {
		t.Fatalf("updated = %+v", updated)
	}

	if err := svc.RecordCredentialOutcome(ctx, credential.ID, provider.StatusRateLimited, 1_700_000_060, 0, "429"); err != nil {
		t.Fatalf("RecordCredentialOutcome: %v", err)
	}
	resp = e.expect(t, http.MethodPost, "/api/v1/credentials/"+credential.ID+"/reset", adminKey, nil, http.StatusOK)
	var reset provider.Credential
	decodeBody(t, resp, &reset)
	if reset.Status != provider.StatusUnknown || reset.Disabled {
		t.Fatalf("reset = %+v", reset)
	}

	resp = e.expect(t, http.MethodPatch, "/api/v1/credentials/missing", adminKey, map[string]any{
		"label": "x",
	}, http.StatusNotFound)
	if code := errCode(t, resp); code != "credential_not_found" {
		t.Fatalf("code = %q", code)
	}
}

func TestProviderCustomOAuthValidation(t *testing.T) {
	e := newTestEnv(t)
	e.enableProviders(t)

	resp := e.expect(t, http.MethodPost, "/api/v1/providers", adminKey, createProviderRequest{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: "https://api.example.com/v1",
		OAuth: &provider.OAuthConfig{
			ClientID:     "client",
			AuthorizeURL: "https://login.example.com/authorize",
			TokenURL:     "not-a-url",
			RedirectURI:  "https://login.example.com/callback",
		},
	}, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "invalid_request" {
		t.Fatalf("code = %q", code)
	}
}

func TestSyncRoutesEndpoint(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	}))
	t.Cleanup(upstream.Close)

	ctx := context.Background()
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "p1", Kind: provider.KindOpenAI, BaseURL: upstream.URL + "/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "", provider.CredentialAPIKey, "sk-live"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	reloaded := 0
	e.server.ReloadModels = func(context.Context) error {
		reloaded++
		return nil
	}

	resp := e.expect(t, http.MethodPost, "/api/v1/providers/p1/sync-routes", adminKey, nil, http.StatusOK)
	var result provider.SyncResult
	decodeBody(t, resp, &result)
	if result.Total != 2 || len(result.Created) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if reloaded != 1 {
		t.Fatalf("reloads = %d", reloaded)
	}

	resp = e.expect(t, http.MethodGet, "/api/v1/model-routes", readerKey, nil, http.StatusOK)
	var routes struct {
		Routes []provider.Route `json:"routes"`
	}
	decodeBody(t, resp, &routes)
	if len(routes.Routes) != 2 {
		t.Fatalf("routes = %+v", routes.Routes)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/providers/p1/sync-routes", readerKey, nil, http.StatusForbidden)
	_ = errCode(t, resp)
}

func TestDeviceFlowEndpoints(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)

	mux := http.NewServeMux()
	polls := 0
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"device_code":"d1","user_code":"WXYZ-9876","verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls < 2 {
			io.WriteString(w, `{"error":"authorization_pending"}`)
			return
		}
		io.WriteString(w, `{"access_token":"gh-token","token_type":"bearer"}`)
	})
	mux.HandleFunc("/exchange", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"token":"copilot-token","expires_at":1700003600}`)
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"login":"octocat"}`)
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	devices := oauth.NewDeviceRegistry()
	devices.Set(oauth.DeviceConfig{
		Kind: "github-copilot", ClientID: "cid",
		DeviceURL: fake.URL + "/device", TokenURL: fake.URL + "/token",
		ExchangeURL: fake.URL + "/exchange", UserURL: fake.URL + "/user",
	})
	svc.Devices = devices

	ctx := context.Background()
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "gh", Kind: provider.KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	resp := e.expect(t, http.MethodPost, "/api/v1/providers/gh/device/start", adminKey, nil, http.StatusOK)
	var start provider.DeviceStart
	decodeBody(t, resp, &start)
	if start.UserCode != "WXYZ-9876" || start.SessionID == "" {
		t.Fatalf("start = %+v", start)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/providers/gh/device/poll", adminKey, map[string]any{
		"session_id": start.SessionID,
	}, http.StatusOK)
	var pending provider.DevicePoll
	decodeBody(t, resp, &pending)
	if pending.Status != "pending" {
		t.Fatalf("pending = %+v", pending)
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/providers/gh/device/poll", adminKey, map[string]any{
		"session_id": start.SessionID,
	}, http.StatusOK)
	var complete provider.DevicePoll
	decodeBody(t, resp, &complete)
	if complete.Status != "complete" || complete.Credential == nil {
		t.Fatalf("complete = %+v", complete)
	}
	if complete.Credential.Account != "octocat" || complete.Credential.Kind != provider.CredentialOAuth {
		t.Fatalf("credential = %+v", complete.Credential)
	}
	if complete.Credential.SecretCiphertext != "" {
		t.Fatal("response leaked the sealed secret")
	}

	resp = e.expect(t, http.MethodPost, "/api/v1/providers/gh/device/start", readerKey, nil, http.StatusForbidden)
	_ = errCode(t, resp)

	// A channel without a device flow is rejected instead of guessing.
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "ds", Kind: provider.KindOpenAI, Channel: "deepseek",
		BaseURL: "https://api.deepseek.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider ds: %v", err)
	}
	resp = e.expect(t, http.MethodPost, "/api/v1/providers/ds/device/start", adminKey, nil, http.StatusUnprocessableEntity)
	if code := errCode(t, resp); code != "oauth_unsupported" {
		t.Fatalf("code = %q", code)
	}
}

func TestProxyListingExposesAuthMethods(t *testing.T) {
	e := newTestEnv(t)
	svc := e.enableProviders(t)
	ctx := context.Background()
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "gh", Kind: provider.KindOpenAI, Channel: "github-copilot",
		BaseURL: "https://api.githubcopilot.com",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateProvider(ctx, provider.Provider{
		ID: "ds", Kind: provider.KindOpenAI, Channel: "deepseek",
		BaseURL: "https://api.deepseek.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider ds: %v", err)
	}

	resp := e.expect(t, http.MethodGet, "/api/v1/providers", readerKey, nil, http.StatusOK)
	var list struct {
		Providers []provider.Provider `json:"providers"`
	}
	decodeBody(t, resp, &list)
	auths := map[string][]string{}
	for _, item := range list.Providers {
		auths[item.ID] = item.Auth
	}
	if len(auths["gh"]) == 0 || auths["gh"][0] != "device_code" {
		t.Fatalf("gh auth = %v", auths["gh"])
	}
	if len(auths["ds"]) == 0 || auths["ds"][0] != "api_key" {
		t.Fatalf("ds auth = %v", auths["ds"])
	}
}
