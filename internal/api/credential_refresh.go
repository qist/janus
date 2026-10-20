package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"janus/internal/channel/workbuddy"
	"janus/internal/db"
	"janus/internal/httpx"
)

// credential_refresh.go —— 凭据「刷新授权 / 重新授权」。
//
// 按 provider.auth_kind 分派：
//
//	workbuddy → 用 refreshToken 换新的 accessToken（WorkBuddy 渠道私有协议）
//	oauth     → 标准 OAuth2 refresh_token 授权（需 secret 里带 token_url）
//	api_key / none → 不适用
//
// 续期成功后把新 secret 回写数据库（密钥仍走 AES-GCM 加密）。OpenCode/厂商若
// 拒绝续期（refreshToken 过期或撤销），返回 502 并提示「重新授权」——即用新的
// access_token 覆盖凭据。
func (s *Server) handleRefreshCredential(w http.ResponseWriter, r *http.Request) {
	st, ok := s.storeOrError(w)
	if !ok {
		return
	}
	cred, found := st.GetCredential(r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "credential not found", nil)
		return
	}
	prov, found := st.GetProvider(cred.ProviderID)
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "provider not found", nil)
		return
	}
	plain, err := db.DecryptBlob(cred.Secret)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "decrypt_failed", err.Error(), nil)
		return
	}
	var secret map[string]any
	if err := json.Unmarshal(plain, &secret); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_credential",
			"credential secret is not a JSON object", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var next map[string]any
	switch prov.AuthKind {
	case "workbuddy":
		next, err = refreshWorkBuddy(ctx, secret)
	case "oauth":
		next, err = refreshOAuthSecret(ctx, secret)
	default:
		writeAPIError(w, http.StatusBadRequest, "unsupported",
			fmt.Sprintf("auth_kind %q does not support refresh", prov.AuthKind), nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "refresh_failed", err.Error(), nil)
		return
	}

	updated, err := st.UpdateCredential(cred.ID, nil, next, nil, nil)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	view := credentialToView(st, updated)
	view.AuthKind = prov.AuthKind
	httpx.WriteJSON(w, http.StatusOK, view)
}

// refreshWorkBuddy 用 WorkBuddy 渠道的续期接口换新 accessToken。
func refreshWorkBuddy(ctx context.Context, secret map[string]any) (map[string]any, error) {
	cred, err := workbuddy.CredentialFromSecret(secret)
	if err != nil {
		return nil, err
	}
	if cred.RefreshToken == "" {
		return nil, errors.New("凭据没有 refreshToken，无法续期；请重新授权（重新导出 WorkBuddy 明文凭据）")
	}
	client := workbuddy.NewClient(cred, "")
	if err := client.Refresh(ctx); err != nil {
		return nil, err
	}
	out := client.Credential().Secret()
	// 保留调用方附加的自定义键（如备注），避免刷新时丢失。
	for k, v := range secret {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out, nil
}

// refreshOAuthSecret 走标准 OAuth2 refresh_token 授权。
//
// 需要 secret 里带 token_url；client_id/client_secret 可选（有则一起提交）。
// 成功后回写 access_token / refresh_token / expires_at。
func refreshOAuthSecret(ctx context.Context, secret map[string]any) (map[string]any, error) {
	str := func(k string) string {
		if v, ok := secret[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	tokenURL := str("token_url")
	refresh := str("refresh_token")
	if tokenURL == "" || refresh == "" {
		return nil, errors.New("凭据缺少 refresh_token 或 token_url，无法自动续期；请点击「重新授权」粘贴新的 access_token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	if cid := str("client_id"); cid != "" {
		form.Set("client_id", cid)
	}
	if cs := str("client_secret"); cs != "" {
		form.Set("client_secret", cs)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("续期失败 http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("续期响应不是 JSON: %s", truncate(string(raw), 200))
	}
	if tok.AccessToken == "" {
		return nil, errors.New("续期响应没有 access_token")
	}
	out := map[string]any{}
	for k, v := range secret {
		out[k] = v
	}
	out["access_token"] = tok.AccessToken
	if tok.RefreshToken != "" {
		out["refresh_token"] = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		out["expires_at"] = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return out, nil
}
