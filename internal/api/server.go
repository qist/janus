package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"janus/internal/agent"
	"janus/internal/channel/opencode"
	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/forensics"
	"janus/internal/gateway"
	"janus/internal/httpx"
	"janus/internal/logx"
	"janus/internal/mcpbridge"
	"janus/internal/metricx"
	"janus/internal/modelx"
	"janus/internal/paths"
	"janus/internal/ratelimit"
	"janus/internal/ui"
	"janus/internal/upstream"
	"janus/internal/usage"
	"janus/internal/useragent"
)

type Server struct {
	cfg config.Config
	log *logx.Logger

	up        *upstream.Upstream
	bus       *upstream.EventBus
	store     *Store
	models    *opencode.Cache
	opencode  *opencode.Supervisor // OpenCode 渠道：端点探活 + 托管进程生命周期
	usage     *usage.Client
	tools     *ToolBridge
	responses *responseStore

	perKeyLimit *ratelimit.RateLimiter
	globalLimit *ratelimit.RateLimiter
	metrics     *metricx.Metrics
	httpc       *http.Client         // 用于下载远程附件，与上游客户端分开
	proxyc      *http.Client         // 模型面直连厂商：不设总超时（流式），只限连接阶段
	forensics   *forensics.Forensics // 楔死取证落盘器；nil = 未启用

	known map[string]struct{} // 曾经创建过的上游 session，供 janitor 回收

	// ownedMu 保护 owned：本桥创建/使用过的上游 session。
	// 权限自动应答靠它区分"本桥的 agent 会话"和 desktop 端会话——不能只看
	// 当前有没有在飞请求，因为 agent 会在两次请求之间异步发起工具权限请求。
	ownedMu sync.Mutex
	owned   map[string]time.Time

	// lastModel 记住最近一次"客户端显式指定且解析成功"的真实模型，
	// 作为 claude-* 等别名解析不到时的兜底。这样无需任何配置，
	// 也不会像纯启发式那样挑到本账号没额度的模型（如按量计费的 opencode/*）。
	lastModelMu sync.Mutex
	lastModel   *upstream.OCModelRef

	// mcpAllow 非空时限制内置 MCP 端点 /mcp/{token} 的来源（IP/CIDR）。
	mcpAllow []netip.Prefix

	// db 为 nil 时纯内存；非 nil 时响应与会话映射落 SQLite。
	db *db.Store

	// router 是模型面路由解析器（providers / credentials / routes）。
	router *gateway.Router

	// keyCache 缓存 API key 摘要 → 记录（30s）。
	keyCache keyCache

	// internalKeys 是进程内临时 agent key（janus 拉起 CLI harness 时发给子进程
	// 的模型面回调凭据）。不落库，turn 结束即删除。
	internalMu   sync.Mutex
	internalKeys map[string]internalKey

	// defaultModel 是运行时可切换的默认模型（web 里选），持久化在 DB。
	// 非空时优先于 cfg.DefaultModel；空则回落 cfg.DefaultModel / 上游默认。
	defaultMu    sync.RWMutex
	defaultModel string

	// settingsMu 串行化运行期设置的读改写（PATCH /api/v1/settings）。
	settingsMu sync.Mutex

	// remoteUsage 是「远程分渠道用量」的每凭据缓存（含失败负缓存）。
	remoteUsage *remoteUsageCache

	// agents 是 Agent 面（harness）runtime 注册表，惰性构建；agentMu 保护它。
	agentMu sync.Mutex
	agents  *agent.Registry
	// harnesses 是 harness provisioner 注册表（codex/claude-code/opencode），
	// 同样惰性构建，agentMu 保护。
	harnesses *agent.HarnessRegistry

	// clientSeen 用于「客户端诊断日志」去重：每个不同的客户端签名只打一次，
	// 方便看不同 IDE / 设备到底发了什么（user / 头 / 目录 / 首条 user）。
	clientMu   sync.Mutex
	clientSeen map[string]bool
}

