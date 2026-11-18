package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

func modelsServerFor(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-live" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		data := make([]map[string]any, 0, len(models))
		for _, model := range models {
			data = append(data, map[string]any{"id": model})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSyncRoutesCreatesMissingAndReportsStale(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	upstream := modelsServerFor(t, "m1", "m2", "m3")

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "p1", Kind: KindOpenAI, BaseURL: upstream.URL + "/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "primary", CredentialAPIKey, "sk-live"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "m1", ProviderID: "p1", UpstreamModel: "m1",
	}); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "old", ProviderID: "p1", UpstreamModel: "old",
	}); err != nil {
		t.Fatalf("CreateRoute stale: %v", err)
	}

	result, err := svc.SyncRoutes(ctx, "p1", SyncOptions{})
	if err != nil {
		t.Fatalf("SyncRoutes: %v", err)
	}
	if result.Total != 3 || result.Error != "" {
		t.Fatalf("result = %+v", result)
	}
	sort.Strings(result.Created)
	if len(result.Created) != 2 || result.Created[0] != "m2" || result.Created[1] != "m3" {
		t.Fatalf("created = %v", result.Created)
	}
	if len(result.Existing) != 1 || result.Existing[0] != "m1" {
		t.Fatalf("existing = %v", result.Existing)
	}
	if len(result.Stale) != 1 || result.Stale[0] != "old" {
		t.Fatalf("stale = %v", result.Stale)
	}
	if len(result.Pruned) != 0 {
		t.Fatalf("pruned without prune option: %v", result.Pruned)
	}

	// Without prune the stale route still resolves; with prune it is gone.
	routes, _ := svc.ListRoutes(ctx)
	if len(routes) != 4 {
		t.Fatalf("routes = %d", len(routes))
	}
	result, err = svc.SyncRoutes(ctx, "p1", SyncOptions{Prune: true})
	if err != nil {
		t.Fatalf("SyncRoutes prune: %v", err)
	}
	if len(result.Pruned) != 1 || result.Pruned[0] != "old" {
		t.Fatalf("pruned = %v", result.Pruned)
	}
	if len(result.Created) != 0 {
		t.Fatalf("second sync created duplicates: %v", result.Created)
	}
	routes, _ = svc.ListRoutes(ctx)
	if len(routes) != 3 {
		t.Fatalf("routes after prune = %d", len(routes))
	}
}

func TestSyncRoutesDoesNotStealExposedName(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	upstream := modelsServerFor(t, "shared")

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "p1", Kind: KindOpenAI, BaseURL: upstream.URL + "/v1",
	}); err != nil {
		t.Fatalf("CreateProvider p1: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-live"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "p2", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("CreateProvider p2: %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "shared", ProviderID: "p2", UpstreamModel: "shared",
	}); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	result, err := svc.SyncRoutes(ctx, "p1", SyncOptions{})
	if err != nil {
		t.Fatalf("SyncRoutes: %v", err)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0] != "shared" {
		t.Fatalf("conflicts = %v", result.Conflicts)
	}
	if len(result.Created) != 0 {
		t.Fatalf("created = %v", result.Created)
	}
	routes, _ := svc.ListRoutes(ctx)
	if len(routes) != 1 || routes[0].ProviderID != "p2" {
		t.Fatalf("routes = %+v", routes)
	}
}

func TestSyncRoutesBadCredentialReportsStatus(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	upstream := modelsServerFor(t, "m1")

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "p1", Kind: KindOpenAI, BaseURL: upstream.URL + "/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	credential, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-wrong")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.SyncRoutes(ctx, "p1", SyncOptions{})
	if err != nil {
		t.Fatalf("SyncRoutes: %v", err)
	}
	if result.Error == "" {
		t.Fatal("expected an error message")
	}
	stored, _ := svc.ListCredentials(ctx, "p1")
	for _, item := range stored {
		if item.ID == credential.ID && item.Status != StatusInvalid {
			t.Fatalf("status = %q, want invalid", item.Status)
		}
	}
	if !errors.Is(err, nil) && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSyncRoutesSkipsAliases(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	upstream := modelsServerFor(t, "m1", "janus", "default")

	if _, err := svc.CreateProvider(ctx, Provider{
		ID: "p1", Kind: KindOpenAI, BaseURL: upstream.URL + "/v1",
	}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-live"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.SyncRoutes(ctx, "p1", SyncOptions{Skip: []string{"janus", "default"}})
	if err != nil {
		t.Fatalf("SyncRoutes: %v", err)
	}
	if len(result.Created) != 1 || result.Created[0] != "m1" {
		t.Fatalf("created = %v", result.Created)
	}
	if len(result.Skipped) != 2 {
		t.Fatalf("skipped = %v", result.Skipped)
	}
	if result.Total != 3 {
		t.Fatalf("total = %d", result.Total)
	}
}
