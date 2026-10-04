package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	cfg Config
	log *Logger

	up        *Upstream
	bus       *EventBus
	store     *Store
	models    *ModelCache
	usage     *UsageClient
	tools     *ToolBridge
	responses *responseStore

	perKeyLimit *RateLimiter
	globalLimit *RateLimiter
	metrics     *metrics
	httpc       *http.Client // 用于下载远程附件，与上游客户端分开

	known map[string]struct{} // 曾经创建过的上游 session，供 janitor 回收

	// ownedMu 保护 owned：本桥创建/使用过的上游 session。
	// 权限自动应答靠它区分"本桥的 agent 会话"和 desktop 端会话——不能只看
	// 当前有没有在飞请求，因为 agent 会在两次请求之间异步发起工具权限请求。
	ownedMu sync.Mutex
	owned   map[string]time.Time

	// mcpAllow 非空时限制内置 MCP 端点 /mcp/{token} 的来源（IP/CIDR）。
	mcpAllow []netip.Prefix

	// db 为 nil 时纯内存；非 nil 时响应与会话映射落 SQLite。
	db *dbStore
}

func NewServer(cfg Config, log *Logger) *Server {
	cfg.applyDefaults()
	up := NewUpstream(cfg, log)
	srv := &Server{
		cfg:         cfg,
		log:         log,
		up:          up,
		bus:         NewEventBus(up, log, cfg.PermissionReply),
		store:       NewStore(log, cfg.SessionTTL, cfg.MaxConversations),
		models:      NewModelCache(60 * time.Second),
		usage:       NewUsageClient(cfg, log),
		tools:       NewToolBridge(log, cfg.ToolCallWait),
		responses:   NewResponseStore(cfg.ResponseTTL),
		perKeyLimit: NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitBurst),
		globalLimit: NewRateLimiter(cfg.RateLimitGlobalRPM, 0),
		httpc: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		known: map[string]struct{}{},
		owned: map[string]time.Time{},
	}
	srv.mcpAllow = parseCIDRList(cfg.MCPAllow)
	if !dbMemoryDSN(cfg.DBPath) {
		if opened, err := openDB(cfg.DBPath); err != nil {
			log.Warnf("sqlite open failed (%s): %v; falling back to in-memory", cfg.DBPath, err)
		} else {
			srv.db = opened
			srv.store.attachDB(opened)
			srv.responses.attachDB(opened)
			log.Infof("persistence enabled: %s", cfg.DBPath)
		}
	}
	srv.metrics = newMetrics(func() int { return srv.store.Count() })
	srv.bus.setOwnedFunc(srv.isOwnedSession)
	return srv
}

// markOwnedSession 记录一个属于本桥的上游 session，供权限自动应答识别。
func (s *Server) markOwnedSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.ownedMu.Lock()
	s.owned[sessionID] = time.Now()
	// 上限保护：太多时清掉最旧的（正常 TTL 会回收，这里只兜底防内存泄漏）。
	if len(s.owned) > 4096 {
		var oldestID string
		var oldest time.Time
		for id, t := range s.owned {
			if oldestID == "" || t.Before(oldest) {
				oldestID, oldest = id, t
			}
		}
		delete(s.owned, oldestID)
	}
	s.ownedMu.Unlock()
}

// isOwnedSession 判断某上游 session 是否由本桥创建/服务过。
// 超过 6 小时的记录视为过期（会话早该回收了）。
func (s *Server) isOwnedSession(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.ownedMu.Lock()
	defer s.ownedMu.Unlock()
	t, ok := s.owned[sessionID]
	if !ok {
		return false
	}
	if time.Since(t) > 6*time.Hour {
		delete(s.owned, sessionID)
		return false
	}
	return true
}

// persistConv 把"会话键 → 上游 sessionID（+ model/agent/dir）"落库，
// 供进程重启后用 previous_response_id / 同一锚点续上同一个上游 session。
func (s *Server) persistConv(conv *Conversation) {
	if s.db == nil || conv == nil {
		return
	}
	sid := conv.snapshotSessionID()
	if sid == "" {
		return
	}
	s.db.saveConv(dbConversation{
		Key:        conv.Key,
		SessionID:  sid,
		ProviderID: conv.model.ProviderID,
		ModelID:    conv.model.ID,
		Variant:    conv.model.Variant,
		Agent:      conv.agent,
		Directory:  conv.directory,
	})
}

