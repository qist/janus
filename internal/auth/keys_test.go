package auth

import (
	"context"
	"errors"
	"testing"
)

type mapKeySource struct {
	records map[string]KeyRecord
	touched []string
}

func (m *mapKeySource) LookupAPIKey(_ context.Context, digest string) (KeyRecord, error) {
	record, ok := m.records[digest]
	if !ok {
		return KeyRecord{}, ErrKeyNotFound
	}
	return record, nil
}

func (m *mapKeySource) TouchAPIKey(_ context.Context, id string) {
	m.touched = append(m.touched, id)
}

func TestNewKeyRecordPrefixPerKind(t *testing.T) {
	cases := map[string]string{
		KeyKindAdmin:  "sk-janus-a-",
		KeyKindClient: "sk-janus-c-",
		KeyKindAgent:  "sk-janus-g-",
		KeyKindTool:   "sk-janus-t-",
	}
	for kind, prefix := range cases {
		plaintext, record, err := NewKeyRecord(kind, "", "", 0, func() int64 { return 100 })
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(plaintext) <= len(prefix) || plaintext[:len(prefix)] != prefix {
			t.Fatalf("%s: plaintext %q lacks prefix %q", kind, plaintext, prefix)
		}
		if record.Hash != HashKey(plaintext) {
			t.Fatalf("%s: hash does not match plaintext", kind)
		}
		if record.Name != kind {
			t.Fatalf("%s: name = %q", kind, record.Name)
		}
		if record.CreatedAt != 100 {
			t.Fatalf("%s: created_at = %d", kind, record.CreatedAt)
		}
	}
}

func TestNewKeyRecordRejectsUnknownKind(t *testing.T) {
	if _, _, err := NewKeyRecord("root", "", "", 0, nil); err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
}

func TestDynamicKeysResolvesStoredKey(t *testing.T) {
	plaintext, record, err := NewKeyRecord(KeyKindAgent, "worker", "", 0, nil)
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}
	source := &mapKeySource{records: map[string]KeyRecord{record.Hash: record}}
	keys := NewDynamicKeys(source)

	identity, err := keys.Authenticate(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.AgentID != record.ID || identity.Kind != KeyKindAgent {
		t.Fatalf("identity = %+v", identity)
	}
	if !identity.Has(ScopeChat) {
		t.Fatalf("agent key should be able to chat: %+v", identity.Scopes)
	}
	if identity.Has(ScopeAdmin) {
		t.Fatal("agent key must not be admin")
	}
}

func TestDynamicKeysRejectsUnknownRevokedAndExpired(t *testing.T) {
	ctx := context.Background()

	plaintext, record, err := NewKeyRecord(KeyKindClient, "app", "", 0, nil)
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}

	cases := []struct {
		name   string
		record KeyRecord
		now    int64
	}{
		{name: "revoked", record: withRevoked(record), now: 50},
		{name: "expired", record: withExpiry(record, 10), now: 10},
	}
	for _, tc := range cases {
		source := &mapKeySource{records: map[string]KeyRecord{tc.record.Hash: tc.record}}
		keys := NewDynamicKeys(source)
		keys.Now = func() int64 { return tc.now }
		if _, err := keys.Authenticate(ctx, plaintext); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s: err = %v, want ErrUnauthenticated", tc.name, err)
		}
	}

	if _, err := NewDynamicKeys(&mapKeySource{records: map[string]KeyRecord{}}).
		Authenticate(ctx, plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown key: err = %v", err)
	}
}

func TestDynamicKeysHonoursNarrowerScopes(t *testing.T) {
	plaintext, record, err := NewKeyRecord(KeyKindAgent, "worker", "", 0, nil)
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}
	record.Scopes = []Scope{ScopeRead}
	source := &mapKeySource{records: map[string]KeyRecord{record.Hash: record}}

	identity, err := NewDynamicKeys(source).Authenticate(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Has(ScopeChat) {
		t.Fatalf("stored scopes must narrow the kind default: %+v", identity.Scopes)
	}
}

func TestChainFallsBackToSecondAuthenticator(t *testing.T) {
	static := NewStaticKeys()
	static.Add("env-key", Identity{AgentID: "admin", Scopes: []Scope{ScopeAdmin}})

	plaintext, record, err := NewKeyRecord(KeyKindClient, "app", "", 0, nil)
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}
	dynamic := NewDynamicKeys(&mapKeySource{records: map[string]KeyRecord{record.Hash: record}})
	chain := NewChain(static, dynamic)

	ctx := context.Background()
	if identity, err := chain.Authenticate(ctx, "env-key"); err != nil || identity.AgentID != "admin" {
		t.Fatalf("static key: %+v, %v", identity, err)
	}
	if identity, err := chain.Authenticate(ctx, plaintext); err != nil || identity.Kind != KeyKindClient {
		t.Fatalf("dynamic key: %+v, %v", identity, err)
	}
	if _, err := chain.Authenticate(ctx, "nope"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown key: err = %v", err)
	}
}

func withRevoked(record KeyRecord) KeyRecord {
	record.Revoked = true
	return record
}

func withExpiry(record KeyRecord, expiresAt int64) KeyRecord {
	record.ExpiresAt = expiresAt
	return record
}
