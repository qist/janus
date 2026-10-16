package main

// workbuddy_provider.go —— 把 WorkBuddy 内嵌成 janus 的一个 OpenAI 兼容 provider。
//
// 形态：janus 在自己的 mux 上挂一条带 token 的路由
//
//	GET  /workbuddy/{token}/v1/models
//	POST /workbuddy/{token}/v1/chat/completions
//
// 然后通过 OPENCODE_CONFIG_CONTENT 把这个 baseURL 注册成 OpenCode 的 provider
// （providerID=workbuddy）。于是 WorkBuddy 的模型在 janus 的 /v1/models 里以
// `workbuddy/<id>` 出现，会话、工具桥、权限、用量面板全部复用现有链路，无需改动。
//
// 为什么要有这一层而不是让 OpenCode 直连上游：
//   - 上游要求一整套桌面身份头（X-IDE-* / X-Conv* 等），OpenCode 配置表达不了；
//   - 上游拒绝非流式、拒绝 developer 角色、tool_choice 只认字符串；
//   - access token 会过期，需要在服务端用 refresh token 续期。
//
// 路由 token 与 /mcp/{token} 同理：URL 里的 128 位随机串就是凭据，随进程短命。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// workbuddyRuntime 是一次进程生命周期内的 WorkBuddy provider 运行时。
type workbuddyRuntime struct {
	cfg    *Config
	log    *Logger
	token  string
	host   string // 给 OpenCode 用的回环基址，如 http://127.0.0.1:2810
	client *workbuddyClient
	models []wbModel
	store  string // 缓存目录（凭据副本 / 模型目录）

	// 积分余额缓存：新鲜窗口内直接复用；失败后进入负缓存窗口（不重试，
	// 有旧数据回落旧数据），对齐用量接口的失败负缓存策略。
	creditsMu      sync.Mutex
	creditsCache   *wbCredits
	creditsFreshAt time.Time
	creditsFailAt  time.Time

	// reqLog 是 workbuddy 上游调用的内存环形记录（最近 100 条，面板展示）。
	reqMu  sync.Mutex
	reqLog []wbReqLog
}

// wbReqLogCap 是 workbuddy 最近请求的内存保留条数。
const wbReqLogCap = 100

// recordWBReq 追加一条请求记录（最新在前，超限截断）。
func (r *workbuddyRuntime) recordWBReq(e wbReqLog) {
	r.reqMu.Lock()
	defer r.reqMu.Unlock()
	r.reqLog = append([]wbReqLog{e}, r.reqLog...)
	if len(r.reqLog) > wbReqLogCap {
		r.reqLog = r.reqLog[:wbReqLogCap]
	}
}

// recentRequests 返回最近请求副本（无记录返回 nil）。
func (r *workbuddyRuntime) recentRequests() []wbReqLog {
	r.reqMu.Lock()
	defer r.reqMu.Unlock()
	if len(r.reqLog) == 0 {
		return nil
	}
	out := make([]wbReqLog, len(r.reqLog))
	copy(out, r.reqLog)
	return out
}

// 积分缓存窗口：新鲜 5 分钟；失败后 60 秒内不再打上游。
const (
	wbCreditsFreshTTL  = 5 * time.Minute
	wbCreditsRetryGate = 60 * time.Second
)

// creditsSnapshot 返回积分余额（带缓存）。没有可用数据时返回 nil——调用方
// 据此省略字段，绝不能因为积分挂了拖垮 /v1/usage 主报告。
func (r *workbuddyRuntime) creditsSnapshot(ctx context.Context) *wbCredits {
	r.creditsMu.Lock()
	defer r.creditsMu.Unlock()
	now := time.Now()
	if r.creditsCache != nil && now.Sub(r.creditsFreshAt) < wbCreditsFreshTTL {
		return r.creditsCache
	}
	if now.Sub(r.creditsFailAt) < wbCreditsRetryGate {
		return r.creditsCache
	}
	credits, err := r.client.fetchCredits(ctx)
	if err != nil {
		r.creditsFailAt = now
		r.log.Debugf("workbuddy: 积分查询失败: %v", err)
		return r.creditsCache
	}
	r.creditsCache = credits
	r.creditsFreshAt = now
	return credits
}

// wbCreditsFor 返回 workbuddy 模型的计费倍率（其他 provider 返回空串）。
func wbCreditsFor(providerID, modelID string) string {
	if wbRuntime == nil || providerID != "workbuddy" {
		return ""
	}
	for _, m := range wbRuntime.models {
		if m.ID == modelID {
			return m.Credits
		}
	}
	return ""
}

