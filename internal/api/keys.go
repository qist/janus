package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"janus/internal/auth"
	"janus/internal/store"
)

// KeyStore is the persistence subset the console needs. It is an interface so
// the HTTP layer can be tested without SQLite.
type KeyStore interface {
	ListAPIKeys(ctx context.Context) ([]store.APIKey, error)
	SaveAPIKey(ctx context.Context, key auth.KeyRecord) error
	GetAPIKey(ctx context.Context, id string) (store.APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error
}

type createKeyRequest struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Notes         string `json:"notes"`
	ExpiresInDays int    `json:"expires_in_days"`
	ExpiresAt     int64  `json:"expires_at"`
}

type createKeyResponse struct {
	Key       store.APIKey `json:"key"`
	Plaintext string       `json:"plaintext"`
}

func (s *Server) keysUnavailable(w http.ResponseWriter) bool {
	if s.Keys == nil {
		writeError(w, http.StatusNotImplemented, "keys_unavailable", "API key storage is not configured")
		return true
	}
	return false
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.keysUnavailable(w) {
		return
	}
	list, err := s.Keys.ListAPIKeys(r.Context())
	if err != nil {
		s.logf("list api keys: %v", err)
		writeError(w, http.StatusInternalServerError, "store_error", "cannot list API keys")
		return
	}
	if list == nil {
		list = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": list})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.keysUnavailable(w) {
		return
	}
	var req createKeyRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	if req.Kind == "" {
		req.Kind = auth.KeyKindAgent
	}

	now := time.Now().Unix()
	expiresAt := req.ExpiresAt
	if req.ExpiresInDays < 0 {
		writeError(w, http.StatusBadRequest, "invalid_expiry", "expires_in_days must be zero or positive")
		return
	}
	if expiresAt > 0 && expiresAt <= now {
		writeError(w, http.StatusBadRequest, "invalid_expiry", "expires_at must be in the future")
		return
	}
	if expiresAt == 0 && req.ExpiresInDays > 0 {
		expiresAt = time.Now().AddDate(0, 0, req.ExpiresInDays).Unix()
	}

	plaintext, record, err := auth.NewKeyRecord(req.Kind, req.Name, req.Notes, expiresAt, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_kind", err.Error())
		return
	}
	if err := s.Keys.SaveAPIKey(r.Context(), record); err != nil {
		s.logf("save api key: %v", err)
		writeError(w, http.StatusInternalServerError, "store_error", "cannot save API key")
		return
	}

	key, err := s.Keys.GetAPIKey(r.Context(), record.ID)
	if err != nil {
		s.logf("read back api key: %v", err)
		key = store.APIKey{ID: record.ID, Kind: record.Kind, Name: record.Name, Prefix: record.Prefix}
	}
	writeJSON(w, http.StatusCreated, createKeyResponse{Key: key, Plaintext: plaintext})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.keysUnavailable(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if err := s.Keys.RevokeAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, auth.ErrKeyNotFound) {
			writeError(w, http.StatusNotFound, "key_not_found", "API key not found")
			return
		}
		s.logf("revoke api key: %v", err)
		writeError(w, http.StatusInternalServerError, "store_error", "cannot revoke API key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRotateKey mints a replacement with the same kind and grants, then
// revokes the old row. The old plaintext stops working immediately.
func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.keysUnavailable(w) {
		return
	}
	old, err := s.Keys.GetAPIKey(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, auth.ErrKeyNotFound) {
			writeError(w, http.StatusNotFound, "key_not_found", "API key not found")
			return
		}
		s.logf("get api key for rotate: %v", err)
		writeError(w, http.StatusInternalServerError, "store_error", "cannot read API key")
		return
	}
	if old.Revoked {
		writeError(w, http.StatusConflict, "key_revoked", "cannot rotate a revoked key")
		return
	}

	plaintext, record, err := auth.NewKeyRecord(old.Kind, old.Name, old.Notes, old.ExpiresAt, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_kind", err.Error())
		return
	}
	record.Scopes = old.Scopes
	if err := s.Keys.SaveAPIKey(r.Context(), record); err != nil {
		s.logf("save rotated api key: %v", err)
		writeError(w, http.StatusInternalServerError, "store_error", "cannot save API key")
		return
	}
	if err := s.Keys.RevokeAPIKey(r.Context(), old.ID); err != nil {
		s.logf("revoke rotated api key: %v", err)
	}

	key, err := s.Keys.GetAPIKey(r.Context(), record.ID)
	if err != nil {
		key = store.APIKey{ID: record.ID, Kind: record.Kind, Name: record.Name, Prefix: record.Prefix}
	}
	writeJSON(w, http.StatusCreated, createKeyResponse{Key: key, Plaintext: plaintext})
}