// sweepPermissions 定时扫描待审批权限，补齐事件流可能漏掉的请求。
//
// 背景：agent 调自己的工具（shell/read…）访问会话目录之外时会先 ask。
// 这个请求可能发生在两次客户端请求之间的空档，此时事件流虽然收到了，
// 但旧判定（"当前有订阅者"）会漏掉；所以按"本桥拥有的 session"再扫一遍。
func (s *Server) sweepPermissions(ctx context.Context) {
	if s.cfg.PermissionReply == "" || s.cfg.PermissionReply == "off" {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		perms, err := s.up.ListPermissions(cctx, s.cfg.Directory)
		if err != nil {
			cancel()
			continue
		}
		for _, p := range perms {
			if p.ID == "" || p.SessionID == "" || !s.isOwnedSession(p.SessionID) {
				continue
			}
			if err := s.up.ReplyPermission(cctx, p.SessionID, p.ID, s.cfg.PermissionReply); err != nil {
				s.log.Warnf("permission sweep reply failed: request=%s: %v", p.ID, err)
				continue
			}
			s.log.Infof("permission answered (sweep): session=%s request=%s decision=%s action=%s resources=%v",
				p.SessionID, p.ID, s.cfg.PermissionReply, p.Action, p.Resources)
		}
		cancel()
	}
}

// applyDefaults 兜底零值配置。
//
// 配置文件的正常路径会填好这些，但 NewServer 也可能被测试/嵌入方用部分 Config 调用；
// 零值很危险（MaxBodyBytes=0 会让所有 POST 读到 EOF，RequestTimeout=0 会立刻超时）。
func (c *Config) applyDefaults() {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 8 << 20
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 600 * time.Second
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 3 * time.Second
	}
	if c.IdlePollInterval <= 0 {
		c.IdlePollInterval = time.Second
	}
	if c.StreamHeartbeat <= 0 {
		c.StreamHeartbeat = 15 * time.Second
	}
	if c.ToolCallWait <= 0 {
		c.ToolCallWait = 5 * time.Minute
	}
	if c.PermissionReply == "" {
		c.PermissionReply = "once"
	}
	if c.ResponseTTL <= 0 {
		c.ResponseTTL = 30 * time.Minute
	}
	if c.MaxConversations <= 0 {
		c.MaxConversations = 256
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = 30 * time.Minute
	}
}

