package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"janus/internal/auth"
	"janus/internal/store"
)

const testAdminKey = "env-admin-key"

type fakeKeyStore struct {
	mu      sync.Mutex
	records map[string]auth.KeyRecord
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{records: map[string]auth.KeyRecord{}}
}

func (f *fakeKeyStore) SaveAPIKey(_ context.Context, key auth.KeyRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[key.ID] = key
	return nil
}

func (f *fakeKeyStore) ListAPIKeys(_ context.Context) ([]store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.APIKey, 0, len(f.records))
	for _, record := range f.records {
		out = append(out, publicKey(record))
	}
	return out, nil
}

func (f *fakeKeyStore) GetAPIKey(_ context.Context, id string) (store.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.records[id]
	if !ok {
		return store.APIKey{}, auth.ErrKeyNotFound
	}
	return publicKey(record), nil
}

func (f *fakeKeyStore) RevokeAPIKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.records[id]
	if !ok {
		return auth.ErrKeyNotFound
	}
	record.Revoked = true
	f.records[id] = record
	return nil
}

func (f *fakeKeyStore) LookupAPIKey(_ context.Context, digest string) (auth.KeyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.records {
		if record.Hash == digest {
			return record, nil
		}
	}
	return auth.KeyRecord{}, auth.ErrKeyNotFound
}

func (f *fakeKeyStore) TouchAPIKey(context.Context, string) {}

func publicKey(record auth.KeyRecord) store.APIKey {
	return store.APIKey{
		ID:         record.ID,
		Kind:       record.Kind,
		Name:       record.Name,
		Prefix:     record.Prefix,
		Scopes:     record.Scopes,
		Notes:      record.Notes,
		ExpiresAt:  record.ExpiresAt,
		Revoked:    record.Revoked,
		LastUsedAt: record.LastUsedAt,
		CreatedAt:  record.CreatedAt,
	}
}

func newKeyEnv(t *testing.T) (*httptest.Server, *auth.DynamicKeys) {
	t.Helper()
	static := NewStaticKeys()
	static.Add(testAdminKey, Identity{AgentID: "admin", Scopes: []Scope{ScopeAdmin}})

	keys := newFakeKeyStore()
	dynamic := auth.NewDynamicKeys(keys)

	server := New(nil, auth.NewChain(static, dynamic))
	server.Keys = keys
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts, dynamic
}

func keyRequest(t *testing.T, ts *httptest.Server, method, path, key, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func decodeKey(t *testing.T, resp *http.Response) createKeyResponse {
	t.Helper()
	defer resp.Body.Close()
	var out createKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestCreateKeyReturnsPlaintextOnce(t *testing.T) {
	ts, dynamic := newKeyEnv(t)

	resp := keyRequest(t, ts, http.MethodPost, "/api/v1/keys", testAdminKey,
		`{"kind":"client","name":"console app","expires_in_days":30}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	out := decodeKey(t, resp)
	if !strings.HasPrefix(out.Plaintext, "sk-janus-c-") {
		t.Fatalf("plaintext = %q", out.Plaintext)
	}
	if out.Key.ID == "" || out.Key.Kind != auth.KeyKindClient || out.Key.ExpiresAt == 0 {
		t.Fatalf("key = %+v", out.Key)
	}

	identity, err := dynamic.Authenticate(context.Background(), out.Plaintext)
	if err != nil {
		t.Fatalf("stored key should authenticate: %v", err)
	}
	if identity.Kind != auth.KeyKindClient {
		t.Fatalf("identity = %+v", identity)
	}

	list := keyRequest(t, ts, http.MethodGet, "/api/v1/keys", testAdminKey, "")
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", list.StatusCode)
	}
	var payload struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(list.Body).Decode(&payload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(payload.Keys) != 1 {
		t.Fatalf("keys = %+v", payload.Keys)
	}
	if _, leaked := payload.Keys[0]["hash"]; leaked {
		t.Fatal("list must not expose the stored hash")
	}
}

func TestClientKeyCannotUseAdminAPI(t *testing.T) {
	ts, dynamic := newKeyEnv(t)

	resp := keyRequest(t, ts, http.MethodPost, "/api/v1/keys", testAdminKey,
		`{"kind":"client","name":"app"}`)
	out := decodeKey(t, resp)

	denied := keyRequest(t, ts, http.MethodGet, "/api/v1/keys", out.Plaintext, "")
	defer denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", denied.StatusCode)
	}

	me := keyRequest(t, ts, http.MethodGet, "/api/v1/me", out.Plaintext, "")
	defer me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/me status = %d", me.StatusCode)
	}
	if identity, err := dynamic.Authenticate(context.Background(), out.Plaintext); err != nil || identity.Kind != auth.KeyKindClient {
		t.Fatalf("dynamic identity = %+v, %v", identity, err)
	}
}

func TestRotateKeyInvalidatesOldPlaintext(t *testing.T) {
	ts, dynamic := newKeyEnv(t)

	created := decodeKey(t, keyRequest(t, ts, http.MethodPost, "/api/v1/keys", testAdminKey,
		`{"kind":"agent","name":"worker"}`))

	rotated := keyRequest(t, ts, http.MethodPost, "/api/v1/keys/"+created.Key.ID+"/rotate", testAdminKey, "")
	if rotated.StatusCode != http.StatusCreated {
		t.Fatalf("rotate status = %d", rotated.StatusCode)
	}
	out := decodeKey(t, rotated)
	if out.Plaintext == created.Plaintext || out.Key.ID == created.Key.ID {
		t.Fatal("rotate must mint a new key")
	}
	if _, err := dynamic.Authenticate(context.Background(), created.Plaintext); err == nil {
		t.Fatal("old plaintext should stop working")
	}
	if _, err := dynamic.Authenticate(context.Background(), out.Plaintext); err != nil {
		t.Fatalf("new plaintext should work: %v", err)
	}
}

func TestRevokeKey(t *testing.T) {
	ts, dynamic := newKeyEnv(t)

	created := decodeKey(t, keyRequest(t, ts, http.MethodPost, "/api/v1/keys", testAdminKey,
		`{"kind":"tool","name":"mcp"}`))

	resp := keyRequest(t, ts, http.MethodDelete, "/api/v1/keys/"+created.Key.ID, testAdminKey, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	if _, err := dynamic.Authenticate(context.Background(), created.Plaintext); err == nil {
		t.Fatal("revoked key must not authenticate")
	}
	missing := keyRequest(t, ts, http.MethodDelete, "/api/v1/keys/key_missing", testAdminKey, "")
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing revoke status = %d", missing.StatusCode)
	}
}
