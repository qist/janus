package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"janus/internal/channel/opencode"
	"janus/internal/db"
	"janus/internal/httpx"
	"janus/internal/upstream"
)

// accountView 是返回给前端/客户端的账号视图（不含凭据）。
type accountView struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	ProviderID string   `json:"provider_id"`
	Label      string   `json:"label"`
	ExternalID string   `json:"external_id"`
	Active     bool     `json:"active"`
	Models     []string `json:"models"`
	CreatedAt  int64    `json:"created_at"`
	UpdatedAt  int64    `json:"updated_at"`
}

func (s *Server) accountToView(a db.Account) accountView {
	v := accountView{
		ID:         a.ID,
		Kind:       a.Kind,
		ProviderID: a.ProviderID,
		Label:      a.Label,
		ExternalID: a.ExternalID,
		Active:     a.Active,
		CreatedAt:  a.CreatedAt,
		UpdatedAt:  a.UpdatedAt,
	}
	if s.db != nil {
		v.Models = s.db.AccountModels(a.ID)
	}
	if v.Models == nil {
		v.Models = []string{}
	}
	return v
}

// accountSwitchMu 序列化 OpenCode 账号切换，避免并发请求互相干扰。
var accountSwitchMu sync.Mutex
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"accounts": []accountView{}})
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	rows := s.db.ListAccounts(kind)
	out := make([]accountView, 0, len(rows))
	for _, a := range rows {
		out = append(out, s.accountToView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// handleCreateAccount: POST /v1/accounts
// body: {kind, provider_id, label, credential}
func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db disabled"})
		return
	}
	var in struct {
		Kind       string         `json:"kind"`
		ProviderID string         `json:"provider_id"`
		Label      string         `json:"label"`
		ExternalID string         `json:"external_id"`
		Credential map[string]any `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.Kind == "" {
		in.Kind = db.AccountKindOpenCode
	}
	if in.ProviderID == "" {
		in.ProviderID = in.Kind
	}
	if in.Label == "" {
		in.Label = in.ProviderID
	}
	if in.Credential == nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "credential required"})
		return
	}
	a, err := s.db.CreateAccount(db.Account{
		Kind:       in.Kind,
		ProviderID: in.ProviderID,
		Label:      in.Label,
		ExternalID: in.ExternalID,
	}, in.Credential)
	if err != nil {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, s.accountToView(a))
}

// handleGetAccount: GET /v1/accounts/{id}
func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db disabled"})
		return
	}
	id := r.PathValue("id")
	row, _, ok := s.db.GetAccount(id)
	if !ok {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.accountToView(row))
}
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db disabled"})
		return
	}
	id := r.PathValue("id")
	if !s.db.DeleteAccount(id) {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleActivateAccount: POST /v1/accounts/{id}/activate
func (s *Server) handleActivateAccount(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db disabled"})
		return
	}
	id := r.PathValue("id")
	// find kind
	row, _, ok := s.db.GetAccount(id)
	if !ok {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if err := s.db.SetActiveAccount(row.Kind, id); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if r2, _, ok2 := s.db.GetAccount(id); ok2 {
		row = r2
	}
	httpx.WriteJSON(w, http.StatusOK, s.accountToView(row))
}

// handleAccountModels: GET /v1/accounts/{id}/models
func (s *Server) handleAccountModels(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db disabled"})
		return
	}
	id := r.PathValue("id")
	models := s.db.AccountModels(id)
	if models == nil {
		models = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"account_id": id, "models": models})
}

// ensureAccountForModel 在请求使用某模型前，确保其所属账号处于 active 状态。
// 若模型未被任何账号认领，或账号已是 active，或切换失败，均 best-effort 处理，
// 不阻断请求（OpenCode 侧自行决定能否处理）。
func (s *Server) ensureAccountForModel(ctx context.Context, ref upstream.OCModelRef) {
	if s.db == nil {
		return
	}
	accountID := s.db.ModelsForAccount(ref.String())
	if accountID == "" {
		return
	}
	row, _, ok := s.db.GetAccount(accountID)
	if !ok {
		return
	}
	// 已是 active 则跳过
	if row.Active {
		return
	}
	accountSwitchMu.Lock()
	defer accountSwitchMu.Unlock()

	// 重新检查（可能已被并发切换）
	if r2, _, ok2 := s.db.GetAccount(accountID); ok2 && r2.Active {
		return
	}
	if err := s.db.SetActiveAccount(row.Kind, accountID); err != nil {
		s.log.Warnf("switch active account %s failed: %v", accountID, err)
		return
	}
	// OpenCode 侧同步切换（best-effort）
	if row.Kind == db.AccountKindOpenCode && row.ExternalID != "" {
		provider := row.ProviderID
		if provider == "" {
			provider = "opencode"
		}
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	bin, err := opencode.FindBinary(s.cfg.OpencodeBin)
	if err != nil {
		s.log.Warnf("ensureAccountForModel: find binary: %v", err)
		return
	}
	cmd := exec.CommandContext(ctx2, bin, "auth", "switch", provider, row.ExternalID)
		if out, err := cmd.CombinedOutput(); err != nil {
			s.log.Warnf("opencode auth switch %s %s failed: %v (%s)", provider, row.ExternalID, err, strings.TrimSpace(string(out)))
		} else {
			s.log.Infof("switched opencode active account -> %s (%s)", row.ExternalID, row.Label)
		}
	}
}
// 自动发现该账号可见的模型列表并写入 account_models。
func (s *Server) handleDiscoverAccount(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "db disabled"})
		return
	}
	id := r.PathValue("id")
	row, cred, ok := s.db.GetAccount(id)
	if !ok {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	models, extID, err := s.discoverAccountModels(r.Context(), row, cred)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	s.db.ReplaceAccountModels(id, models)
	if extID != "" && row.ExternalID == "" {
		s.db.UpdateAccountExternalID(id, extID)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"account_id": id, "models": models, "external_id": extID})
}