// ---------- 路由 ----------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /ui", s.handleUI)
	// 根路径直接进用量面板，省得记地址。
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	mux.HandleFunc("GET /v1/models", s.handleListModels)
	mux.HandleFunc("GET /v1/usage", s.handleUsage)
	mux.HandleFunc("POST /v1/responses", s.handleCreateResponse)
	mux.HandleFunc("GET /v1/responses/{id}", s.handleGetResponse)
	mux.HandleFunc("DELETE /v1/responses/{id}", s.handleDeleteResponse)
	// 用 {id...} 而不是 {id}：模型名形如 provider/model 含斜杠，
	// 单段 {id} 匹配不到，会掉进 /v1/ 的兜底 404。
	mux.HandleFunc("GET /v1/models/{id...}", s.handleGetModel)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/completions", s.handleLegacyCompletions)

	// 内置 MCP server：OpenCode 以 remote MCP 方式注册它，用来调用客户端声明的工具。
	// 既是路由，也当鉴权（URL 里的 token 是 128 位随机串）。
	mux.HandleFunc("/mcp/{token}", s.handleMCP)

	// 其余 /v1/* 一律返回 OpenAI 格式 404（客户端不会因解析失败而崩）
	mux.HandleFunc("/v1/", s.handleUnknownV1)

	// 顺序：request-id → CORS → 限流 → 业务。
	// 限流放在 CORS 之后，OPTIONS 预检已在中间件内豁免。
	return s.withRequestID(s.withCORS(s.withRateLimit(s.withMetrics(mux))))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	info := map[string]any{
		"status":   "ok",
		"upstream": s.cfg.Upstream,
	}
	if err := s.up.do(ctx, http.MethodGet, "/api/info", nil, nil, nil); err != nil {
		info["status"] = "upstream_unreachable"
		info["error"] = Redact(err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	q := r.URL.Query()
	force := q.Get("refresh") == "1"
	list, err := s.models.Get(r.Context(), s.up, s.cfg.Directory, force)
	if err != nil {
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}

	// 可选过滤（方便脚本/模型选择器），见 ModelFilter。
	filter := ModelFilter{
		Provider:   strings.TrimSpace(q.Get("provider")),
		Modality:   strings.TrimSpace(q.Get("modality")),
		Tools:      q.Get("tools") == "true" || q.Get("tools") == "1",
		MinContext: atoiOrZero(q.Get("min_context")),
	}

	out := ModelList{Object: "list", Data: make([]ModelObject, 0, len(list)+1)}

	// 首条放一个虚拟模型 "default"，让客户端下拉框里就有"用上游默认"这个选项，
	// 不必写死 fledge-alpha-free 这类具体名字（上游换默认时客户端无需改配置）。
	// 只有在没有任何过滤条件时才放，否则会污染过滤结果。
	if filter.Empty() {
		if ref, err := s.resolveDefaultModel(r.Context(), list); err == nil {
			if def := FindModel(list, ref); def != nil {
				obj := ToOpenAI(*def)
				obj.ID = "default"
				obj.OwnedBy = def.ProviderID
				out.Data = append(out.Data, obj)
			}
		} else {
			s.log.Warnf("cannot resolve configured default model: %v", err)
		}
	}

	for _, m := range list {
		if filter.Match(m) {
			out.Data = append(out.Data, ToOpenAI(m))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ModelFilter 是 /v1/models 的可选过滤条件（零值 = 不过滤）。
type ModelFilter struct {
	Provider   string // 只保留该 provider
	Modality   string // 只保留支持该输入模态的（text/image/pdf/audio/video）
	Tools      bool   // 只保留支持工具调用的
	MinContext int    // 只保留上下文 >= 该值的
}

func (f ModelFilter) Empty() bool {
	return f.Provider == "" && f.Modality == "" && !f.Tools && f.MinContext == 0
}

func (f ModelFilter) Match(m OCModel) bool {
	if !m.Enabled {
		return false
	}
	if f.Provider != "" && m.ProviderID != f.Provider {
		return false
	}
	if f.Tools && !m.Capabilities.Tools {
		return false
	}
	if f.MinContext > 0 && m.Limit.Context < f.MinContext {
		return false
	}
	if f.Modality != "" {
		ok := false
		for _, in := range m.Capabilities.Input {
			if in == f.Modality {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	id := r.PathValue("id")
	list, err := s.models.Get(r.Context(), s.up, s.cfg.Directory, false)
	if err != nil {
		st, typ, msg := s.mapUpstreamError(err)
		writeOpenAIError(w, st, typ, msg, "")
		return
	}
	ref, err := s.resolveModel(r.Context(), id, list)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error(), "model_not_found")
		return
	}
	for _, m := range list {
		if m.ProviderID == ref.ProviderID && m.ID == ref.ID {
			detail := ToDetail(m)
			if strings.EqualFold(strings.TrimSpace(id), "default") {
				detail.ID = "default"
			}
			writeJSON(w, http.StatusOK, detail)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("model %q not found", id), "model_not_found")
}

// handleLegacyCompletions 把 /v1/completions 降级为单轮 chat。
func (s *Server) handleLegacyCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	var legacy struct {
		Model     string `json:"model"`
		Prompt    any    `json:"prompt"`
		Stream    bool   `json:"stream"`
		MaxTokens *int   `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"invalid JSON body: "+err.Error(), "")
		return
	}

	var prompt string
	switch p := legacy.Prompt.(type) {
	case string:
		prompt = p
	case []any:
		var sb strings.Builder
		for _, x := range p {
			sb.WriteString(fmt.Sprint(x))
		}
		prompt = sb.String()
	default:
		prompt = fmt.Sprint(p)
	}
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"prompt must be a string", "invalid_prompt")
		return
	}

	chat := ChatRequest{
		Model:     legacy.Model,
		Messages:  []ChatMessage{{Role: "user", Content: MessageContent{Text: prompt}}},
		Stream:    legacy.Stream,
		MaxTokens: legacy.MaxTokens,
	}
	// 复用 chat handler：把原始 body 换成 chat 形状
	rec := r.Clone(r.Context())
	rec.Body = io.NopCloser(strings.NewReader(mustJSON(chat)))
	rec.ContentLength = int64(len(mustJSON(chat)))
	s.handleChatCompletions(w, rec)
}

// handleUsage 暴露账号余额与用量。
//
// 这是本桥的扩展端点（OpenAI 没有对应接口）：OpenCode 的本地 server 只给
// 「已消耗」，余额得走 console API。客户端一般不会调它，主要给人/监控用。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	if !s.cfg.UsageEnabled {
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
			"usage endpoint disabled (BRIDGE_USAGE_ENABLED=false)", "disabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	rep, err := s.usage.Report(ctx)
	if err != nil {
		s.log.Warnf("usage report failed: %v", err)
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
			"cannot read usage: "+err.Error(), "usage_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleUnknownV1(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("unknown endpoint %q; implemented: /v1/models, /v1/models/{id}, /v1/chat/completions, /v1/completions, /v1/usage",
			r.URL.Path),
		"unknown_endpoint")
}

// ---------- 鉴权 ----------

func (s *Server) checkAuth(r *http.Request) error {
	if s.cfg.APIKey == "" {
		return nil // 未配置 → 放行（启动时已打 WARN）
	}
	got := bearer(r)
	if got == "" {
		got = r.Header.Get("x-api-key")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIKey)) != 1 {
		return fmt.Errorf("invalid API key")
	}
	return nil
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// withRequestID 给每个响应带上 X-Request-Id，方便和上游日志对账。
// 客户端给了就沿用（便于端到端串联 trace），没给就生成一个。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = newID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

// ---------- CORS ----------
//
// 默认**不发任何 CORS 头**（= 拒绝跨源浏览器访问）。
// 之前的默认是"回显请求方 Origin 且带 credentials"，等于对任意站点开放，
// 是审计里的一条安全缺口。桌面客户端（Trae/CodeBuddy）不受 CORS 约束，
// 所以收紧默认值不影响它们。
//
//	BRIDGE_CORS_ORIGIN 未设置 → 不发 CORS 头
//	BRIDGE_CORS_ORIGIN=*      → Allow-Origin: *（不带 credentials）
//	BRIDGE_CORS_ORIGIN=a,b    → 命中列表的 Origin 才放行（带 credentials）
func (s *Server) withCORS(next http.Handler) http.Handler {
	allowAll := s.cfg.CORSOrigin == "*"
	explicit := !allowAll && strings.TrimSpace(s.cfg.CORSOrigin) != ""

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 公开的静态路径（用量面板、健康检查、根跳转）不受 BRIDGE_CORS_ORIGIN
		// 限制：这些页面不含敏感数据，鉴权仍在数据端点（/v1/*、/metrics）。
		// 不限定方法，否则 OPTIONS 预检会被下面的 origin 校验拦掉。
		public := isPublicPath(r.URL.Path)

		allowed := false
		if public {
			allowed = true
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
		} else if origin != "" {
			switch {
			case allowAll:
				allowed = true
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case explicit && originAllowed(origin, s.cfg.CORSOrigin):
				allowed = true
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		if allowed {
			if !public {
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, X-Session-ID, X-OpenCode-Session, X-OpenCode-Agent, X-OpenCode-Directory, X-Api-Key")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, X-RateLimit-Limit-Requests, X-RateLimit-Remaining-Requests, X-RateLimit-Reset-Requests, X-Janus-Session, X-Janus-Conversation")
		}

		if r.Method == http.MethodOptions {
			if origin != "" && !allowed {
				// 明确的预检失败比"静默无头"更好排查
				writeOpenAIError(w, http.StatusForbidden, "invalid_request_error",
					"origin not allowed by BRIDGE_CORS_ORIGIN", "cors_origin_denied")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isPublicPath 返回无需 API key、且不受 CORS 限制的静态路径。
//
// 这些路径只提供静态页面/健康信息，没有敏感数据；真正要鉴权的是
// /v1/*（用量、模型、补全）和 /metrics（由 BRIDGE_METRICS_PUBLIC 决定）。
func isPublicPath(p string) bool {
	switch p {
	case "/", "/ui", "/healthz":
		return true
	}
	return false
}

func originAllowed(origin, allowed string) bool {
	for _, a := range strings.Split(allowed, ",") {
		if strings.TrimSpace(a) == origin {
			return true
		}
	}
	return false
}

// parseCIDRList 解析逗号分隔的 IP / CIDR 列表，忽略空项与非法项。
// 裸 IP 会按 /32（IPv4）或 /128（IPv6）处理。
func parseCIDRList(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p)
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil {
			bits := 128
			if a.Is4() {
				bits = 32
			}
			out = append(out, netip.PrefixFrom(a, bits))
		}
	}
	return out
}

// mcpSourceAllowed 判断 /mcp/{token} 的来源是否在白名单内。
// 白名单为空 = 不限制（默认，OpenCode 与本桥同机走回环）。
func (s *Server) mcpSourceAllowed(r *http.Request) bool {
	if len(s.mcpAllow) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range s.mcpAllow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ---------- 小工具 ----------

func readAllLimit(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
