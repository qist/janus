package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	var seen string
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))

	// Plain HTTP must not advertise HSTS: the browser would ignore it, and on a
	// migration port the claim would be false.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("HSTS on plain HTTP: %q", rec.Header().Get("Strict-Transport-Security"))
	}
	for _, name := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if rec.Header().Get(name) == "" {
			t.Fatalf("missing %s", name)
		}
	}
	if seen != "/api/v1/healthz" {
		t.Fatalf("handler not reached: %q", seen)
	}

	tlsReq := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	tlsReq.TLS = &tlsState{}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, tlsReq)
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("missing HSTS on TLS")
	}
}
