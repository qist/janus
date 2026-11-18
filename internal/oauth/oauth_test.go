package oauth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testConfig(tokenURL string) Config {
	cfg, _ := DefaultRegistry().Get("openai")
	cfg.TokenURL = tokenURL
	return cfg
}

func TestAuthURLContainsPKCE(t *testing.T) {
	cfg := testConfig("https://example.com/token")
	verifier, err := NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if len(verifier) != 128 {
		t.Fatalf("hex verifier length = %d, want 128", len(verifier))
	}
	state, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}

	raw, err := cfg.AuthURL(state, verifier)
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := parsed.Query()
	if query.Get("client_id") != cfg.ClientID {
		t.Fatalf("client_id = %q", query.Get("client_id"))
	}
	if query.Get("state") != state {
		t.Fatalf("state = %q", query.Get("state"))
	}
	if query.Get("code_challenge") != Challenge(verifier) {
		t.Fatalf("challenge mismatch")
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("method = %q", query.Get("code_challenge_method"))
	}
	if !strings.Contains(query.Get("scope"), "offline_access") {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
	if query.Get("redirect_uri") != cfg.RedirectURI {
		t.Fatalf("redirect = %q", query.Get("redirect_uri"))
	}
}

func TestVerifierEncoding(t *testing.T) {
	hexCfg := testConfig("")
	hexVerifier, err := NewVerifier(hexCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hex.DecodeString(hexVerifier); err != nil {
		t.Fatalf("hex verifier is not hex: %v", err)
	}

	claude, _ := DefaultRegistry().Get("anthropic")
	verifier, err := NewVerifier(claude)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(verifier, "+/=") {
		t.Fatalf("base64url verifier has padding or unsafe chars: %q", verifier)
	}
	if len(verifier) < 40 {
		t.Fatalf("verifier too short: %d", len(verifier))
	}
}

func TestExchangeForm(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(raw))
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", ct)
		}
		io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1","token_type":"Bearer","expires_in":3600,"scope":"openid"}`)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	tokens, err := cfg.Exchange(context.Background(), "code-1", "verifier-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tokens.AccessToken != "at-1" || tokens.RefreshToken != "rt-1" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokens.ExpiresAt == 0 {
		t.Fatal("ExpiresAt not set from expires_in")
	}
	if got.Get("grant_type") != "authorization_code" || got.Get("code") != "code-1" ||
		got.Get("code_verifier") != "verifier-1" || got.Get("client_id") != cfg.ClientID {
		t.Fatalf("form = %v", got)
	}
}

func TestExchangeJSON(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"access_token":"claude-at","expires_in":60}`)
	}))
	defer srv.Close()

	cfg, _ := DefaultRegistry().Get("anthropic")
	cfg.TokenURL = srv.URL
	tokens, err := cfg.Exchange(context.Background(), "code-2", "verifier-2")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tokens.AccessToken != "claude-at" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if body["grant_type"] != "authorization_code" || body["code"] != "code-2" ||
		body["code_verifier"] != "verifier-2" {
		t.Fatalf("body = %v", body)
	}
}

func TestExchangeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"code expired"}`)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	_, err := cfg.Exchange(context.Background(), "bad", "v")
	if !errors.Is(err, ErrExchange) {
		t.Fatalf("err = %v, want ErrExchange", err)
	}
	if !strings.Contains(err.Error(), "code expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefresh(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(raw))
		io.WriteString(w, `{"access_token":"new-at"}`)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	tokens, err := cfg.Refresh(context.Background(), "rt-9")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tokens.AccessToken != "new-at" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-9" {
		t.Fatalf("form = %v", form)
	}
}

func TestExpired(t *testing.T) {
	tokens := TokenSet{ExpiresAt: 1000}
	if tokens.Expired(800, 120) {
		t.Fatal("token should still be fresh")
	}
	if !tokens.Expired(890, 120) {
		t.Fatal("token inside the skew must be treated as expiring")
	}
	if !tokens.Expired(1200, 120) {
		t.Fatal("past expiry must be expired")
	}
	if (TokenSet{}).Expired(1<<40, 120) {
		t.Fatal("a set without expiry must not expire")
	}
}
