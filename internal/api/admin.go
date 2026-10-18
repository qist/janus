package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/gateway"
	"janus/internal/httpx"
)

// admin.go —— 控制面 /api/v1/*（admin key）。
//
// 所有资源都是数据库里的数据：providers / credentials / models / routes /
// keys / agents / settings。这里只做「校验 + 转发到 store」，不含任何隐藏的
// 全局单例状态。

// registerAdminRoutes 注册控制面路由。
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	// providers
	mux.HandleFunc("GET /api/v1/providers", s.adminGuard(s.handleListProviders))
	mux.HandleFunc("POST /api/v1/providers", s.adminGuard(s.handleCreateProvider))
	mux.HandleFunc("GET /api/v1/providers/{id}", s.adminGuard(s.handleGetProvider))
	mux.HandleFunc("PATCH /api/v1/providers/{id}", s.adminGuard(s.handlePatchProvider))
	mux.HandleFunc("DELETE /api/v1/providers/{id}", s.adminGuard(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/v1/providers/{id}/discover", s.adminGuard(s.handleDiscoverProvider))
	mux.HandleFunc("POST /api/v1/providers/{id}/test", s.adminGuard(s.handleTestProvider))

	// credentials
	mux.HandleFunc("GET /api/v1/credentials", s.adminGuard(s.handleListCredentials))
	mux.HandleFunc("POST /api/v1/credentials", s.adminGuard(s.handleCreateCredential))
	mux.HandleFunc("GET /api/v1/credentials/{id}", s.adminGuard(s.handleGetCredential))
	mux.HandleFunc("PATCH /api/v1/credentials/{id}", s.adminGuard(s.handlePatchCredential))
	mux.HandleFunc("DELETE /api/v1/credentials/{id}", s.adminGuard(s.handleDeleteCredential))
	mux.HandleFunc("POST /api/v1/credentials/{id}/verify", s.adminGuard(s.handleVerifyCredential))

	// models
	mux.HandleFunc("GET /api/v1/models", s.adminGuard(s.handleListCatalog))
	mux.HandleFunc("POST /api/v1/models", s.adminGuard(s.handleCreateCatalogModel))
	mux.HandleFunc("PATCH /api/v1/models/{id...}", s.adminGuard(s.handlePatchCatalogModel))
	mux.HandleFunc("DELETE /api/v1/models/{id...}", s.adminGuard(s.handleDeleteCatalogModel))

	// routes
	mux.HandleFunc("GET /api/v1/routes", s.adminGuard(s.handleListRoutes))
	mux.HandleFunc("POST /api/v1/routes", s.adminGuard(s.handleCreateRoute))
	mux.HandleFunc("PATCH /api/v1/routes/{id}", s.adminGuard(s.handlePatchRoute))
	mux.HandleFunc("DELETE /api/v1/routes/{id}", s.adminGuard(s.handleDeleteRoute))
	mux.HandleFunc("POST /api/v1/routes/resolve", s.adminGuard(s.handleResolveRoute))

	// keys
	mux.HandleFunc("GET /api/v1/keys", s.adminGuard(s.handleListKeys))
	mux.HandleFunc("POST /api/v1/keys", s.adminGuard(s.handleCreateKey))
	mux.HandleFunc("DELETE /api/v1/keys/{id}", s.adminGuard(s.handleRevokeKey))
	mux.HandleFunc("POST /api/v1/keys/{id}/rotate", s.adminGuard(s.handleRotateKey))

	// agents
	mux.HandleFunc("GET /api/v1/agents", s.adminGuard(s.handleListAgents))
	mux.HandleFunc("POST /api/v1/agents", s.adminGuard(s.handleCreateAgent))
	mux.HandleFunc("PATCH /api/v1/agents/{id}", s.adminGuard(s.handlePatchAgent))
	mux.HandleFunc("DELETE /api/v1/agents/{id}", s.adminGuard(s.handleDeleteAgent))
	mux.HandleFunc("POST /api/v1/agents/{id}/provision", s.adminGuard(s.handleProvisionAgent))

	// settings
	mux.HandleFunc("GET /api/v1/settings", s.adminGuard(s.handleGetRuntimeSettings))
	mux.HandleFunc("PATCH /api/v1/settings", s.adminGuard(s.handlePatchRuntimeSettings))
	mux.HandleFunc("GET /api/v1/settings/schema", s.adminGuard(s.handleSettingsSchema))

	// sessions
	mux.HandleFunc("GET /api/v1/sessions", s.adminGuard(s.handleListSessions))
	mux.HandleFunc("DELETE /api/v1/sessions/{key...}", s.adminGuard(s.handleDeleteSession))

	// 用量 / 请求日志（原 /v1/*，移到控制面）
	mux.HandleFunc("GET /api/v1/usage", s.adminGuard(s.handleUsage))
	mux.HandleFunc("GET /api/v1/requests", s.adminGuard(s.handleRequests))
}

// ---------- 通用 ----------

func (s *Server) storeOrError(w http.ResponseWriter) (*db.Store, bool) {
	if s.db == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "persistence_disabled",
			"persistence is disabled (JANUS_DB is empty or memory)", nil)
		return nil, false
	}
	return s.db, true
}