// wbCreditsIsFree 判断倍率是否为零倍率（免费）："x0.00" / "x0.0" 视为免费，
// 与插件的 free 判定（/^x?0\.0+$/）一致。
func wbCreditsIsFree(credits string) bool {
	s := strings.TrimPrefix(strings.TrimSpace(credits), "x")
	if s == "" {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

// wbContextOverride 返回 workbuddy 模型当前生效的上下文预算（读运行时设置，
// 设置变更立即反映到 /v1/models，不等 OpenCode 重注入）；非 workbuddy 返回 0。
func wbContextOverride(providerID, modelID string) int {
	if wbRuntime == nil || providerID != "workbuddy" {
		return 0
	}
	mode := "max"
	if wbRuntime.cfg != nil {
		mode = wbRuntime.cfg.WorkBuddyContext
	}
	for _, m := range wbRuntime.models {
		if m.ID == modelID {
			return m.wbMaxContext(mode)
		}
	}
	return 0
}

// toOpenAIWithCredits 把模型转成 OpenAI 条目；workbuddy 模型附带计费倍率，
// 且上下文长度用运行时档位覆盖（立即生效）。非 workbuddy 模型（opencode
// 渠道）可用 ocCtx 覆盖展示预算（0 = 跟随目录）。
// 零倍率同时打上免费标记（workbuddy 无 OpenCode cost 数据，isFreeModel 判不出）。
func toOpenAIWithCredits(m OCModel, ocCtx int) ModelObject {
	obj := ToOpenAI(m)
	if c := wbCreditsFor(m.ProviderID, m.ID); c != "" {
		obj.WorkBuddyCredits = c
		if wbCreditsIsFree(c) {
			obj.Free = true
		}
	}
	if ctx := wbContextOverride(m.ProviderID, m.ID); ctx > 0 {
		obj.ContextLength = ctx
	} else if ocCtx > 0 && m.ProviderID != "workbuddy" {
		obj.ContextLength = ocCtx
	}
	return obj
}

// wbRuntime 是全局运行时（nil = 未启用）。
//
// 生命周期：main.go 在 EnsureUpstream **之前**调用 initWorkBuddy（OpenCode 的
// 内联配置在拉起子进程时生成，必须早于那一刻）；Handler() 挂路由、
// opencodeInlineConfig 注入 provider 都读它。初始化失败只降级（Warn + nil），
// 不影响 janus 主链路。
var wbRuntime *workbuddyRuntime

// initWorkBuddy 按 cfg 初始化 WorkBuddy provider 运行时（含模型目录）。
// 未启用或初始化失败时 wbRuntime 保持 nil（Warn 说明原因）。
func initWorkBuddy(cfg *Config, log *Logger) {
	if !cfg.WorkBuddyEnabled {
		return
	}
	rt, err := newWorkBuddyRuntime(cfg, log)
	if err != nil {
		log.Warnf("workbuddy provider disabled: %v", err)
		return
	}
	wbRuntime = rt
}

// wbCatalogTTL 是模型目录缓存的“新鲜”窗口；过期只是会尝试刷新，刷新失败仍用旧的。
const wbCatalogTTL = 12 * time.Hour

// newWorkBuddyRuntime 加载凭据、准备模型目录并返回运行时。
// 任何一步失败都返回 error，由调用方决定是禁用还是退出。
func newWorkBuddyRuntime(cfg *Config, log *Logger) (*workbuddyRuntime, error) {
	store := ""
	if d := janusDataDir(); d != "" {
		store = filepath.Join(d, "workbuddy")
	}

	cred, err := loadWBCredential(cfg.WorkBuddyAuthFile, store)
	if err != nil {
		return nil, err
	}

	client := newWorkBuddyClient(cred, cfg.WorkBuddyAppVersion)
	rt := &workbuddyRuntime{
		cfg:    cfg,
		log:    log,
		token:  wbRandomToken(),
		host:   wbLoopbackBase(cfg.Addr),
		client: client,
		store:  store,
	}
	// 续期成功后把新凭据落盘，避免每次重启都重刷一遍。
	client.onRefresh = func(c *wbCredential) { rt.persistCredential(c) }

	// 模型目录：先看缓存，再同步刷新。必须同步——variants（思考强度档位）
	// 是在拉起 opencode 时随 OPENCODE_CONFIG_CONTENT 注入的，刷新晚了
	// 这一轮进程就带不上真实档位（缓存旧形态会全部退化成无档）。
	// 刷新失败仍有缓存兜底；两者皆无才禁用 provider。
	rt.loadCatalog()
	if err := rt.refreshCatalog(context.Background()); err != nil && len(rt.models) == 0 {
		return nil, fmt.Errorf("拿不到 WorkBuddy 模型目录: %w", err)
	}

	log.Infof("workbuddy provider enabled: region=%s base=%s models=%d",
		cred.region(), rt.baseURL(), len(rt.models))
	return rt, nil
}

// baseURL 是注册给 OpenCode 的 baseURL（含 token，指向 janus 自己的 mux）。
func (r *workbuddyRuntime) baseURL() string {
	return r.host + "/workbuddy/" + r.token + "/v1"
}

// modelConfigs 生成 OpenCode provider 配置里的 models 映射。
//
// variants 语义（180 上 opencode v2.0.27 实测）：
//   - 声明了档位的推理模型 → 注入 {effort: {reasoningEffort: effort}}，
//     OpenCode 的模型列表即只提供这些档位（请求构造时映射为线上的
//     reasoning_effort），替换掉它自作主张的通用档 low/medium/high；
//   - 未声明档位的推理模型 → 不注入，保持 OpenCode 通用默认；
//   - 非推理模型 → 注入空 variants 压掉自动档（选不到就不会发出上游
//     必 400 的 effort）。
//   - off 档只在 canDisableThinking 为真时提供（CN 端 onlyReasoning 模型
//     线上拒 off，同样不给）。
func (r *workbuddyRuntime) modelConfigs() map[string]any {
	out := make(map[string]any, len(r.models))
	mode := ""
	if r.cfg != nil {
		mode = r.cfg.WorkBuddyContext
	}
	for _, m := range r.models {
		entry := map[string]any{"name": m.Name}
		limit := map[string]any{}
		if ctx := m.wbMaxContext(mode); ctx > 0 {
			limit["context"] = ctx
		}
		if m.MaxOutputToken > 0 {
			limit["output"] = m.MaxOutputToken
		}
		if len(limit) > 0 {
			entry["limit"] = limit
		}
		if m.SupportsImages {
			entry["attachment"] = true
		}
		variants := map[string]any{}
		switch {
		case m.Reasoning && len(m.SupportedEfforts) > 0:
			for _, e := range m.SupportedEfforts {
				variants[e] = map[string]any{"reasoningEffort": e}
			}
			if m.CanDisableThinking && !(m.OnlyReasoning && r.client.credential().region() == "cn") {
				variants["off"] = map[string]any{"reasoningEffort": "off"}
			}
			entry["variants"] = variants
		case m.Reasoning:
			// 未声明档位：保持 OpenCode 通用默认档。
		default:
			entry["variants"] = variants // 空 map：压掉自动档
		}
		out[m.ID] = entry
	}
	return out
}

// ---------- 路由 ----------

// registerRoutes 把 provider 端点挂到 mux 上（带 token 的路径即鉴权）。
func (r *workbuddyRuntime) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /workbuddy/{token}/v1/models", r.handleModels)
	mux.HandleFunc("POST /workbuddy/{token}/v1/chat/completions", r.handleChat)
}

// tokenOK 常时比较 URL 里的 token。
func (r *workbuddyRuntime) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(r.token)) == 1
}

