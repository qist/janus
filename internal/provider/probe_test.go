package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func addProvider(t *testing.T, svc *Service, baseURL string) Provider {
	t.Helper()
	created, err := svc.CreateProvider(context.Background(), Provider{
		ID: "p1", Kind: KindOpenAI, BaseURL: baseURL,
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	return created
}

func modelsServer(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if wantKey != "" && r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"m-1"},{"id":"m-2"},{"id":""}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeSuccessMarksCredentialOK(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	svc.ProbeTimeout = 2 * time.Second

	srv := modelsServer(t, "sk-test-key-1234")
	addProvider(t, svc, srv.URL+"/v1")
	if _, err := svc.CreateCredential(ctx, "p1", "primary", CredentialAPIKey, "sk-test-key-1234"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.Probe(ctx, "p1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !result.OK || result.Status != StatusOK {
		t.Fatalf("result = %+v", result)
	}
	// The upstream advertises three entries but one id is empty; only routable
	// ids count.
	if result.ModelCount != 2 || len(result.Models) != 2 {
		t.Fatalf("models = %d/%v", result.ModelCount, result.Models)
	}
	if result.Masked != "…1234" || result.CredentialID == "" {
		t.Fatalf("credential = %q/%q", result.CredentialID, result.Masked)
	}
	if result.LatencyMS < 0 {
		t.Fatalf("latency = %d", result.LatencyMS)
	}

	stored, err := svc.Store.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if stored[0].Status != StatusOK {
		t.Fatalf("stored status = %q", stored[0].Status)
	}
}

func TestProbeInvalidKeyMarksCredentialInvalid(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	svc.ProbeTimeout = 2 * time.Second

	srv := modelsServer(t, "sk-right")
	addProvider(t, svc, srv.URL+"/v1")
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-wrong"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.Probe(ctx, "p1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.OK || result.Status != StatusInvalid {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error, "401") {
		t.Fatalf("error = %q", result.Error)
	}

	stored, _ := svc.Store.ListCredentials(ctx, "p1")
	if stored[0].Status != StatusInvalid {
		t.Fatalf("stored status = %q", stored[0].Status)
	}
}

func TestProbeFailsOverToNextCredential(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	svc.ProbeTimeout = 2 * time.Second

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer quota-out" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"m-1"}]}`)
	}))
	t.Cleanup(srv.Close)

	addProvider(t, svc, srv.URL)
	first, err := svc.CreateCredential(ctx, "p1", "first", CredentialAPIKey, "quota-out")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	second, err := svc.CreateCredential(ctx, "p1", "second", CredentialAPIKey, "sk-good")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.Probe(ctx, "p1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !result.OK || result.CredentialID != second.ID {
		t.Fatalf("result = %+v", result)
	}

	stored, _ := svc.Store.ListCredentials(ctx, "p1")
	byID := map[string]Credential{}
	for _, credential := range stored {
		byID[credential.ID] = credential
	}
	if byID[first.ID].Status != StatusRateLimited {
		t.Fatalf("first status = %q", byID[first.ID].Status)
	}
	if byID[second.ID].Status != StatusOK {
		t.Fatalf("second status = %q", byID[second.ID].Status)
	}
}

func TestProbeWithoutCredentials(t *testing.T) {
	svc := testService(t)
	addProvider(t, svc, "https://api.example.com/v1")

	result, err := svc.Probe(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.OK || !strings.Contains(result.Error, "no credentials") {
		t.Fatalf("result = %+v", result)
	}
}

func TestProbeUnknownProvider(t *testing.T) {
	svc := testService(t)
	if _, err := svc.Probe(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Probe = %v, want ErrNotFound", err)
	}
}

func TestProbeUnreachableEndpoint(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	svc.ProbeTimeout = 300 * time.Millisecond

	// A closed port: the connection is refused immediately.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := closed.URL
	closed.Close()

	addProvider(t, svc, base)
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-123"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.Probe(ctx, "p1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.OK || result.Error == "" {
		t.Fatalf("result = %+v", result)
	}
	if result.Status != "" {
		t.Fatalf("status = %q, want empty for an infra failure", result.Status)
	}

	// The credential is not blamed for the endpoint being down.
	stored, _ := svc.Store.ListCredentials(ctx, "p1")
	if stored[0].Status != StatusUnknown {
		t.Fatalf("stored status = %q", stored[0].Status)
	}
}

func TestProbeDoesNotLeakSecretInErrors(t *testing.T) {
	svc := testService(t)
	svc.ProbeTimeout = 200 * time.Millisecond

	// Point at an unroutable address so the transport error is wrapped.
	addProvider(t, svc, "http://127.0.0.1:1/v1")
	const secret = "sk-should-not-appear"
	if _, err := svc.CreateCredential(context.Background(), "p1", "", CredentialAPIKey, secret); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	result, err := svc.Probe(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if strings.Contains(result.Error, secret) {
		t.Fatalf("probe error leaked the secret: %s", result.Error)
	}
	if got := sanitizeProbeError(errors.New("dial with "+secret+" failed"), secret); strings.Contains(got, secret) {
		t.Fatalf("sanitize left the secret: %s", got)
	}
}