func readJSONBody(w http.ResponseWriter, r *http.Request, max int64, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, max))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "empty body", nil)
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error(), nil)
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", nil)
	case errors.Is(err, db.ErrConflict):
		writeAPIError(w, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, db.ErrDisabled):
		writeAPIError(w, http.StatusServiceUnavailable, "persistence_disabled", err.Error(), nil)
	default:
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
	}
}

// ---------- providers ----------

// providerToView 是控制面返回的 provider（Headers 解析成对象）。
// 显式映射字段，避免 db.Provider 的 Go 字段名直接漏到 JSON。
func providerToView(p db.Provider) map[string]any {
	var headers map[string]string
	if len(p.Headers) > 0 {
		_ = json.Unmarshal(p.Headers, &headers)
	}
	return map[string]any{
		"id": p.ID, "name": p.Name, "type": p.Type, "base_url": p.BaseURL,
		"auth_kind": p.AuthKind, "headers": headers, "enabled": p.Enabled,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
	}
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	rows := st.ListProviders(r.URL.Query().Get("enabled") == "1")
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, providerToView(p))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"providers": out})
}

type providerInput struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	BaseURL  string            `json:"base_url"`
	AuthKind string            `json:"auth_kind"`
	Headers  map[string]string `json:"headers"`
	Enabled  *bool             `json:"enabled"`
}

func (in providerInput) toRow(base db.Provider) (db.Provider, error) {
	row := base
	if in.ID != "" {
		row.ID = strings.TrimSpace(in.ID)
	}
	if in.Name != "" {
		row.Name = strings.TrimSpace(in.Name)
	}
	if in.Type != "" {
		switch in.Type {
		case gateway.ProtocolOpenAIChat, gateway.ProtocolOpenAIResponses, gateway.ProtocolAnthropic:
			row.Type = in.Type
		default:
			return row, fmt.Errorf("type must be %s|%s|%s",
				gateway.ProtocolOpenAIChat, gateway.ProtocolOpenAIResponses, gateway.ProtocolAnthropic)
		}
	}
	if in.BaseURL != "" {
		row.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	}
	if in.AuthKind != "" {
		switch in.AuthKind {
		case "api_key", "oauth", "none":
			row.AuthKind = in.AuthKind
		default:
			return row, errors.New("auth_kind must be api_key|oauth|none")
		}
	}
	if in.Headers != nil {
		raw, err := json.Marshal(in.Headers)
		if err != nil {
			return row, err
		}
		row.Headers = raw
	}
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	if row.ID == "" {
		return row, errors.New("id is required")
	}
	if row.Name == "" {
		row.Name = row.ID
	}
	if row.Type == "" {
		row.Type = gateway.ProtocolOpenAIChat
	}
	if row.AuthKind == "" {
		row.AuthKind = "api_key"
	}
	return row, nil
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in providerInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	row, err := in.toRow(db.Provider{Enabled: true})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := st.SaveProvider(row)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, providerToView(saved))
}

func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	row, found := st.GetProvider(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, providerToView(row))
}