// logClientInfo 打印一次客户端信息（按签名去重）。用于排查跨设备/跨 IDE 的
// 会话定位（user、workspace 头、首条 user、计算出的 key）。
func (s *Server) logClientInfo(r *http.Request, user, dir, key string, msgs []ChatMessage) {
	ccSession := firstNonEmpty(r.Header.Get("x-claude-code-session-id"), r.Header.Get("X-Claude-Code-Session-Id"))
	ip := httpx.ClientIP(r, s.cfg.TrustProxy)
	sig := ip + "\x00" + user + "\x00" + dir + "\x00" + r.UserAgent() + "\x00" +
		r.Header.Get("X-Session-ID") + "\x00" + r.Header.Get("X-OpenCode-Session") + "\x00" + ccSession

	s.clientMu.Lock()
	if s.clientSeen == nil {
		s.clientSeen = map[string]bool{}
	}
	seen := s.clientSeen[sig]
	s.clientSeen[sig] = true
	s.clientMu.Unlock()
	if seen {
		return
	}
	_, scopeReadable := s.scopeOf(r, r.Header.Get("X-OpenCode-Directory"), msgs)
	xff := r.Header.Get("X-Forwarded-For")
	s.log.Infof("client: ip=%q xff=%q user=%q dir=%q ua=%q x-session=%q x-oc-session=%q x-oc-dir=%q x-oc-agent=%q cc-session=%q firstUser=%q key=%s scope=%s",
		ip, xff, user, dir, r.UserAgent(),
		r.Header.Get("X-Session-ID"), r.Header.Get("X-OpenCode-Session"),
		r.Header.Get("X-OpenCode-Directory"), r.Header.Get("X-OpenCode-Agent"), ccSession,
		truncate(firstUserTitle(msgs, 60), 80), key, scopeReadable)

	// 把所有请求头也打一遍，便于找 workspace / 会话 id 藏在哪个头里。
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var hb strings.Builder
	for _, k := range keys {
		hb.WriteString(k)
		hb.WriteByte('=')
		hb.WriteString(truncate(r.Header.Get(k), 80))
		hb.WriteByte(' ')
	}
	s.log.Infof("client headers: %s", truncate(hb.String(), 1200))

	// 有些客户端把信息放在自定义 token（如 Trae 的 Acl-Token）里：解一下 JWT payload。
	if tok := r.Header.Get("Acl-Token"); tok != "" {
		if pl := jwtPayload(tok); pl != "" {
			s.log.Infof("client Acl-Token payload: %s", truncate(pl, 600))
		}
	}
}

// jwtPayload 解出 JWT 的 payload（第 2 段）明文；失败返回空。
func jwtPayload(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	return string(b)
}

// conversationKey 计算会话键：显式会话头 > scope(开启时) > 指纹(system+user+dir)。
// scope 模式用于「没有会话 id」时退化到 IDE+项目 级归属（见 scope.go）。
func (s *Server) conversationKey(r *http.Request, explicit, system, user, dir string, msgs []ChatMessage) string {
	if explicit != "" {
		return "x:" + explicit
	}
	if s.cfg.ScopeKey {
		k, _ := s.scopeOf(r, r.Header.Get("X-OpenCode-Directory"), msgs)
		return k
	}
	return ConversationKey(explicit, system, user, dir)
}

// runtimeDefaultModel 返回 web 里选定的默认模型（可能为空）。
func (s *Server) runtimeDefaultModel() string {
	s.defaultMu.RLock()
	defer s.defaultMu.RUnlock()
	return s.defaultModel
}

// setRuntimeDefaultModel 设置并持久化默认模型（空=清除，回落配置）。
func (s *Server) setRuntimeDefaultModel(v string) {
	v = strings.TrimSpace(v)
	s.defaultMu.Lock()
	s.defaultModel = v
	s.defaultMu.Unlock()
	if s.db != nil {
		s.db.SetSetting("default_model", v)
	}
}

