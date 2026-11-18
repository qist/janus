package provider

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const testKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func testBox(t *testing.T) *SecretBox {
	t.Helper()
	key, err := ParseMasterKey(testKeyHex)
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}
	return box
}

func testService(t *testing.T) *Service {
	t.Helper()
	now := int64(1_700_000_000)
	return &Service{
		Store: NewMemoryStore(),
		Box:   testBox(t),
		Now:   func() int64 { return now },
	}
}

func TestParseMasterKeyForms(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")

	cases := map[string]string{
		"hex":    hex.EncodeToString(raw),
		"base64": base64.StdEncoding.EncodeToString(raw),
		"raw":    string(raw),
	}
	for name, encoded := range cases {
		got, err := ParseMasterKey(encoded)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != string(raw) {
			t.Fatalf("%s: key = %x", name, got)
		}
	}

	if _, err := ParseMasterKey("short"); err == nil {
		t.Fatal("accepted a short key")
	}
	if _, err := ParseMasterKey(""); err == nil {
		t.Fatal("accepted an empty key")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	box := testBox(t)

	sealed, err := box.Seal("sk-live-1234567890")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "sk-live") {
		t.Fatal("ciphertext contains the plaintext")
	}

	plain, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if plain != "sk-live-1234567890" {
		t.Fatalf("plain = %q", plain)
	}

	// Nonce is random, so sealing twice must differ.
	again, _ := box.Seal("sk-live-1234567890")
	if again == sealed {
		t.Fatal("two seals produced the same ciphertext")
	}
}

func TestOpenRejectsWrongKeyAndTamper(t *testing.T) {
	box := testBox(t)
	sealed, err := box.Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, err := ParseMasterKey(hex.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	wrong, _ := NewSecretBox(other)
	if _, err := wrong.Open(sealed); err == nil {
		t.Fatal("wrong key decrypted the secret")
	}

	raw, _ := base64.RawURLEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 0xff
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := box.Open(tampered); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestMask(t *testing.T) {
	if got := Mask("sk-abcdefghijklmnop"); got != "…mnop" {
		t.Fatalf("mask = %q", got)
	}
	if got := Mask("short"); got != "••••" {
		t.Fatalf("short mask = %q", got)
	}
}

func TestCreateProviderValidation(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)

	cases := []Provider{
		{Kind: KindOpenAI, BaseURL: "https://api.example.com"},
		{ID: "p1", Kind: "bogus", BaseURL: "https://api.example.com"},
		{ID: "p1", Kind: KindOpenAI, BaseURL: "ftp://api.example.com"},
		{ID: "p1", Kind: KindOpenAI},
	}
	for _, tc := range cases {
		if _, err := svc.CreateProvider(ctx, tc); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%+v: err = %v, want ErrInvalidInput", tc, err)
		}
	}

	created, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com/"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if created.Name != "p1" || !created.Enabled || created.BaseURL != "https://api.example.com" {
		t.Fatalf("created = %+v", created)
	}

	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate = %v, want ErrConflict", err)
	}
}

func TestCredentialIsMaskedEverywhere(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	created, err := svc.CreateCredential(ctx, "p1", "primary", CredentialAPIKey, "sk-live-abcdefghijkl")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if created.SecretCiphertext != "" {
		t.Fatal("created credential leaks ciphertext")
	}
	if created.Masked != "…ijkl" || !created.HasSecret {
		t.Fatalf("created = %+v", created)
	}

	list, err := svc.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(list) != 1 || list[0].SecretCiphertext != "" {
		t.Fatalf("list = %+v", list)
	}

	// The store keeps the sealed value, so the service can rebuild a client.
	stored, err := svc.Store.ListCredentials(ctx, "p1")
	if err != nil {
		t.Fatalf("store list: %v", err)
	}
	if len(stored) != 1 || stored[0].SecretCiphertext == "" {
		t.Fatalf("stored = %+v", stored)
	}
	plain, err := svc.Box.Open(stored[0].SecretCiphertext)
	if err != nil || plain != "sk-live-abcdefghijkl" {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
}

func TestCreateCredentialValidation(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.CreateCredential(ctx, "missing", "", CredentialAPIKey, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing provider = %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "", "bad", "x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad kind = %v", err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "   "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty secret = %v", err)
	}

	svc.Box = nil
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "x"); !errors.Is(err, ErrSecretBox) {
		t.Fatalf("nil box = %v", err)
	}
}

func TestDeleteProviderRefusesWhileInUse(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}
	credential, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-live-abcdefghijkl")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteProvider(ctx, "p1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with credential = %v, want ErrInUse", err)
	}

	if err := svc.DeleteCredential(ctx, credential.ID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if err := svc.DeleteProvider(ctx, "p1"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := svc.GetProvider(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProvider after delete = %v", err)
	}
}

func TestRouteNamesAreUnique(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}

	route, err := svc.CreateRoute(ctx, Route{
		ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini", Aliases: []string{" turbo ", "fast"},
	})
	if err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}
	if len(route.Aliases) != 1 || route.Aliases[0] != "turbo" {
		t.Fatalf("aliases = %v", route.Aliases)
	}

	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate exposed = %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "other", ProviderID: "p1", UpstreamModel: "x", Aliases: []string{"turbo"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate alias = %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "x", ProviderID: "missing", UpstreamModel: "y"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing provider = %v", err)
	}
	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "x", ProviderID: "p1"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing upstream = %v", err)
	}
}

func TestClientsBuildFromConfiguration(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Name: "openai", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCredential(ctx, "p1", "", CredentialAPIKey, "sk-live-abcdefghijkl"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "gpt-4o-mini", Aliases: []string{"f"}}); err != nil {
		t.Fatal(err)
	}

	clients, err := svc.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("clients = %v", keysOf(clients))
	}
	client, ok := clients["fast"]
	if !ok || clients["f"] != client {
		t.Fatalf("alias did not resolve to the same client: %v", keysOf(clients))
	}
	if client.Model != "gpt-4o-mini" || client.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("client = %+v", client)
	}
	if client.Credentials == nil {
		t.Fatal("client has no credential source")
	}
	credential, err := client.Credentials.Next(ctx, client.Name, client.Model)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if credential.APIKey != "sk-live-abcdefghijkl" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestClientsSkipDisabled(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := svc.UpdateProvider(ctx, "p1", ProviderPatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "m"}); err != nil {
		t.Fatal(err)
	}

	clients, err := svc.Clients(ctx)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("disabled provider still produced clients: %v", keysOf(clients))
	}
}

func TestClientsFailOnUndecryptableSecret(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, err := svc.CreateProvider(ctx, Provider{ID: "p1", Kind: KindOpenAI, BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRoute(ctx, Route{ExposedModel: "fast", ProviderID: "p1", UpstreamModel: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SaveCredential(ctx, Credential{
		ID: "cred_bad", ProviderID: "p1", Kind: CredentialAPIKey, Status: StatusOK,
		SecretCiphertext: "not-a-sealed-secret", HasSecret: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Clients(ctx); err == nil {
		t.Fatal("Clients accepted an undecryptable secret")
	}
}

func keysOf[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