func (s *Server) handlePatchProvider(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	base, found := st.GetProvider(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	var in providerInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	in.ID = base.ID
	row, err := in.toRow(base)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := st.SaveProvider(row)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, providerToView(saved))
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	if err := st.DeleteProvider(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- credentials ----------

// credentialView 不返回明文，只返回 secret 的键名（便于 UI 显示"已配置 api_key"）。
type credentialView struct {
	ID         string   `json:"id"`
	ProviderID string   `json:"provider_id"`
	Label      string   `json:"label"`
	Priority   int      `json:"priority"`
	Enabled    bool     `json:"enabled"`
	SecretKeys []string `json:"secret_keys"`
	LastUsedAt int64    `json:"last_used_at"`
	LastError  string   `json:"last_error"`
	FailUntil  int64    `json:"fail_until"`
	CreatedAt  int64    `json:"created_at"`
	UpdatedAt  int64    `json:"updated_at"`
}

func credentialToView(c db.Credential) credentialView {
	v := credentialView{
		ID: c.ID, ProviderID: c.ProviderID, Label: c.Label, Priority: c.Priority,
		Enabled: c.Enabled, LastUsedAt: c.LastUsedAt, LastError: c.LastError,
		FailUntil: c.FailUntil, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if plain, err := db.DecryptBlob(c.Secret); err == nil {
		var m map[string]any
		if json.Unmarshal(plain, &m) == nil {
			for k := range m {
				v.SecretKeys = append(v.SecretKeys, k)
			}
			sort.Strings(v.SecretKeys)
		}
	}
	return v
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	rows := st.ListCredentials(r.URL.Query().Get("provider_id"))
	out := make([]credentialView, 0, len(rows))
	for _, c := range rows {
		out = append(out, credentialToView(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

func (s *Server) handleGetCredential(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	row, found := st.GetCredential(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "credential not found", nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, credentialToView(row))
}

type credentialInput struct {
	ProviderID string         `json:"provider_id"`
	Label      string         `json:"label"`
	Secret     map[string]any `json:"secret"`
	Priority   *int           `json:"priority"`
	Enabled    *bool          `json:"enabled"`
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in credentialInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	if in.ProviderID == "" {
		in.ProviderID = r.URL.Query().Get("provider_id")
	}
	priority := 0
	if in.Priority != nil {
		priority = *in.Priority
	}
	row, err := st.CreateCredential(in.ProviderID, db.CredentialInput{
		Label: in.Label, Secret: in.Secret, Priority: priority, Enabled: in.Enabled,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, credentialToView(row))
}

func (s *Server) handlePatchCredential(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in struct {
		Label    *string        `json:"label"`
		Secret   map[string]any `json:"secret"`
		Priority *int           `json:"priority"`
		Enabled  *bool          `json:"enabled"`
	}
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	row, err := st.UpdateCredential(r.PathValue("id"), in.Label, in.Secret, in.Priority, in.Enabled)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, credentialToView(row))
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	if err := st.DeleteCredential(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVerifyCredential(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	row, found := st.GetCredential(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "credential not found", nil)
		return
	}
	p, found := st.GetProvider(row.ProviderID)
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	err := s.probeProvider(r.Context(), p, row)
	if err != nil {
		st.MarkCredentialError(row.ID, err.Error(), 0)
		writeAPIError(w, http.StatusBadGateway, "credential_invalid", err.Error(), nil)
		return
	}
	st.MarkCredentialUsed(row.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "provider_id": p.ID, "credential_id": row.ID})
}

// ---------- models（目录） ----------

func (s *Server) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	rows := st.ListModels(r.URL.Query().Get("provider_id"), false)
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		if q != "" && !strings.Contains(strings.ToLower(m.ID), q) {
			continue
		}
		out = append(out, catalogModelView(m))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": out})
}

func catalogModelView(m db.Model) map[string]any {
	var caps any
	if len(m.Capabilities) > 0 {
		_ = json.Unmarshal(m.Capabilities, &caps)
	}
	var pricing any
	if len(m.Pricing) > 0 {
		_ = json.Unmarshal(m.Pricing, &pricing)
	}
	return map[string]any{
		"id": m.ID, "provider_id": m.ProviderID, "upstream_id": m.UpstreamID,
		"display_name": m.DisplayName, "capabilities": caps, "pricing": pricing,
		"source": m.Source, "enabled": m.Enabled,
		"created_at": m.CreatedAt, "updated_at": m.UpdatedAt,
	}
}

type catalogModelInput struct {
	ProviderID   string `json:"provider_id"`
	UpstreamID   string `json:"upstream_id"`
	DisplayName  string `json:"display_name"`
	Capabilities any    `json:"capabilities"`
	Pricing      any    `json:"pricing"`
	Enabled      *bool  `json:"enabled"`
}

func (in catalogModelInput) toRow(base db.Model) (db.Model, error) {
	row := base
	if in.ProviderID != "" {
		row.ProviderID = strings.TrimSpace(in.ProviderID)
	}
	if in.UpstreamID != "" {
		row.UpstreamID = strings.TrimSpace(in.UpstreamID)
	}
	if in.DisplayName != "" {
		row.DisplayName = in.DisplayName
	}
	if in.Capabilities != nil {
		raw, err := json.Marshal(in.Capabilities)
		if err != nil {
			return row, err
		}
		row.Capabilities = raw
	}
	if in.Pricing != nil {
		raw, err := json.Marshal(in.Pricing)
		if err != nil {
			return row, err
		}
		row.Pricing = raw
	}
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	if row.ProviderID == "" || row.UpstreamID == "" {
		return row, errors.New("provider_id and upstream_id are required")
	}
	if row.ID == "" {
		row.ID = row.ProviderID + "/" + row.UpstreamID
	}
	if row.Source == "" {
		row.Source = db.ModelSourceManual
	}
	return row, nil
}

func (s *Server) handleCreateCatalogModel(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in catalogModelInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	row, err := in.toRow(db.Model{Enabled: true})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := st.SaveModel(row)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, catalogModelView(saved))
}

func (s *Server) handlePatchCatalogModel(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	base, found := st.GetModel(id)
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "model not found", nil)
		return
	}
	var in catalogModelInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	// provider/upstream 是主键的一部分：不允许通过 PATCH 改（会变成新条目）。
	in.ProviderID, in.UpstreamID = "", ""
	row, err := in.toRow(base)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := st.SaveModel(row)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, catalogModelView(saved))
}

func (s *Server) handleDeleteCatalogModel(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	if err := st.DeleteModel(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- routes ----------

func routeView(r db.ModelRoute) map[string]any {
	return map[string]any{
		"id": r.ID, "scope": r.Scope, "scope_id": r.ScopeID, "alias": r.Alias,
		"target": r.Target, "priority": r.Priority, "enabled": r.Enabled,
		"created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
	}
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	rows := st.ListRoutes(r.URL.Query().Get("scope"))
	out := make([]map[string]any, 0, len(rows))
	for _, rt := range rows {
		out = append(out, routeView(rt))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"routes": out})
}

type routeInput struct {
	Scope    string `json:"scope"`
	ScopeID  string `json:"scope_id"`
	Alias    string `json:"alias"`
	Target   string `json:"target"`
	Priority *int   `json:"priority"`
	Enabled  *bool  `json:"enabled"`
}

func (in routeInput) toRow(base db.ModelRoute) db.ModelRoute {
	row := base
	if in.Scope != "" {
		row.Scope = in.Scope
	}
	if in.ScopeID != "" {
		row.ScopeID = in.ScopeID
	}
	if in.Alias != "" {
		row.Alias = in.Alias
	}
	if in.Target != "" {
		row.Target = in.Target
	}
	if in.Priority != nil {
		row.Priority = *in.Priority
	}
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	return row
}

func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in routeInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	row, err := st.SaveRoute(in.toRow(db.ModelRoute{Enabled: true}))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, routeView(row))
}

func (s *Server) handlePatchRoute(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	base, found := st.GetRoute(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "route not found", nil)
		return
	}
	var in routeInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	row, err := st.SaveRoute(in.toRow(base))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, routeView(row))
}