func (r *workbuddyRuntime) handleModels(w http.ResponseWriter, req *http.Request) {
	if !r.tokenOK(req.PathValue("token")) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "invalid workbuddy token", "type": "invalid_request_error"},
		})
		return
	}
	data := make([]map[string]any, 0, len(r.models))
	for _, m := range r.models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": "workbuddy",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (r *workbuddyRuntime) handleChat(w http.ResponseWriter, req *http.Request) {
	if !r.tokenOK(req.PathValue("token")) {
		r.writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid workbuddy token")
		return
	}
	limit := r.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil {
		r.writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}
	if int64(len(raw)) > limit {
		r.writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "请求体过大")
		return
	}
	body := wbPrepareBody(raw)

	// 请求记录：console 的 request-logs 不统计 workbuddy 渠道，这里自记
	// （模型名取自原始 body；耗时到上游响应为止，SSE 流式阶段不计）。
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &probe)
	start := time.Now()
	status := http.StatusOK
	defer func() {
		r.recordWBReq(wbReqLog{
			Time:   time.Now().Format("15:04:05"),
			Model:  probe.Model,
			Status: status,
			DurMs:  time.Since(start).Milliseconds(),
		})
	}()

	resp, err := r.client.chat(req.Context(), body)
	if err != nil {
		status = http.StatusBadGateway
		r.writeOpenAIError(w, http.StatusBadGateway, "api_error", "workbuddy: "+Redact(err.Error()))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, wbErrorBodyLimit))
		kind := wbClassify(resp.StatusCode, string(head))
		msg := wbUpstreamMessage(string(head))
		if msg == "" {
			msg = shrink(head, 300)
		}
		status = wbStatusFor(kind)
		r.writeOpenAIError(w, status, string(kind), "workbuddy 上游: "+msg)
		return
	}

	// 上游是 SSE；原样透传，按块 flush。
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/event-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	streamCopy(w, resp.Body)
}

