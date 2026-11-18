package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadDeepSeekQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("authorization header = %q", got)
		}
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"42.50"}]}`))
	}))
	defer server.Close()

	quota := readDeepSeek(context.Background(), server.URL+"/v1", "sk-test", 2*time.Second)
	if quota.Error != "" {
		t.Fatalf("unexpected error: %s", quota.Error)
	}
	if quota.Balance != "42.50" || quota.Currency != "CNY" {
		t.Fatalf("quota = %+v", quota)
	}
}

func TestReadOpenRouterQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/credits" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"total_credits":20,"total_usage":5.5}}`))
	}))
	defer server.Close()

	quota := readOpenRouter(context.Background(), server.URL+"/api/v1", "sk-test", 2*time.Second)
	if quota.Error != "" {
		t.Fatalf("unexpected error: %s", quota.Error)
	}
	if quota.Balance != "14.5000" || quota.Currency != "USD" {
		t.Fatalf("quota = %+v", quota)
	}
}

func TestQuotaHTTPErrorKeepsMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer server.Close()

	quota := readDeepSeek(context.Background(), server.URL, "sk-secret", 2*time.Second)
	if quota.Error == "" {
		t.Fatal("expected an error")
	}
	if quota.Balance != "" {
		t.Fatalf("balance should be empty on failure, got %q", quota.Balance)
	}
}

func TestNormalizePercent(t *testing.T) {
	if got := normalizePercent(0.25); got != 25 {
		t.Fatalf("fraction: got %v", got)
	}
	if got := normalizePercent(25); got != 25 {
		t.Fatalf("percent: got %v", got)
	}
	if got := normalizePercent(150); got != 150 {
		t.Fatalf("overspend: got %v", got)
	}
}
