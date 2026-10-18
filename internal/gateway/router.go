// Package gateway 实现「模型面」的唯一入口：把客户端传来的模型名解析成
// (provider, credential, upstream model)，并负责与厂商通信。
//
// 设计见 docs/REFACTOR.md §3.1。核心约束：
//   - 映射键可以带作用域（credential / provider / global / role），
//     同一个别名在不同凭据下可以指向不同模型；
//   - 凭据不再有「Active」全局状态，同一 provider 的多份凭据天然并发；
//   - 没有显式 hint 且候选凭据多于一份时返回歧义错误，绝不随机挑。
package gateway

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"janus/internal/db"
)

// 协议常量（与 providers.type 对应）。
const (
	ProtocolOpenAIChat      = "openai-chat"
	ProtocolOpenAIResponses = "openai-responses"
	ProtocolAnthropic       = "anthropic-messages"
)

var (
	// ErrModelNotFound 表示模型无法解析（provider 未知）。
	ErrModelNotFound = errors.New("model not found")
	// ErrNoCredential 表示解析到了 provider，但没有任何可用凭据。
	ErrNoCredential = errors.New("no available credential")
	// ErrInvalidRoute 表示路由目标自身是别名（禁止链式）。
	ErrInvalidRoute = errors.New("invalid route")
)

// AmbiguousError 表示同名模型对应多份凭据且调用方没有指定。
type AmbiguousError struct {
	Model      string
	Candidates []string // 形如 "work/claude-opus-4-1"
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("model %q is ambiguous; candidates: %s (use \"<credential>/<model>\" or X-Janus-Credential)",
		e.Model, strings.Join(e.Candidates, ", "))
}

// RouteInput 是一次解析的输入。
type RouteInput struct {
	Model          string   // 客户端传来的模型名
	Protocol       string   // 期望的上游协议（空 = 不限）
	CredentialHint string   // X-Janus-Credential 或 "<label>/" 前缀
	ProviderIDs    []string // key 白名单（空 = 全部）
	CredentialIDs  []string // key 白名单（空 = 全部）
}

// RouteResult 是解析结果。
type RouteResult struct {
	ProviderID   string `json:"provider_id"`
	CredentialID string `json:"credential_id"`
	UpstreamID   string `json:"upstream_id"`
	Variant      string `json:"variant,omitempty"`
	Rule         string `json:"rule"`        // 命中说明，进日志与 /routes/resolve
	Passthrough  bool   `json:"passthrough"` // provider 已知但模型不在目录
}

// Router 把数据库里的 providers / credentials / models / routes 变成解析器。
type Router struct {
	store *db.Store
}

// New 构造解析器。store 为 nil 时所有解析都会失败（内存模式仅用于测试）。
func New(store *db.Store) *Router { return &Router{store: store} }

func nowUnix() int64 { return time.Now().Unix() }

// Store 暴露底层存储（调用方偶尔需要读凭据明文）。
func (r *Router) Store() *db.Store { return r.store }