func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	if err := st.DeleteRoute(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResolveRoute(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model      string `json:"model"`
		Credential string `json:"credential"`
		Protocol   string `json:"protocol"`
		ProviderID string `json:"provider_id"`
	}
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	if s.router == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "persistence_disabled", "router unavailable", nil)
		return
	}
	res, err := s.router.Route(gateway.RouteInput{
		Model: in.Model, Protocol: in.Protocol, CredentialHint: in.Credential,
	})
	if err != nil {
		status := http.StatusNotFound
		code := "model_not_found"
		var amb *gateway.AmbiguousError
		switch {
		case errors.As(err, &amb):
			status, code = http.StatusConflict, "ambiguous_model"
			writeAPIError(w, status, code, err.Error(), map[string]any{"candidates": amb.Candidates})
			return
		case errors.Is(err, gateway.ErrNoCredential):
			status, code = http.StatusServiceUnavailable, "no_available_credential"
		case errors.Is(err, gateway.ErrInvalidRoute):
			status, code = http.StatusBadRequest, "invalid_route"
		}
		writeAPIError(w, status, code, err.Error(), nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// ---------- keys ----------

func keyView(k db.APIKey) map[string]any {
	return map[string]any{
		"id": k.ID, "kind": k.Kind, "name": k.Name, "prefix": k.Prefix,
		"scopes": db.DecodeStrings(k.Scopes), "provider_ids": db.DecodeStrings(k.ProviderIDs),
		"credential_ids": db.DecodeStrings(k.CredentialIDs),
		"expires_at":     k.ExpiresAt, "revoked": k.Revoked,
		"last_used_at": k.LastUsedAt, "created_at": k.CreatedAt,
	}
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	rows := st.ListKeys(r.URL.Query().Get("kind"))
	out := make([]map[string]any, 0, len(rows))
	for _, k := range rows {
		out = append(out, keyView(k))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": out})
}

type keyInput struct {
	Kind          string   `json:"kind"`
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ProviderIDs   []string `json:"provider_ids"`
	CredentialIDs []string `json:"credential_ids"`
	ExpiresAt     int64    `json:"expires_at"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in keyInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	if in.Kind == "" {
		in.Kind = db.KeyKindClient
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		switch in.Kind {
		case db.KeyKindAdmin:
			scopes = []string{ScopeAdmin}
		case db.KeyKindTool:
			scopes = []string{ScopeSession}
		default:
			scopes = []string{ScopeModel}
		}
	}
	row, plain, err := st.CreateKey(in.Kind, in.Name, scopes, in.ProviderIDs, in.CredentialIDs, in.ExpiresAt)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := keyView(row)
	out["key"] = plain // 只在这里返回一次
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	if err := st.RevokeKey(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	row, plain, err := st.RotateKey(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := keyView(row)
	out["key"] = plain
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// ---------- agents ----------

func agentView(a db.Agent) map[string]any {
	var cfg any
	if len(a.Config) > 0 {
		_ = json.Unmarshal(a.Config, &cfg)
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "kind": a.Kind, "base_url": a.BaseURL,
		"auth_kind": a.AuthKind, "api_key_id": a.APIKeyID, "enabled": a.Enabled,
		"config": cfg, "status": a.Status, "last_seen": a.LastSeen,
		"created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
		"has_secret": len(a.Secret) > 0,
	}
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	rows := st.ListAgents()
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, agentView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"agents": out})
}

type agentInput struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	BaseURL  string         `json:"base_url"`
	AuthKind string         `json:"auth_kind"`
	Secret   map[string]any `json:"secret"`
	APIKeyID string         `json:"api_key_id"`
	Config   any            `json:"config"`
	Enabled  *bool          `json:"enabled"`
	Status   string         `json:"status"`
}

func (in agentInput) toRow(base db.Agent) (db.Agent, error) {
	row := base
	if in.ID != "" {
		row.ID = strings.TrimSpace(in.ID)
	}
	if in.Name != "" {
		row.Name = in.Name
	}
	if in.Kind != "" {
		row.Kind = in.Kind
	}
	if in.BaseURL != "" {
		row.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	}
	if in.AuthKind != "" {
		row.AuthKind = in.AuthKind
	}
	if in.Secret != nil {
		raw, err := json.Marshal(in.Secret)
		if err != nil {
			return row, err
		}
		blob, err := db.EncryptBlob(raw)
		if err != nil {
			return row, err
		}
		row.Secret = blob
	}
	if in.APIKeyID != "" {
		row.APIKeyID = in.APIKeyID
	}
	if in.Config != nil {
		raw, err := json.Marshal(in.Config)
		if err != nil {
			return row, err
		}
		row.Config = raw
	}
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	if in.Status != "" {
		row.Status = in.Status
	}
	if row.ID == "" {
		return row, errors.New("id is required")
	}
	if row.Name == "" {
		row.Name = row.ID
	}
	if row.Kind == "" {
		row.Kind = "opencode"
	}
	if row.AuthKind == "" {
		row.AuthKind = "basic"
	}
	return row, nil
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	var in agentInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	row, err := in.toRow(db.Agent{Enabled: true})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := st.SaveAgent(row)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, agentView(saved))
}

func (s *Server) handlePatchAgent(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	base, found := st.GetAgent(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "agent not found", nil)
		return
	}
	var in agentInput
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	in.ID = base.ID
	row, err := in.toRow(base)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	saved, err := st.SaveAgent(row)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, agentView(saved))
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	if err := st.DeleteAgent(r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProvisionAgent 返回把该 harness 接到 janus 所需的配置片段。
func (s *Server) handleProvisionAgent(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	a, found := st.GetAgent(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "agent not found", nil)
		return
	}
	base := s.publicBaseURL(r)
	snippet := map[string]any{
		"agent": a.ID,
		"env": map[string]string{
			"OPENAI_BASE_URL":      base + "/v1",
			"OPENAI_API_KEY":       "<agent key: POST /api/v1/keys {\"kind\":\"agent\"}>",
			"ANTHROPIC_BASE_URL":   base + "/anthropic",
			"ANTHROPIC_AUTH_TOKEN": "<agent key>",
		},
	}
	if a.APIKeyID != "" {
		snippet["api_key_id"] = a.APIKeyID
	}
	if a.Kind == "opencode" {
		snippet["config"] = map[string]any{
			"provider": map[string]any{
				"janus": map[string]any{
					"npm":     "@ai-sdk/openai-compatible",
					"name":    "Janus",
					"options": map[string]any{"baseURL": base + "/v1", "apiKey": "<agent key>"},
					"models":  map[string]any{},
				},
			},
		}
	}
	httpx.WriteJSON(w, http.StatusOK, snippet)
}

func (s *Server) publicBaseURL(r *http.Request) string {
	if v := strings.TrimSpace(s.cfg.MCPPublicURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = s.cfg.Addr
	}
	return scheme + "://" + host
}

// ---------- settings ----------

// handleGetRuntimeSettings 返回运行期配置（settings.runtime）+ 零散设置。
func (s *Server) handleGetRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"runtime":       s.cfg,
		"default_model": s.runtimeDefaultModel(),
		"ui_prefs":      s.uiPrefs(),
	}
	if s.db != nil {
		out["settings"] = s.db.AllSettings()
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// handlePatchRuntimeSettings 批量更新 {key: value}。
//
// 运行期配置整份存在 settings.runtime：先把补丁合并进旧 JSON，再交给
// config.ApplySettings 解码（缺失项自动回落默认值），最后整份写回。
func (s *Server) handlePatchRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]json.RawMessage
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &patch) {
		return
	}
	if len(patch) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "empty patch", nil)
		return
	}

	// 零散设置：default_model / ui_prefs 不参与 runtime 合并。
	if raw, ok := patch["default_model"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "default_model must be a string", nil)
			return
		}
		s.setRuntimeDefaultModel(strings.TrimSpace(v))
		delete(patch, "default_model")
	}
	if raw, ok := patch["ui_prefs"]; ok {
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "ui_prefs must be an object", nil)
			return
		}
		if s.db != nil {
			if err := s.db.SetSettingJSON("ui_prefs", v); err != nil {
				writeStoreError(w, err)
				return
			}
		}
		delete(patch, "ui_prefs")
	}

	if len(patch) > 0 {
		st, ok := s.storeOrError(w)
		if !ok {
			return
		}
		merged := map[string]json.RawMessage{}
		if raw, found := st.GetSettingRaw(db.SettingRuntime); found && len(raw) > 0 {
			if err := json.Unmarshal(raw, &merged); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "invalid_settings",
					"stored runtime settings are corrupt: "+err.Error(), nil)
				return
			}
		}
		for k, v := range patch {
			merged[k] = v
		}
		raw, err := json.Marshal(merged)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
			return
		}
		s.settingsMu.Lock()
		err = s.cfg.ApplySettings(raw, json.Unmarshal)
		if err == nil {
			err = st.SetSettingRaw(db.SettingRuntime, raw)
		}
		s.settingsMu.Unlock()
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_settings", err.Error(), nil)
			return
		}
		// 上游端点类设置立即生效（不用等重启）。
		if _, a := patch["upstream"]; a {
			s.up.SetEndpoint(s.cfg.Upstream, s.cfg.Username, s.cfg.Password)
		}
		if _, a := patch["username"]; a {
			s.up.SetEndpoint(s.cfg.Upstream, s.cfg.Username, s.cfg.Password)
		}
		if _, a := patch["password"]; a {
			s.up.SetEndpoint(s.cfg.Upstream, s.cfg.Username, s.cfg.Password)
		}
		s.log.Infof("runtime settings updated: %s", strings.Join(sortedKeys(patch), ", "))
	}

	s.handleGetRuntimeSettings(w, r)
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// settingSchema 是 /settings/schema 的一项。
type settingSchema struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"`
	Group   string   `json:"group"`
	Enum    []string `json:"enum,omitempty"`
	Default any      `json:"default,omitempty"`
}

// handleSettingsSchema 用反射生成配置项 schema，前端据此自动渲染表单。
func (s *Server) handleSettingsSchema(w http.ResponseWriter, r *http.Request) {
	def := config.Load()
	def.ApplyDefaults()
	rows := configSchema(def)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settings": rows})
}

func configSchema(def config.Config) []settingSchema {
	var out []settingSchema
	t := reflect.TypeOf(def)
	v := reflect.ValueOf(def)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		key := strings.Split(tag, ",")[0]
		if key == "" || key == "-" {
			continue
		}
		item := settingSchema{Key: key, Type: schemaType(f.Type), Group: schemaGroup(key)}
		if e := schemaEnum(key); len(e) > 0 {
			item.Enum = e
		}
		item.Default = v.Field(i).Interface()
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func schemaType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int32, reflect.Int64:
		if t == reflect.TypeOf(time.Duration(0)) {
			return "duration"
		}
		return "int"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice:
		return "list"
	case reflect.Map:
		return "map"
	default:
		return "string"
	}
}

func schemaGroup(key string) string {
	switch {
	case strings.HasPrefix(key, "upstream"), strings.HasPrefix(key, "opencode"), strings.HasPrefix(key, "autostart"), key == "reuse_external", key == "upstream_cleanup":
		return "upstream"
	case strings.HasPrefix(key, "tool"), strings.HasPrefix(key, "mcp"), key == "permission_reply":
		return "tools"
	case strings.HasPrefix(key, "rate_limit"), key == "trust_proxy", key == "cors_origin", key == "metrics_public":
		return "security"
	case strings.HasPrefix(key, "stream"), strings.HasPrefix(key, "session"), strings.HasPrefix(key, "request"), strings.HasPrefix(key, "idle"), strings.HasPrefix(key, "prompt"), strings.HasPrefix(key, "reconcile"), strings.HasPrefix(key, "history"), strings.HasPrefix(key, "conv"), strings.HasPrefix(key, "max_"):
		return "runtime"
	case strings.HasPrefix(key, "usage"), key == "console_url", key == "go_usage_url", key == "opencode_db":
		return "usage"
	case strings.HasPrefix(key, "response"), strings.HasPrefix(key, "anthropic"), strings.HasPrefix(key, "websearch"), key == "model_echo":
		return "protocol"
	default:
		return "general"
	}
}

func schemaEnum(key string) []string {
	switch key {
	case "permission_reply":
		return []string{"once", "always", "reject", "off"}
	case "model_echo":
		return []string{"real", "request"}
	case "mcp_transport":
		return []string{"local", "remote"}
	case "workbuddy_context":
		return []string{"max", "default"}
	}
	return nil
}

// ---------- sessions ----------

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	limit := atoiOrZero(r.URL.Query().Get("limit"))
	rows := s.db.ListConvs(limit)
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"key": c.Key, "session_id": c.SessionID, "provider_id": c.ProviderID,
			"model_id": c.ModelID, "variant": c.Variant, "agent": c.Agent,
			"agent_id": c.AgentID, "credential": c.Credential, "directory": c.Directory,
			"updated_at": c.UpdatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "persistence_disabled", "persistence is disabled", nil)
		return
	}
	key := r.PathValue("key")
	row, found := s.db.LoadConv(key)
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "session not found", nil)
		return
	}
	if row.SessionID != "" {
		s.deleteSession(row.SessionID)
	}
	s.db.DeleteConv(key)
	w.WriteHeader(http.StatusNoContent)
}
