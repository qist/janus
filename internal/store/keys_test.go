package store

import (
	"context"
	"errors"
	"testing"

	"janus/internal/auth"
)

func TestAPIKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, record, err := auth.NewKeyRecord(auth.KeyKindClient, "app", "test note", 0, func() int64 { return 1_700_000_000 })
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}
	if err := s.SaveAPIKey(ctx, record); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}

	list, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(list) != 1 || list[0].ID != record.ID || list[0].Kind != auth.KeyKindClient {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Scopes == nil || len(list[0].Scopes) != 2 {
		t.Fatalf("scopes = %+v", list[0].Scopes)
	}

	got, err := s.GetAPIKey(ctx, record.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.Name != "app" || got.Notes != "test note" || got.Prefix != record.Prefix {
		t.Fatalf("got = %+v", got)
	}

	found, err := s.LookupAPIKey(ctx, record.Hash)
	if err != nil {
		t.Fatalf("LookupAPIKey: %v", err)
	}
	if found.ID != record.ID || found.Kind != record.Kind || len(found.Scopes) != 2 {
		t.Fatalf("found = %+v", found)
	}
	if _, err := s.LookupAPIKey(ctx, auth.HashKey("sk-janus-c-unknown")); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("unknown digest: err = %v", err)
	}
}

func TestRevokeAPIKey(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, record, err := auth.NewKeyRecord(auth.KeyKindAgent, "worker", "", 0, nil)
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}
	if err := s.SaveAPIKey(ctx, record); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, record.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	found, err := s.LookupAPIKey(ctx, record.Hash)
	if err != nil {
		t.Fatalf("LookupAPIKey: %v", err)
	}
	if !found.Revoked {
		t.Fatal("record should be revoked")
	}
	if err := s.RevokeAPIKey(ctx, "key_missing"); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("missing revoke: err = %v", err)
	}
}

func TestTouchAPIKeyStampsLastUsed(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_, record, err := auth.NewKeyRecord(auth.KeyKindTool, "mcp", "", 0, nil)
	if err != nil {
		t.Fatalf("NewKeyRecord: %v", err)
	}
	if err := s.SaveAPIKey(ctx, record); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	s.TouchAPIKey(ctx, record.ID)

	got, err := s.GetAPIKey(ctx, record.ID)
	if err != nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	if got.LastUsedAt == 0 {
		t.Fatal("last_used_at was not stamped")
	}
}
