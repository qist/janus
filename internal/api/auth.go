package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"janus/internal/db"
	"janus/internal/httpx"
)

// auth.go —— API key 解析与 scope 校验。
//
// 四类 key（见 docs/REFACTOR.md §2.5）：
//
//	admin  → /api/v1/*（管理面）
//	client → /v1/*（模型面 + 会话）
//	agent  → /v1/*（模型面，默认全量可用）
//	tool   → /mcp/{session}（由会话 token 绑定，不走这里）
//
// 失败统一 401，不区分「key 不存在」与「scope 不足」，避免枚举。

// scope 常量。
const (
	ScopeAdmin   = "admin:*"
	ScopeModel   = "model:*"
	ScopeSession = "session:*"
)

// keyCache 缓存 key 摘要 → 记录，避免每个请求都查库。
type keyCache struct {
	mu   sync.Mutex
	rows map[string]cachedKey
}

type cachedKey struct {
	key     db.APIKey
	expires time.Time
}

const keyCacheTTL = 30 * time.Second

func (c *keyCache) get(hash string) (db.APIKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.rows[hash]
	if !ok || time.Now().After(row.expires) {
		return db.APIKey{}, false
	}
	return row.key, true
}

func (c *keyCache) put(hash string, k db.APIKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = map[string]cachedKey{}
	}
	c.rows[hash] = cachedKey{key: k, expires: time.Now().Add(keyCacheTTL)}
}

// Principal 是当前请求的调用者。
type Principal struct {
	Key         db.APIKey
	Kind        string
	Scopes      []string
	Providers   []string
	Credentials []string
	Static      bool // true = cfg.APIKey（测试/应急），视作 admin
}

// HasScope 判断是否具备某 scope（支持 "model:*" 这类前缀）。
func (p Principal) HasScope(scope string) bool {
	if p.Static || p.Kind == db.KeyKindAdmin {
		return true
	}
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
		if strings.HasSuffix(s, "*") && strings.HasPrefix(scope, strings.TrimSuffix(s, "*")) {
			return true
		}
	}
	return false
}

// bearer 取请求里的 key 明文。
func bearer(r *http.Request) string {
	if v := httpx.Bearer(r); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// principal 解析并校验请求的 key。cfg.APIKey 非空时也接受（静态应急 key）。
func (s *Server) principal(r *http.Request) (Principal, error) {
	plain := bearer(r)
	if plain == "" {
		return Principal{}, errors.New("missing API key")
	}
	if s.cfg.APIKey != "" && plain == s.cfg.APIKey {
		return Principal{Kind: db.KeyKindAdmin, Static: true}, nil
	}
	if s.db == nil {
		return Principal{}, errors.New("invalid API key")
	}
	hash := db.HashKey(plain)
	if k, ok := s.keyCache.get(hash); ok {
		return principalFromKey(k), nil
	}
	k, ok := s.db.LookupKey(plain)
	if !ok {
		return Principal{}, errors.New("invalid API key")
	}
	s.keyCache.put(hash, k)
	s.db.TouchKey(k.ID)
	return principalFromKey(k), nil
}

func principalFromKey(k db.APIKey) Principal {
	return Principal{
		Key:         k,
		Kind:        k.Kind,
		Scopes:      db.DecodeStrings(k.Scopes),
		Providers:   db.DecodeStrings(k.ProviderIDs),
		Credentials: db.DecodeStrings(k.CredentialIDs),
	}
}

// requireKind 校验 key 类型；失败统一 401。
func (s *Server) requireKind(r *http.Request, kinds ...string) (Principal, bool) {
	p, err := s.principal(r)
	if err != nil {
		return Principal{}, false
	}
	if p.Static {
		return p, true
	}
	for _, k := range kinds {
		if p.Kind == k {
			return p, true
		}
	}
	return Principal{}, false
}

// writeAPIError 写统一错误体 {"error":{"code","message","details"}}。
func writeAPIError(w http.ResponseWriter, status int, code, msg string, details any) {
	body := map[string]any{"error": map[string]any{"code": code, "message": msg}}
	if details != nil {
		body["error"].(map[string]any)["details"] = details
	}
	httpx.WriteJSON(w, status, body)
}

// adminGuard 包一层：校验 admin key 后再进业务。
func (s *Server) adminGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		p, err := s.principal(r)
		if err != nil || (!p.Static && p.Kind != db.KeyKindAdmin) {
			writeAPIError(rw, http.StatusUnauthorized, "unauthorized", "invalid API key", nil)
			return
		}
		h(rw, r)
	}
}

// modelGuard 校验 client/agent key（模型面）。
func (s *Server) modelGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		p, err := s.principal(r)
		if err != nil {
			writeAPIError(rw, http.StatusUnauthorized, "unauthorized", "invalid API key", nil)
			return
		}
		if !p.Static && p.Kind != db.KeyKindClient && p.Kind != db.KeyKindAgent && p.Kind != db.KeyKindAdmin {
			writeAPIError(rw, http.StatusUnauthorized, "unauthorized", "invalid API key", nil)
			return
		}
		h(rw, r)
	}
}

func describeKind(kind string) string {
	switch kind {
	case db.KeyKindAdmin:
		return "admin"
	case db.KeyKindClient:
		return "client"
	case db.KeyKindAgent:
		return "agent"
	case db.KeyKindTool:
		return "tool"
	default:
		return fmt.Sprintf("unknown(%s)", kind)
	}
}
