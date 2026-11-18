package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"janus/internal/provider"
)

type createProviderRequest struct {
	ID       string                `json:"id"`
	Name     string                `json:"name"`
	Kind     provider.Kind         `json:"kind"`
	BaseURL  string                `json:"base_url"`
	Channel  string                `json:"channel"`
	Protocol string                `json:"protocol"`
	OAuth    *provider.OAuthConfig `json:"oauth"`
}

type updateProviderRequest struct {
	Name       *string               `json:"name"`
	BaseURL    *string               `json:"base_url"`
	Enabled    *bool                 `json:"enabled"`
	Protocol   *string               `json:"protocol"`
	OAuth      *provider.OAuthConfig `json:"oauth"`
	ClearOAuth bool                  `json:"clear_oauth"`
}

type createCredentialRequest struct {
	Label  string                  `json:"label"`
	Kind   provider.CredentialKind `json:"kind"`
	Secret string                  `json:"secret"`
}

type updateCredentialRequest struct {
	Label    *string `json:"label"`
	Account  *string `json:"account"`
	Priority *int    `json:"priority"`
	Disabled *bool   `json:"disabled"`
}

type importRequest struct {
	Source string `json:"source"`
	Entry  string `json:"entry"`
	Label  string `json:"label"`
}

type oauthCompleteRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
	Label     string `json:"label"`
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
	for index := range list {
		list[index] = list[index].WithAuth()
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}

// handleListChannels returns the static upstream catalogue. It needs no
// provider store, so it stays available even when JANUS_MASTER_KEY is unset.
func (s *Server) handleListChannels(w http.ResponseWriter, _ *http.Request, _ Identity) {
	writeJSON(w, http.StatusOK, map[string]any{"channels": provider.Channels()})
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
		ID:       req.ID,
		Name:     req.Name,
		Kind:     req.Kind,
		BaseURL:  req.BaseURL,
		Channel:  req.Channel,
		Protocol: provider.Protocol(req.Protocol),
		OAuth:    req.OAuth,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusCreated, created.WithAuth())
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
		Name:       req.Name,
		BaseURL:    req.BaseURL,
		Enabled:    req.Enabled,
		Protocol:   req.Protocol,
		OAuth:      req.OAuth,
		ClearOAuth: req.ClearOAuth,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, updated.WithAuth())
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

type syncRoutesRequest struct {
	Prune bool     `json:"prune"`
	Skip  []string `json:"skip"`
}

// handleSyncRoutes compares the provider's /models with the stored routes and
// creates the missing ones. It never touches another provider's exposed names.
func (s *Server) handleSyncRoutes(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req syncRoutesRequest
	if r.ContentLength != 0 {
		if !decode(w, r, s.maxBody(), &req) {
			return
		}
	}
	// Each model insert reads the route table; keep the whole sweep under one
	// budget so a large catalogue cannot hold the console forever.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	result, err := s.Providers.SyncRoutes(ctx, r.PathValue("id"), provider.SyncOptions{Prune: req.Prune, Skip: req.Skip})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, result)
}

// handleRefreshQuota reads the allowance/balance of every credential of one
// provider. The vendor call happens server-side; the secret never leaves.
func (s *Server) handleRefreshQuota(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	// Several credentials can be read in sequence; cap the whole call so a dead
	// vendor cannot pin a console request.
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	results, err := s.Providers.RefreshQuota(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if results == nil {
		results = []provider.QuotaResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"quotas": results})
}

// handleListQuotas returns the stored snapshots without contacting vendors.
func (s *Server) handleListQuotas(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	results, err := s.Providers.ListQuotas(r.Context(), r.URL.Query().Get("provider"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if results == nil {
		results = []provider.QuotaResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"quotas": results})
}

// handleStartDevice begins a device-code sign-in (GitHub Copilot). The console
// shows the user code and polls until the operator finishes.
func (s *Server) handleStartDevice(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req deviceRequest
	if r.ContentLength != 0 {
		if !decode(w, r, s.maxBody(), &req) {
			return
		}
	}
	start, err := s.Providers.StartDevice(r.Context(), r.PathValue("id"), req.Label)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, start)
}

// handlePollDevice checks once. A pending answer is a 200 carrying status, not
// an error: the console keeps polling at the returned interval.
func (s *Server) handlePollDevice(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req deviceRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	result, err := s.Providers.PollDevice(r.Context(), r.PathValue("id"), req.SessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if result.Status == "complete" {
		s.reloadModels(r.Context())
	}
	writeJSON(w, http.StatusOK, result)
}

type deviceRequest struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label"`
}

// handleStartProviderOAuth begins a PKCE sign-in and returns the vendor URL.
// The operator opens it, authorizes, and pastes the code back.
func (s *Server) handleStartProviderOAuth(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	start, err := s.Providers.StartOAuth(r.Context(), r.PathValue("id"),
		strings.TrimSpace(r.URL.Query().Get("credential_id")))
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
	credential, err := s.Providers.CompleteOAuth(r.Context(), r.PathValue("id"), req.SessionID, req.Code, req.Label)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, credential)
}

// handleUpdateCredential changes the operator-visible metadata of one account:
// its note, its place in the rotation, or whether it is used at all.
func (s *Server) handleUpdateCredential(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req updateCredentialRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	updated, err := s.Providers.UpdateCredential(r.Context(), r.PathValue("id"), provider.CredentialPatch{
		Label:    req.Label,
		Account:  req.Account,
		Priority: req.Priority,
		Disabled: req.Disabled,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, updated)
}

// handleResetCredential clears cooldown/exhausted state after a top-up or a
// re-authorization done elsewhere.
func (s *Server) handleResetCredential(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	updated, err := s.Providers.ResetCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, updated)
}

// handleImportLocal adopts a CLI sign-in that already exists on this host.
func (s *Server) handleImportLocal(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.providersUnavailable(w) {
		return
	}
	var req importRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	result, err := s.Providers.ImportLocal(r.Context(),
		r.PathValue("id"), provider.ImportSource(strings.ToLower(strings.TrimSpace(req.Source))),
		req.Entry, req.Label)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.reloadModels(r.Context())
	writeJSON(w, http.StatusOK, result)
}