func NewServer(cfg config.Config, log *logx.Logger) *Server {
	cfg.ApplyDefaults()
	up := upstream.New(cfg, log)
	srv := &Server{
		cfg:         cfg,
		log:         log,
		up:          up,
		bus:         upstream.NewEventBus(up, log, cfg.PermissionReply),
		store:       NewStore(log, cfg.SessionTTL, cfg.SharedSessionTTL, cfg.MaxConversations),
		models:      opencode.NewCache(60 * time.Second),
		usage:       usage.New(cfg, log),
		tools:       NewToolBridge(log, cfg.ToolCallWait, cfg.ToolCallWaitFast, cfg.ToolFastTools),
		responses:   NewResponseStore(cfg.ResponseTTL),
		perKeyLimit: ratelimit.New(cfg.RateLimitPerMin, cfg.RateLimitBurst),
		globalLimit: ratelimit.New(cfg.RateLimitGlobalRPM, 0),
		httpc: &http.Client{
			Timeout: 20 * time.Second,
			Transport: useragent.Wrap(&http.Transport{
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     30 * time.Second,
			}),
		},
		proxyc: &http.Client{
			// 不设总超时：流式回答可能跑很久。靠请求 ctx 与连接超时兜底。
			Transport: useragent.Wrap(&http.Transport{
				DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				TLSHandshakeTimeout: 15 * time.Second,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			}),
		},
		known:       map[string]struct{}{},
		owned:       map[string]time.Time{},
		remoteUsage: newRemoteUsageCache(cfg.UsageTTL),
	}
	srv.mcpAllow = httpx.ParseCIDRList(cfg.MCPAllow)
	// 渠道监督器持有 &srv.cfg：热替换端点时读取的是运行时最新配置。
	srv.opencode = opencode.NewSupervisor(log, &srv.cfg, up, srv.bus, mcpbridge.NamePrefix)
	if cfg.WedgeForensics {
		// 楔死取证：仅当数据目录可定位时启用（见 forensics.go）
		srv.forensics = forensics.New(filepath.Join(paths.DataDir(), "forensics"), 20, log)
	}
	if !db.MemoryDSN(cfg.DBPath) {
		if opened, err := db.Open(cfg.DBPath); err != nil {
			log.Warnf("sqlite open failed (%s): %v; falling back to in-memory", cfg.DBPath, err)
		} else {
			srv.db = opened
			srv.store.attachDB(opened)
			srv.responses.attachDB(opened)
			// 运行期设置整体存在 settings.runtime（env 只负责 bootstrap）。
			if raw, ok := opened.GetSettingRaw(db.SettingRuntime); ok {
				if err := srv.cfg.ApplySettings(raw, json.Unmarshal); err != nil {
					log.Warnf("runtime settings ignored: %v", err)
				}
			}
			if v, ok := opened.GetSetting("default_model"); ok {
				srv.defaultModel = v
			}
			srv.router = gateway.New(opened)
			log.Infof("persistence enabled: %s", cfg.DBPath)
		}
	}
	if srv.router == nil {
		// 内存模式（测试）：没有 providers/credentials，路由解析一律失败。
		srv.router = gateway.New(nil)
	}
	srv.metrics = metricx.New(func() int { return srv.store.Count() })
	srv.bus.SetOwnedFunc(srv.isOwnedSession)
	return srv
}

