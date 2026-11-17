package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteSuccess(t *testing.T) {
	var got struct {
		path   string
		auth   string
		body   map[string]any
		method string
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.path = r.URL.Path
		got.method = r.Method
		got.auth = r.Header.Get("Authorization")
		_ = json.Unmarshal(raw, &got.body)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m-returned","choices":[{"message":{"role":"assistant","content":"{\"verdict\":\"allow\"}"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	}))
	defer srv.Close()

	c, err := New(Config{Name: "test", BaseURL: srv.URL + "/v1", APIKey: "secret-key", Model: "m-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := c.Complete(context.Background(), Request{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"verdict":"allow"}` {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.Model != "m-returned" || resp.PromptTokens != 11 || resp.CompletionTokens != 7 {
		t.Fatalf("response = %+v", resp)
	}

	if got.method != http.MethodPost || got.path != "/v1/chat/completions" {
		t.Fatalf("request = %s %s", got.method, got.path)
	}
	if got.auth != "Bearer secret-key" {
		t.Fatalf("authorization = %q", got.auth)
	}
	if got.body["model"] != "m-1" {
		t.Fatalf("model = %v", got.body["model"])
	}
	messages, _ := got.body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", got.body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	second, _ := messages[1].(map[string]any)
	if first["role"] != "system" || first["content"] != "sys" {
		t.Fatalf("system message = %v", first)
	}
	if second["role"] != "user" || second["content"] != "usr" {
		t.Fatalf("user message = %v", second)
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL + "/v1/", Model: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Complete(context.Background(), Request{User: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestHTTPErrorIncludesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), Request{User: "x"})
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
}

func TestMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), Request{User: "x"})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), Request{User: "x"})
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptyCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"   "}}]}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), Request{User: "x"})
	if err == nil || !strings.Contains(err.Error(), "empty completion") {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad model"}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), Request{User: "x"})
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIKeyFromEnv(t *testing.T) {
	t.Setenv("TEST_AUDITOR_KEY", "env-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer env-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Model: "m", APIKeyEnv: "TEST_AUDITOR_KEY"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Complete(context.Background(), Request{User: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestMissingEnvKeyFailsFast(t *testing.T) {
	_, err := New(Config{BaseURL: "https://example.com/v1", Model: "m", APIKeyEnv: "JANUS_MISSING_KEY_FOR_TEST"})
	if err == nil {
		t.Fatal("expected an error for a missing api_key_env")
	}
}

func TestNewValidation(t *testing.T) {
	cases := []Config{
		{},
		{BaseURL: "https://example.com/v1"},
		{Model: "m"},
		{BaseURL: "ftp://example.com", Model: "m"},
	}
	for _, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Fatalf("config %+v accepted", cfg)
		}
	}
}

func TestNameDefaultsToModel(t *testing.T) {
	c, err := New(Config{BaseURL: "https://example.com/v1", Model: "m-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Name != "m-1" {
		t.Fatalf("name = %q", c.Name)
	}
}

func TestContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m", TimeoutSeconds: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Complete(ctx, Request{User: "x"}); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestTemperatureAndMaxTokens(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	temp := 0.0
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", MaxTokens: 128, Temperature: &temp})
	if _, err := c.Complete(context.Background(), Request{User: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if body["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	if body["temperature"] != float64(0) {
		t.Fatalf("temperature = %v", body["temperature"])
	}
}

func TestRequestOverridesModel(t *testing.T) {
	var model string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		model, _ = body["model"].(string)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "default-model"})
	if _, err := c.Complete(context.Background(), Request{User: "x", Model: "other-model"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if model != "other-model" {
		t.Fatalf("model = %q", model)
	}
}

type credentialReport struct {
	id     string
	status int
	body   string
}

type fakeSource struct {
	creds   []Credential
	idx     int
	reports []credentialReport
}

func (f *fakeSource) Next(context.Context, string, string) (Credential, error) {
	if f.idx >= len(f.creds) {
		return Credential{}, errors.New("pool empty")
	}
	credential := f.creds[f.idx]
	f.idx++
	return credential, nil
}

func (f *fakeSource) Report(_ context.Context, id string, status int, body string, _ time.Duration) {
	f.reports = append(f.reports, credentialReport{id: id, status: status, body: body})
}

func TestFailoverOnQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":"insufficient_quota","message":"out of credits"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	source := &fakeSource{creds: []Credential{{ID: "a1", APIKey: "k1"}, {ID: "a2", APIKey: "k2"}}}
	c.Credentials = source

	resp, err := c.Complete(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("text = %q", resp.Text)
	}
	if source.idx != 2 {
		t.Fatalf("credentials tried = %d, want 2", source.idx)
	}
	if len(source.reports) != 2 || source.reports[0].status != 429 || source.reports[1].status != 200 {
		t.Fatalf("reports = %+v", source.reports)
	}
}

func TestNoFailoverOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "upstream down")
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	source := &fakeSource{creds: []Credential{{ID: "a1", APIKey: "k1"}, {ID: "a2", APIKey: "k2"}}}
	c.Credentials = source

	if _, err := c.Complete(context.Background(), Request{User: "x"}); err == nil {
		t.Fatal("expected an error")
	}
	if source.idx != 1 {
		t.Fatalf("credentials tried = %d, want 1 (5xx is not account-specific)", source.idx)
	}
	if len(source.reports) != 1 || source.reports[0].status != 503 {
		t.Fatalf("reports = %+v", source.reports)
	}
}

func TestFailoverOnInvalidAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "bad key")
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	source := &fakeSource{creds: []Credential{{ID: "a1", APIKey: "bad"}, {ID: "a2", APIKey: "good"}}}
	c.Credentials = source

	if _, err := c.Complete(context.Background(), Request{User: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestAllCredentialsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	source := &fakeSource{creds: []Credential{{ID: "a1", APIKey: "k1"}, {ID: "a2", APIKey: "k2"}}}
	c.Credentials = source

	_, err := c.Complete(context.Background(), Request{User: "x"})
	if err == nil || !strings.Contains(err.Error(), "pool empty") {
		t.Fatalf("err = %v", err)
	}
	if source.idx != 2 {
		t.Fatalf("credentials tried = %d, want 2", source.idx)
	}
}

func TestSuccessReportsCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	source := &fakeSource{creds: []Credential{{ID: "a1", APIKey: "k1"}}}
	c.Credentials = source

	if _, err := c.Complete(context.Background(), Request{User: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(source.reports) != 1 || source.reports[0].status != 200 || source.reports[0].id != "a1" {
		t.Fatalf("reports = %+v", source.reports)
	}
}

func TestUsageCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"m-returned","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	source := &fakeSource{creds: []Credential{{ID: "a1", APIKey: "k1"}}}
	c.Credentials = source

	var got []Usage
	c.OnUsage = func(u Usage) { got = append(got, u) }

	if _, err := c.Complete(context.Background(), Request{User: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("usage callbacks = %d", len(got))
	}
	u := got[0]
	if u.ProviderID != c.Name || u.CredentialID != "a1" || u.Model != "m-returned" {
		t.Fatalf("usage = %+v", u)
	}
	if u.PromptTokens != 9 || u.CompletionTokens != 4 {
		t.Fatalf("tokens = %+v", u)
	}
}

func TestNoUsageOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Model: "m"})
	called := false
	c.OnUsage = func(Usage) { called = true }

	if _, err := c.Complete(context.Background(), Request{User: "x"}); err == nil {
		t.Fatal("expected an error")
	}
	if called {
		t.Fatal("usage callback fired for a failed call")
	}
}
