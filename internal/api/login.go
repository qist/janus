package api

import (
	"encoding/base64"
	"net/http"
	"strings"

	"janus/internal/db"
	"janus/internal/httpx"
)

// login.go —— 用户名 + 密码换取 admin key（Web 面板登录）。
//
// 面板本身是静态资源（免鉴权），所有数据端点仍然只认 api_keys 里的 key。
// 登录成功后返回一个 admin key：
//
//   - 第一次登录时创建（name=web-login），明文用主密钥加密后存进 settings；
//   - 之后同一管理员每次登录都返回同一个 key，不会随登录次数膨胀；
//   - 存储键带 "secret:" 前缀，GET /api/v1/settings 会过滤掉，不会外泄。
//
// 这样前端只需把 key 放进 localStorage，和手工粘贴 admin key 的用法完全一致
// （curl 也能直接用），不需要服务端 session / cookie。

// loginKeyPrefix 是「登录签发的 admin key」的 settings 键前缀。
const loginKeyPrefix = "secret:login_key:"

// handleLogin 校验管理员账号密码并签发 admin key。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "persistence_disabled",
			"login requires persistence (JANUS_DB)", nil)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSONBody(w, r, s.cfg.MaxBodyBytes, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.Password == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "username and password are required", nil)
		return
	}

	admin, found := s.db.FindAdmin(in.Username)
	// 统一错误：不区分「用户不存在」与「密码错误」，避免枚举账号。
	if !found || !db.VerifyPassword(in.Password, admin.PasswordHash) {
		writeAPIError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password", nil)
		return
	}

	key, err := s.loginAdminKey(admin)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "login_failed", err.Error(), nil)
		return
	}
	s.log.Infof("admin login: user=%s ip=%s", admin.Username, httpx.ClientIP(r, s.cfg.TrustProxy))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"key":      key,
		"username": admin.Username,
		"kind":     db.KeyKindAdmin,
	})
}

// loginAdminKey 返回该管理员的登录 key，没有就创建并加密留存。
func (s *Server) loginAdminKey(admin db.AdminUser) (string, error) {
	settingKey := loginKeyPrefix + admin.ID
	if enc, ok := s.db.GetSetting(settingKey); ok && enc != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(enc); err == nil {
			if plain, err := db.DecryptBlob(raw); err == nil && len(plain) > 0 {
				// 只复用仍然有效的 key（可能已被轮换/吊销）。
				if k, found := s.db.LookupKey(string(plain)); found &&
					k.Kind == db.KeyKindAdmin && !k.Revoked {
					return string(plain), nil
				}
			}
		}
	}

	_, plain, err := s.db.CreateKey(db.KeyKindAdmin, "web-login", []string{ScopeAdmin}, nil, nil, 0)
	if err != nil {
		return "", err
	}
	if blob, err := db.EncryptBlob([]byte(plain)); err == nil {
		// 存失败不影响本次登录，只是下次会再签一个新 key。
		_ = s.db.SetSetting(settingKey, base64.RawURLEncoding.EncodeToString(blob))
	}
	return plain, nil
}