// writeOpenAIError 输出 OpenAI 风格错误体。
func (r *workbuddyRuntime) writeOpenAIError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
}

// streamCopy 把上游响应体按块写回客户端并 flush；结束或被中断即返回。
func streamCopy(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// ---------- 凭据与目录的读取/落盘 ----------

// loadWBCredential 读取明文凭据。优先配置的 auth 文件，失败时回落到上次续期落盘的副本。
func loadWBCredential(path, store string) (*wbCredential, error) {
	var firstErr error
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			firstErr = fmt.Errorf("读取 WORKBUDDY_AUTH_FILE 失败: %w", err)
		} else if cred, perr := parseWBCredential(string(raw)); perr != nil {
			firstErr = perr
		} else {
			return cred, nil
		}
	} else {
		firstErr = errors.New("未配置 WORKBUDDY_AUTH_FILE")
	}
	if store != "" {
		if raw, err := os.ReadFile(filepath.Join(store, "credential.json")); err == nil {
			if cred, perr := parseWBCredential(string(raw)); perr == nil {
				return cred, nil
			}
		}
	}
	return nil, firstErr
}

// persistCredential 把续期后的凭据写回缓存目录（0600）。失败只记日志。
func (r *workbuddyRuntime) persistCredential(cred *wbCredential) {
	if r.store == "" {
		return
	}
	if err := os.MkdirAll(r.store, 0o700); err != nil {
		r.log.Warnf("workbuddy: 建缓存目录失败: %v", err)
		return
	}
	b, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(r.store, "credential.json")
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		r.log.Warnf("workbuddy: 落盘凭据失败: %v", err)
	}
}

func (r *workbuddyRuntime) catalogPath() string {
	if r.store == "" {
		return ""
	}
	return filepath.Join(r.store, fmt.Sprintf("catalog-%s.json", r.client.credential().region()))
}

type wbCatalogCache struct {
	FetchedAtMs int64     `json:"fetchedAtMs"`
	Models      []wbModel `json:"models"`
}

// loadCatalog 读本地缓存目录（不校验新鲜度：刷新失败时它就是兜底）。
func (r *workbuddyRuntime) loadCatalog() {
	p := r.catalogPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var c wbCatalogCache
	if err := json.Unmarshal(raw, &c); err != nil || len(c.Models) == 0 {
		return
	}
	r.models = c.Models
}

// refreshCatalog 拉取目录并落盘。失败时保留现有（可能来自缓存的）列表。
func (r *workbuddyRuntime) refreshCatalog(ctx context.Context) error {
	models, err := r.client.fetchCatalog(ctx)
	if err != nil {
		if len(r.models) > 0 {
			r.log.Warnf("workbuddy: 目录刷新失败，沿用缓存 %d 个模型: %v", len(r.models), err)
			return nil
		}
		return err
	}
	r.models = models
	p := r.catalogPath()
	if p != "" {
		if err := os.MkdirAll(r.store, 0o700); err == nil {
			if b, merr := json.Marshal(wbCatalogCache{FetchedAtMs: time.Now().UnixMilli(), Models: models}); merr == nil {
				_ = os.WriteFile(p, append(b, '\n'), 0o600)
			}
		}
	}
	return nil
}

// ---------- 小工具 ----------

// wbRandomToken 生成 128 位 URL-safe 随机 token（与 /mcp/{token} 同量级）。
func wbRandomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// wbLoopbackBase 把监听地址转成 OpenCode 可访问的回环基址。
// 0.0.0.0 / 空 / [::] 一律映射到 127.0.0.1；指定具体 IP 时原样用。
func wbLoopbackBase(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1" + ensureColonPort(addr)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func ensureColonPort(s string) string {
	if s == "" {
		return ":2810"
	}
	if !strings.HasPrefix(s, ":") {
		return ":" + s
	}
	return s
}
