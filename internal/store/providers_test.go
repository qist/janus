package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"janus/internal/provider"
)

func openProviderStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestProviderCredentialRouteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, path := openProviderStore(t)

	if err := s.SaveProvider(ctx, provider.Provider{
		ID: "p1", Name: "openai", Kind: provider.KindOpenAI,
		BaseURL: "https://api.example.com/v1", Enabled: true, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	if err := s.SaveCredential(ctx, provider.Credential{
		ID: "cred_1", ProviderID: "p1", Label: "primary", Kind: provider.CredentialAPIKey,
		Status: provider.StatusOK, Masked: "…ijkl", HasSecret: true,
		SecretCiphertext: "sealed-blob", Total: 100, Remaining: 90, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if err := s.SaveRoute(ctx, provider.Route{
		ID: "route_1", ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini",
		Aliases: []string{"turbo"}, Priority: 1, Enabled: true, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("SaveRoute: %v", err)
	}

	// Reopen: the ciphertext must survive, because clients are rebuilt from it.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	p, err := reopened.GetProvider(ctx, "p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if p.Kind != provider.KindOpenAI || !p.Enabled {
		t.Fatalf("provider = %+v", p)
	}

	credentials, err := reopened.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(credentials) != 1 || credentials[0].SecretCiphertext != "sealed-blob" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if credentials[0].Masked != "…ijkl" || credentials[0].Remaining != 90 {
		t.Fatalf("credential = %+v", credentials[0])
	}

	routes, err := reopened.ListRoutes(ctx)
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 1 || len(routes[0].Aliases) != 1 || routes[0].Aliases[0] != "turbo" {
		t.Fatalf("routes = %+v", routes)
	}
	if routes[0].UpstreamModel != "gpt-4o-mini" || routes[0].Priority != 1 {
		t.Fatalf("route = %+v", routes[0])
	}
}

func TestProviderStoreNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := openProviderStore(t)

	if _, err := s.GetProvider(ctx, "missing"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("GetProvider = %v", err)
	}
	if err := s.DeleteProvider(ctx, "missing"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("DeleteProvider = %v", err)
	}
	if err := s.DeleteCredential(ctx, "missing"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("DeleteCredential = %v", err)
	}
	if err := s.DeleteRoute(ctx, "missing"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("DeleteRoute = %v", err)
	}
}

func TestCredentialJSONNeverCarriesSecret(t *testing.T) {
	raw, err := json.Marshal(provider.Credential{
		ID: "cred_1", Masked: "…ijkl", HasSecret: true, SecretCiphertext: "sealed-blob",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "sealed-blob") || !strings.Contains(string(raw), "…ijkl") {
		t.Fatalf("json = %s", raw)
	}
}

func TestListCredentialsFilter(t *testing.T) {
	ctx := context.Background()
	s, _ := openProviderStore(t)

	for _, credential := range []provider.Credential{
		{ID: "c1", ProviderID: "p1", Kind: provider.CredentialAPIKey, Status: provider.StatusOK, SecretCiphertext: "a"},
		{ID: "c2", ProviderID: "p2", Kind: provider.CredentialAPIKey, Status: provider.StatusOK, SecretCiphertext: "b"},
	} {
		if err := s.SaveCredential(ctx, credential); err != nil {
			t.Fatalf("SaveCredential: %v", err)
		}
	}

	all, err := s.ListCredentials(ctx, "")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %d", len(all))
	}

	p1, err := s.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("ListCredentials(p1): %v", err)
	}
	if len(p1) != 1 || p1[0].ID != "c1" {
		t.Fatalf("p1 = %+v", p1)
	}
}

func TestUpdateCredentialHealthPreservesSecret(t *testing.T) {
	ctx := context.Background()
	s, _ := openProviderStore(t)
	if err := s.SaveCredential(ctx, provider.Credential{
		ID: "cred_1", ProviderID: "p1", Label: "primary", Kind: provider.CredentialOAuth,
		Status: provider.StatusOK, Masked: "…ijkl", HasSecret: true,
		SecretCiphertext: "sealed-blob", Remaining: 90, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if err := s.UpdateCredentialHealth(ctx, provider.CredentialHealth{
		ID: "cred_1", Status: provider.StatusExhausted, ResetsAt: 500,
		LastUsedAt: 200, LastError: "quota",
	}); err != nil {
		t.Fatalf("UpdateCredentialHealth: %v", err)
	}
	list, err := s.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("credentials = %d", len(list))
	}
	got := list[0]
	if got.SecretCiphertext != "sealed-blob" {
		t.Fatalf("health write clobbered the secret: %q", got.SecretCiphertext)
	}
	if got.Status != provider.StatusExhausted || got.ResetsAt != 500 || got.Remaining != 0 || got.LastError != "quota" {
		t.Fatalf("health = %+v", got)
	}
}

func TestProviderOAuthConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openProviderStore(t)
	if err := s.SaveProvider(ctx, provider.Provider{
		ID: "wb", Kind: provider.KindOpenAI, BaseURL: "https://copilot.tencent.com/v2",
		Channel: "workbuddy", Enabled: true,
		OAuth: &provider.OAuthConfig{
			ClientID: "cid", AuthorizeURL: "https://login.example.com/a",
			TokenURL: "https://login.example.com/t", RedirectURI: "https://login.example.com/cb",
			Scopes: []string{"openid"}, TokenStyle: "json",
		},
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	got, err := s.GetProvider(ctx, "wb")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.OAuth == nil || got.OAuth.ClientID != "cid" || len(got.OAuth.Scopes) != 1 {
		t.Fatalf("oauth = %+v", got.OAuth)
	}
}