// splitVariant 拆出 ":variant" 后缀（variant 不含 '/'）。
func splitVariant(raw string) (id, variant string) {
	if i := strings.LastIndex(raw, ":"); i > 0 && !strings.Contains(raw[i:], "/") {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

// Route 按 docs/REFACTOR.md §3.1 的优先级解析。
func (r *Router) Route(in RouteInput) (RouteResult, error) {
	if r == nil || r.store == nil {
		return RouteResult{}, ErrModelNotFound
	}
	raw := strings.TrimSpace(in.Model)
	if raw == "" {
		return RouteResult{}, ErrModelNotFound
	}
	id, variant := splitVariant(raw)

	// ---- 1. 显式限定：<label>/<model> 或 <provider>/<model> ----
	hint := strings.TrimSpace(in.CredentialHint)
	if i := strings.Index(id, "/"); i > 0 {
		head, rest := id[:i], id[i+1:]
		// provider 前缀优先（"anthropic/claude-..." 是常见写法，不能被
		// 同名的凭据 label 抢走）。
		_, isProvider := r.store.GetProvider(head)
		if rest != "" && !isProvider {
			if cred, ok := r.store.FindCredentialByLabel("", head); ok {
				hint = cred.ID
				id = rest
			}
		}
	}
	if hint != "" {
		if cred, ok := r.credentialByHint(hint); ok {
			if res, ok := r.resolveInScope(cred.ProviderID, cred.ID, id, variant); ok {
				res.CredentialID = cred.ID
				res.Rule = "explicit:" + cred.Label
				if err := r.checkAllowed(res, in); err != nil {
					return RouteResult{}, err
				}
				return res, nil
			}
			// 显式凭据下没有规则/目录命中：仍允许透传到该 provider。
			if p, ok := r.store.GetProvider(cred.ProviderID); ok && p.Enabled {
				res := RouteResult{ProviderID: p.ID, CredentialID: cred.ID, UpstreamID: id, Variant: variant, Rule: "explicit:passthrough", Passthrough: true}
				if err := r.checkAllowed(res, in); err != nil {
					return RouteResult{}, err
				}
				return res, nil
			}
		}
	}

	// ---- 2. 路由表（global），并收集目录候选 ----
	var candidates []RouteResult

	// provider/model 形式：直接按目录/规则解析
	if i := strings.Index(id, "/"); i > 0 {
		pid, mid := id[:i], id[i+1:]
		if p, ok := r.store.GetProvider(pid); ok && p.Enabled {
			if res, ok := r.resolveInScope(p.ID, "", mid, variant); ok {
				res.Rule = "provider-prefix:" + p.ID
				candidates = append(candidates, res)
			} else {
				candidates = append(candidates, RouteResult{
					ProviderID: p.ID, UpstreamID: mid, Variant: variant,
					Rule: "provider-prefix:passthrough", Passthrough: true,
				})
			}
		}
	} else {
		// 裸模型名：先查 global 路由，再查目录（唯一即命中）。
		if res, ok := r.resolveGlobal(id, variant); ok {
			candidates = append(candidates, res)
		}
		if m, ok := r.store.FindModelByUpstream("", id); ok {
			candidates = append(candidates, RouteResult{ProviderID: m.ProviderID, UpstreamID: m.UpstreamID, Variant: variant, Rule: "catalog"})
		} else if ms := r.modelsByUpstream(id); len(ms) > 0 {
			for _, m := range ms {
				candidates = append(candidates, RouteResult{ProviderID: m.ProviderID, UpstreamID: m.UpstreamID, Variant: variant, Rule: "catalog"})
			}
		}
	}
	if len(candidates) == 0 {
		return RouteResult{}, fmt.Errorf("%w: %s", ErrModelNotFound, raw)
	}

	// 目录候选需要按 provider 选凭据；多于一份且无 hint → 歧义。
	var out []RouteResult
	var ambiguous []string
	for _, c := range candidates {
		creds := r.credentialsFor(c.ProviderID, in.CredentialIDs)
		switch len(creds) {
		case 0:
			continue
		case 1:
			c.CredentialID = creds[0].ID
			out = append(out, c)
		default:
			for _, cred := range creds {
				ambiguous = append(ambiguous, cred.Label+"/"+c.UpstreamID)
			}
		}
	}
	if len(out) == 0 {
		if len(ambiguous) > 0 {
			return RouteResult{}, &AmbiguousError{Model: raw, Candidates: ambiguous}
		}
		return RouteResult{}, fmt.Errorf("%w: %s", ErrNoCredential, raw)
	}
	if len(out) > 1 {
		var labels []string
		for _, c := range out {
			labels = append(labels, c.ProviderID+"/"+c.UpstreamID)
		}
		return RouteResult{}, &AmbiguousError{Model: raw, Candidates: labels}
	}
	if err := r.checkAllowed(out[0], in); err != nil {
		return RouteResult{}, err
	}
	return out[0], nil
}

// resolveInScope 依次尝试 credential / provider / global 规则，再退到目录。
func (r *Router) resolveInScope(providerID, credentialID, model, variant string) (RouteResult, bool) {
	if credentialID != "" {
		if res, ok := r.matchRoutes(db.RouteScopeCredential, credentialID, model, variant); ok {
			return res, true
		}
	}
	if providerID != "" {
		if res, ok := r.matchRoutes(db.RouteScopeProvider, providerID, model, variant); ok {
			return res, true
		}
	}
	if res, ok := r.matchRoutes(db.RouteScopeGlobal, "", model, variant); ok {
		return res, true
	}
	// 目录：provider + upstream 精确匹配
	if providerID != "" {
		if m, ok := r.store.FindModelByUpstream(providerID, model); ok && m.Enabled {
			return RouteResult{ProviderID: providerID, UpstreamID: m.UpstreamID, Variant: variant, Rule: "catalog"}, true
		}
	}
	return RouteResult{}, false
}

// resolveGlobal 只查 global 作用域规则。
func (r *Router) resolveGlobal(model, variant string) (RouteResult, bool) {
	return r.matchRoutes(db.RouteScopeGlobal, "", model, variant)
}

// matchRoutes 在给定作用域内匹配规则：先精确，再取最长前缀通配。
func (r *Router) matchRoutes(scope, scopeID, model, variant string) (RouteResult, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	var exact *db.ModelRoute
	var wildcard *db.ModelRoute
	wildLen := -1
	for _, rt := range r.store.ListRoutes(scope) {
		if !rt.Enabled {
			continue
		}
		if scope != db.RouteScopeGlobal && !strings.EqualFold(rt.ScopeID, scopeID) {
			continue
		}
		alias := strings.ToLower(strings.TrimSpace(rt.Alias))
		switch {
		case alias == lower:
			if exact == nil || rt.Priority < exact.Priority {
				cp := rt
				exact = &cp
			}
		case strings.HasSuffix(alias, "*"):
			prefix := strings.TrimSuffix(alias, "*")
			if prefix != "" && strings.HasPrefix(lower, prefix) && len(prefix) > wildLen {
				wildLen = len(prefix)
				cp := rt
				wildcard = &cp
			}
		}
	}
	rule := exact
	if rule == nil {
		rule = wildcard
	}
	if rule == nil {
		return RouteResult{}, false
	}
	target := strings.TrimSpace(rule.Target)
	// 目标是别名（自身命中另一条规则）→ 拒绝链式，防循环。
	if r.targetIsAlias(target) {
		return RouteResult{ProviderID: "", UpstreamID: "", Rule: "invalid_route:" + rule.ID}, false
	}
	pid, uid := splitProviderModel(target)
	res := RouteResult{ProviderID: pid, UpstreamID: uid, Variant: variant, Rule: "route:" + rule.ID}
	if res.Variant == "" {
		if _, v := splitVariant(target); v != "" {
			res.Variant = v
		}
	}
	return res, true
}

// targetIsAlias 判断目标是否本身就是某条规则的别名。
func (r *Router) targetIsAlias(target string) bool {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return true
	}
	for _, rt := range r.store.ListRoutes("") {
		if rt.Enabled && strings.EqualFold(strings.TrimSpace(rt.Alias), t) {
			return true
		}
	}
	return false
}

// splitProviderModel 拆 "provider/model[:variant]"。
func splitProviderModel(target string) (providerID, upstreamID string) {
	target, _ = splitVariant(target)
	if i := strings.Index(target, "/"); i > 0 {
		return target[:i], target[i+1:]
	}
	return "", target
}

// credentialByHint 按 id 或 label 找凭据。
func (r *Router) credentialByHint(hint string) (db.Credential, bool) {
	if c, ok := r.store.GetCredential(hint); ok {
		return c, true
	}
	return r.store.FindCredentialByLabel("", hint)
}

// credentialsFor 返回某 provider 下可用凭据（已过滤白名单/禁用/冷却）。
func (r *Router) credentialsFor(providerID string, allow []string) []db.Credential {
	all := r.store.ListCredentials(providerID)
	allowed := map[string]bool{}
	for _, id := range allow {
		allowed[id] = true
	}
	now := nowUnix()
	var out []db.Credential
	for _, c := range all {
		if !c.Enabled {
			continue
		}
		if len(allowed) > 0 && !allowed[c.ID] {
			continue
		}
		if c.FailUntil > 0 && c.FailUntil > now {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

// checkAllowed 校验 key 的 provider 白名单（空 = 全量可用）。
func (r *Router) checkAllowed(res RouteResult, in RouteInput) error {
	if len(in.ProviderIDs) == 0 {
		return nil
	}
	for _, id := range in.ProviderIDs {
		if id == res.ProviderID {
			return nil
		}
	}
	return fmt.Errorf("%w: provider %q not allowed for this key", ErrModelNotFound, res.ProviderID)
}

// modelsByUpstream 找所有 upstream id 相同的目录条目。
func (r *Router) modelsByUpstream(upstreamID string) []db.Model {
	var out []db.Model
	for _, m := range r.store.ListModels("", true) {
		if strings.EqualFold(m.UpstreamID, upstreamID) {
			out = append(out, m)
		}
	}
	return out
}

// Candidates 返回某模型可能的 "provider/model" 列表（错误提示用）。
func (r *Router) Candidates(model string) []string {
	if r == nil || r.store == nil {
		return nil
	}
	id, _ := splitVariant(strings.TrimSpace(model))
	var out []string
	for _, m := range r.store.ListModels("", true) {
		if strings.EqualFold(m.UpstreamID, id) {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}
