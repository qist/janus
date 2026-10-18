package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"janus/internal/db"
	"janus/internal/gateway"
	"janus/internal/httpx"
)

// probe.go —— provider / credential 的最小探测与目录同步。
//
// 这里不引入厂商 SDK：所有 provider 都按「HTTP + 凭据头」描述，
// 差异只体现在 providers.type 与 credentials.secret 的键名上。

// credentialHTTP 把一份凭据解成上游需要的请求头。
func credentialHTTP(p db.Provider, c db.Credential) (map[string]string, error) {
	plain, err := db.DecryptBlob(c.Secret)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt credential: %w", err)
	}
	var sec map[string]any
	if err := json.Unmarshal(plain, &sec); err != nil {
		return nil, fmt.Errorf("credential secret is not a JSON object: %w", err)
	}
	str := func(k string) string {
		if v, ok := sec[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	h := map[string]string{}
	switch p.AuthKind {
	case "none":
		// 无鉴权
	case "oauth":
		tok := str("access_token")
		if tok == "" {
			return nil, fmt.Errorf("credential has no access_token")
		}
		h["Authorization"] = "Bearer " + tok
	default:
		key := str("api_key")
		if key == "" {
			key = str("access_token")
		}
		if key == "" {
			return nil, fmt.Errorf("credential has no api_key")
		}
		if p.Type == gateway.ProtocolAnthropic {
			h["x-api-key"] = key
			h["anthropic-version"] = "2023-06-01"
		} else {
			h["Authorization"] = "Bearer " + key
		}
	}
	// provider 级附加静态头（如桌面端身份头）
	if len(p.Headers) > 0 {
		var extra map[string]string
		if err := json.Unmarshal(p.Headers, &extra); err == nil {
			for k, v := range extra {
				if k != "" {
					h[k] = v
				}
			}
		}
	}
	return h, nil
}

// modelsURL 返回厂商目录端点。
func modelsURL(p db.Provider) string {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return ""
	}
	if p.Type == gateway.ProtocolAnthropic {
		return base + "/v1/models?limit=1000"
	}
	return base + "/models"
}

// probeProvider 发一个最小的目录请求，验证端点与凭据可用。
func (s *Server) probeProvider(ctx context.Context, p db.Provider, c db.Credential) error {
	url := modelsURL(p)
	if url == "" {
		return fmt.Errorf("provider %s has no base_url", p.ID)
	}
	headers, err := credentialHTTP(p, c)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream http %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return nil
}

// handleTestProvider 用该 provider 的第一份启用凭据做探测。
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	p, found := st.GetProvider(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	var creds []db.Credential
	for _, c := range st.ListCredentials(p.ID) {
		if c.Enabled {
			creds = append(creds, c)
		}
	}
	if len(creds) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no_credential", "provider has no enabled credential", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.probeProvider(ctx, p, creds[0]); err != nil {
		st.MarkCredentialError(creds[0].ID, err.Error(), 0)
		writeAPIError(w, http.StatusBadGateway, "upstream_error", err.Error(), map[string]any{"credential_id": creds[0].ID})
		return
	}
	st.MarkCredentialUsed(creds[0].ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "credential_id": creds[0].ID})
}

// handleDiscoverProvider 拉厂商 /models，覆盖该 provider 的 discovered 条目。
func (s *Server) handleDiscoverProvider(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	p, found := st.GetProvider(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	var cred db.Credential
	for _, c := range st.ListCredentials(p.ID) {
		if c.Enabled {
			cred = c
			break
		}
	}
	if cred.ID == "" {
		writeAPIError(w, http.StatusBadRequest, "no_credential", "provider has no enabled credential", nil)
		return
	}
	url := modelsURL(p)
	if url == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_provider", "provider has no base_url", nil)
		return
	}
	headers, err := credentialHTTP(p, cred)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_credential", err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_provider", err.Error(), nil)
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		st.MarkCredentialError(cred.ID, err.Error(), 0)
		writeAPIError(w, http.StatusBadGateway, "upstream_error", err.Error(), nil)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("upstream http %d: %s", resp.StatusCode, truncate(string(body), 300))
		st.MarkCredentialError(cred.ID, msg, 0)
		writeAPIError(w, http.StatusBadGateway, "upstream_error", msg, nil)
		return
	}

	models, err := parseModelCatalog(p, body)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "upstream_error", err.Error(), nil)
		return
	}
	st.ReplaceProviderModels(p.ID, models)
	st.MarkCredentialUsed(cred.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"provider_id": p.ID, "count": len(models)})
}

// parseModelCatalog 解析 OpenAI 风格 {"data":[{id}]} 或 Anthropic 风格。
func parseModelCatalog(p db.Provider, body []byte) ([]db.Model, error) {
	var doc struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("model catalog is not JSON: %w", err)
	}
	var out []db.Model
	seen := map[string]bool{}
	add := func(id, name string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		out = append(out, db.Model{UpstreamID: id, DisplayName: name, Enabled: true})
	}
	for _, m := range doc.Data {
		add(m.ID, m.DisplayName)
	}
	for _, m := range doc.Models {
		add(m.ID, m.Name)
	}
	return out, nil
}
