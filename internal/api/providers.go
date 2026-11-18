package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"janus/internal/provider"
)

type createProviderRequest struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Kind    provider.Kind `json:"kind"`
	BaseURL string        `json:"base_url"`
}

type updateProviderRequest struct {
	Name    *string `json:"name"`
	BaseURL *string `json:"base_url"`
	Enabled *bool   `json:"enabled"`
}

type createCredentialRequest struct {
	Label  string                  `json:"label"`
	Kind   provider.CredentialKind `json:"kind"`
	Secret string                  `json:"secret"`
}

type oauthCompleteRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
}

type createRouteRequest struct {
	ExposedModel  string   `json:"exposed_model"`
	ProviderID    string   `json:"provider_id"`
	UpstreamModel string   `json:"upstream_model"`
	Aliases       []string `json:"aliases"`
	Priority      int      `json:"priority"`
}

// reloadModels applies a persisted configuration change to the running proxy.
func (s *Server) reloadModels(ctx context.Context) {
	if s.ReloadModels == nil {
		return
	}
	if err := s.ReloadModels(ctx); err != nil {
		s.logf("reload model routes: %v", err)
	}
}

func (s *Server) providersUnavailable(w http.ResponseWriter) bool {
	if s.Providers == nil {
		writeError(w, http.StatusNotImplemented, "providers_unavailable",
			"provider management is not configured (set JANUS_MASTER_KEY)")
		return true
	}
	return false
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	list, err := s.Providers.ListProviders(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []provider.Provider{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req createProviderRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if strings.TrimSpace(string(req.Kind)) == "" {
		req.Kind = provider.KindOpenAI
	}
	created, err := s.Providers.CreateProvider(r.Context(), provider.Provider{
		ID:      req.ID,
		Name:    req.Name,
		Kind:    req.Kind,
		BaseURL: req.BaseURL,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req updateProviderRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	updated, err := s.Providers.UpdateProvider(r.Context(), r.PathValue("id"), provider.ProviderPatch{
		Name:    req.Name,
		BaseURL: req.BaseURL,
		Enabled: req.Enabled,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	if err := s.Providers.DeleteProvider(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	list, err := s.Providers.ListCredentials(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []provider.Credential{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": list})
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req createCredentialRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if strings.TrimSpace(string(req.Kind)) == "" {
		req.Kind = provider.CredentialAPIKey
	}
	created, err := s.Providers.CreateCredential(r.Context(),
		r.PathValue("id"), req.Label, req.Kind, req.Secret)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	// The response is the masked form; the secret is accepted once and never
	// returned, not even to the caller that supplied it.
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	if err := s.Providers.DeleteCredential(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	list, err := s.Providers.ListRoutes(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []provider.Route{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": list})
}

func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req createRouteRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	created, err := s.Providers.CreateRoute(r.Context(), provider.Route{
		ExposedModel:  req.ExposedModel,
		ProviderID:    req.ProviderID,
		UpstreamModel: req.UpstreamModel,
		Aliases:       req.Aliases,
		Priority:      req.Priority,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	if err := s.Providers.DeleteRoute(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// handleProbeProvider checks the endpoint and every stored credential with a
// GET /models. An unhealthy upstream is a 200 with ok=false: the probe itself
// succeeded, and the console needs the detail to show it.
func (s *Server) handleProbeProvider(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	// Several credentials can be tried in sequence; cap the whole call so a
	// dead endpoint cannot pin a request for a minute.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := s.Providers.Probe(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleStartProviderOAuth begins a PKCE sign-in and returns the vendor URL.
// The operator opens it, authorizes, and pastes the code back.
func (s *Server) handleStartProviderOAuth(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	start, err := s.Providers.StartOAuth(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, start)
}

// handleCompleteProviderOAuth exchanges the pasted code and stores the tokens
// as an oauth credential. The secret never travels back to the browser.
func (s *Server) handleCompleteProviderOAuth(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req oauthCompleteRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	credential, err := s.Providers.CompleteOAuth(r.Context(), r.PathValue("id"), req.SessionID, req.Code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusCreated, credential)
}
