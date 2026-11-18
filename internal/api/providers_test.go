package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