// SetManagedUpstream 登记本次进程自己拉起的上游（OpenCode）PID，
// 进程退出时由 Server.Run 负责回收，避免遗留孤儿进程。
func (s *Server) SetManagedUpstream(pid int) {
	if pid > 0 {
		s.opencode.SetManaged(pid)
	}
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

// rememberModel 记录最近一次显式指定的真实模型，供别名兜底。
func (s *Server) rememberModel(ref upstream.OCModelRef) {
	if ref.ProviderID == "" || ref.ID == "" {
		return
	}
	r := ref
	s.lastModelMu.Lock()
	s.lastModel = &r
	s.lastModelMu.Unlock()
}

// rememberedModel 返回最近记录的真实模型；若它已不在/不可用则返回 false。
func (s *Server) rememberedModel(list []upstream.OCModel) (upstream.OCModelRef, bool) {
	s.lastModelMu.Lock()
	ref := s.lastModel
	s.lastModelMu.Unlock()
	if ref == nil {
		return upstream.OCModelRef{}, false
	}
	for _, m := range list {
		if m.ProviderID == ref.ProviderID && m.ID == ref.ID {
			if m.Enabled && m.Capabilities.Tools {
				return *ref, true
			}
			return upstream.OCModelRef{}, false
		}
	}
	return upstream.OCModelRef{}, false
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
	var hist []byte
	if msgs := conv.snapshotLast(); len(msgs) > 0 {
		if b, err := json.Marshal(msgs); err == nil {
			switch limit := s.cfg.HistoryMaxBytes; {
			case limit <= 0 || len(b) <= limit:
				hist = b
			default:
				// 太大就不落历史（仍落会话映射），避免库无界增长
				s.log.Debugf("history too large to persist for %s: %dB > %dB", conv.Key, len(b), limit)
			}
		}
	}
	s.db.SaveConv(db.Conversation{
		Key:        conv.Key,
		SessionID:  sid,
		ProviderID: conv.model.ProviderID,
		ModelID:    conv.model.ID,
		Variant:    conv.model.Variant,
		Agent:      conv.agent,
		Directory:  conv.directory,
		Runtime:    conv.runtime,
		History:    hist,
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

// ---------- 路由 ----------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /ui", s.handleUI)
	mux.HandleFunc("GET /ui/{$}", s.handleUI)
	// 前端静态资源（vendor / 页面模块）。路径不带内容指纹，靠 no-cache 校验。
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", ui.Handler()))
	// 根路径直接进控制台，省得记地址。
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	// 登录：用户名 + 密码换 admin key（面板首访引导用）。
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("GET /v1/models", s.handleListModels)
	mux.HandleFunc("GET /v1/usage", s.handleUsage)
	mux.HandleFunc("GET /v1/requests", s.handleRequests)
	mux.HandleFunc("POST /v1/responses", s.handleCreateResponse)
	mux.HandleFunc("GET /v1/responses/{id}", s.handleGetResponse)
	mux.HandleFunc("DELETE /v1/responses/{id}", s.handleDeleteResponse)
	// 用 {id...} 而不是 {id}：模型名形如 provider/model 含斜杠，
	// 单段 {id} 匹配不到，会掉进 /v1/ 的兜底 404。
	mux.HandleFunc("GET /v1/models/{id...}", s.handleGetModel)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/completions", s.handleLegacyCompletions)
	// Anthropic Messages API（供 Claude Code / Anthropic SDK）
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	// 兼容 DeepSeek 式约定：Anthropic 接口也挂在 /anthropic 前缀下
	// （ANTHROPIC_BASE_URL=http://host:2810/anthropic）
	mux.HandleFunc("POST /anthropic/v1/messages", s.handleMessages)
	mux.HandleFunc("POST /anthropic/v1/messages/count_tokens", s.handleCountTokens)
	// Anthropic 格式的模型列表（Claude Code 模型选择器 / models.list()）。
	mux.HandleFunc("GET /anthropic/v1/models", s.handleAnthropicModels)

	// 内置 MCP server：OpenCode 以 remote MCP 方式注册它，用来调用客户端声明的工具。
	// 既是路由，也当鉴权（URL 里的 token 是 128 位随机串）。
	mux.HandleFunc("/mcp/{token}", s.handleMCP)

	// 控制面：/api/v1/*（admin key）。所有资源都是数据库里的数据。
	s.registerAdminRoutes(mux)

	// 其余 /v1/* 一律返回 OpenAI 格式 404（客户端不会因解析失败而崩）
	mux.HandleFunc("/v1/", s.handleUnknownV1)

	// 顺序：request-id → CORS → 限流 → 访问日志 → 业务。
	// 限流放在 CORS 之后，OPTIONS 预检已在中间件内豁免。
	// 访问日志贴近 mux，能记录真实的业务状态码（含流式请求的总耗时）。
	return httpx.RequestID(newID, httpx.CORS(httpx.CORSOptions{
		AllowOrigin: s.cfg.CORSOrigin,
		IsPublic:    isPublicPath,
		DenyOrigin: func(w http.ResponseWriter, r *http.Request) {
			writeOpenAIError(w, http.StatusForbidden, "invalid_request_error",
				"origin not allowed by BRIDGE_CORS_ORIGIN", "cors_origin_denied")
		},
	}, httpx.RateLimit(httpx.RateLimitOptions{
		PerKey:     s.perKeyLimit,
		Global:     s.globalLimit,
		TrustProxy: s.cfg.TrustProxy,
		Log:        s.log,
		Metrics:    s.metrics,
		WriteError: writeOpenAIError,
	}, httpx.Metrics(s.metrics, httpx.AccessLog(s.log, s.cfg.TrustProxy, mux)))))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	info := map[string]any{
		"status": "ok",
		// 用实时端点：auto 模式会自动发现/热切换上游，cfg.Upstream 是启动时的旧值。
		"upstream": s.up.Base(),
	}
	if err := s.up.Ping(ctx); err != nil {
		info["status"] = "upstream_unreachable"
		info["error"] = logx.Redact(err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	// Anthropic 客户端（Claude Code / anthropic SDK）会带 anthropic-version 头，
	// 且只认 Anthropic 格式的模型列表（type/display_name/created_at + has_more…）。
	if r.Header.Get("anthropic-version") != "" {
		s.handleAnthropicModels(w, r)
		return
	}
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	if p, err := s.principal(r); err == nil && s.directMode(r, p) {
		s.listCatalogModels(w, p)
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

	out := ModelList{Object: "list", Data: make([]ModelObject, 0, len(list)+2)}

	// 虚拟模型：客户端下拉框里可直接选，不必写死具体模型名。
	//   janus   → web 里选的默认模型（见 /ui「支持的模型」→ 设为默认）
	//   default → 上游 / BRIDGE_DEFAULT_MODEL 的默认（旧语义）
	// 只有在没有任何过滤条件时才放，否则会污染过滤结果。
	if filter.Empty() {
		if ref, err := s.resolveJanusModel(r.Context(), list); err == nil {
			if def := modelx.Find(list, ref); def != nil {
				obj := modelx.ToOpenAIWithCredits(*def, s.cfg.OpenCodeContext)
				obj.ID = "janus"
				obj.OwnedBy = "janus"
				out.Data = append(out.Data, obj)
			}
		} else {
			s.log.Warnf("cannot resolve janus default model: %v", err)
		}
		if ref, err := s.resolveDefaultModel(r.Context(), list); err == nil {
			if def := modelx.Find(list, ref); def != nil {
				obj := modelx.ToOpenAIWithCredits(*def, s.cfg.OpenCodeContext)
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
			out.Data = append(out.Data, modelx.ToOpenAIWithCredits(m, s.cfg.OpenCodeContext))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
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

func (f ModelFilter) Match(m upstream.OCModel) bool {
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
	p, err := s.principal(r)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	id := r.PathValue("id")
	// 直连模式：目录在数据库里，不依赖 OpenCode 上游。
	if s.directMode(r, p) {
		var st *db.Store
		if s.router != nil {
			st = s.router.Store()
		}
		if st != nil {
			key := id
			if !strings.Contains(key, "/") {
				key = "" // 裸 id：按 upstream 找
			}
			var m db.Model
			var ok bool
			if key != "" {
				m, ok = st.GetModel(key)
			} else {
				m, ok = st.FindModelByUpstream("", id)
			}
			if ok {
				httpx.WriteJSON(w, http.StatusOK, catalogModelDetail(m))
				return
			}
		}
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("model %q not found", id), "model_not_found")
		return
	}
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
			detail := modelx.ToDetail(m)
			if s.cfg.OpenCodeContext > 0 {
				detail.ContextLength = s.cfg.OpenCodeContext
			}
			if strings.EqualFold(strings.TrimSpace(id), "default") {
				detail.ID = "default"
			}
			httpx.WriteJSON(w, http.StatusOK, detail)
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
	rec.Body = io.NopCloser(strings.NewReader(httpx.JSONString(chat)))
	rec.ContentLength = int64(len(httpx.JSONString(chat)))
	s.handleChatCompletions(w, rec)
}

// handleUsage 暴露上游用量（OpenCode console API）。
//
// 这是 janus 的扩展端点（OpenAI 没有对应接口）：OpenCode 的本地 server 只给
// 「已消耗」，余额得走 console API。客户端一般不会调它，主要给人/监控用。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.principal(r); err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "invalid API key", nil)
		return
	}
	if !s.cfg.UsageEnabled {
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
			"usage endpoint disabled (usage_enabled=false)", "disabled")
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
	httpx.WriteJSON(w, http.StatusOK, rep)
}

// handleRequests 暴露最近的上游请求日志（含每次的缓存命中率），供面板对账，
// 无需打开官方 console。扩展端点：GET /v1/requests?since=<ms>&limit=<n>
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	if !s.cfg.UsageEnabled {
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
			"requests endpoint disabled (BRIDGE_USAGE_ENABLED=false)", "disabled")
		return
	}
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	logs, err := s.usage.RequestLogs(ctx, since, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// 客户端主动断开导致的取消是正常现象，降为 Debug 免刷屏。
			s.log.Debugf("request logs canceled by client")
		} else {
			// console 的 /request-logs 经常超时/连不上；已做 60s 负缓存，
			// 这里降为 Debug，别再刷 WARN。
			s.log.Debugf("request logs failed: %v", err)
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "api_error",
			"cannot read request logs: "+err.Error(), "requests_unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"requests":   logs,
		"fetched_at": time.Now().UTC(),
	})
}

// uiPrefs 读持久化的 UI 偏好（无/坏值返回 null，由前端兜底）。
func (s *Server) uiPrefs() map[string]any {
	if s.db == nil {
		return nil
	}
	raw, ok := s.db.GetSetting("ui_prefs")
	if !ok || raw == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (s *Server) handleUnknownV1(w http.ResponseWriter, r *http.Request) {
	if err := s.checkAuth(r); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
		return
	}
	writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("unknown endpoint %q; implemented: /v1/models, /v1/models/{id}, /v1/chat/completions, /v1/completions, /v1/responses, /v1/messages, /v1/usage",
			r.URL.Path),
		"unknown_endpoint")
}

// ---------- 鉴权 ----------

// checkAuth 是旧调用点的兼容包装：走统一的 principal 解析。
//
// 未配置任何 key 且没有持久化层（纯内存/测试）时放行，便于本地起服务；
// 一旦有 DB，就必须带 api_keys 里的 key。
func (s *Server) checkAuth(r *http.Request) error {
	if _, err := s.principal(r); err != nil {
		return err
	}
	return nil
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
	// 控制台静态资源：页面与 JS/CSS 本身不含数据，跨源加载无风险。
	if strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/ui/") {
		return true
	}
	return false
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
