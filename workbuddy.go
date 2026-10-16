package main

// workbuddy.go —— WorkBuddy（腾讯 CodeBuddy / workbuddy.ai）上游客户端。
//
// 目标：把 WorkBuddy 桌面 App 的模型接入本桥，使其在客户端 /v1/models 里以
// providerID=workbuddy 出现（模型引用形如 workbuddy/<id>）。
//
// 上游本身就是 OpenAI chat-completions 兼容端点，所以本桥只做四件事：
//
//	1. 桌面身份头（X-IDE-* / X-User-Id / X-Conv* 等，缺一个都可能被渠道校验拦掉）；
//	2. body 规整（强制 stream、developer→system、tool_choice 拍平、去掉 off 档）；
//	3. access token 过期时用 refresh token 续期；
//	4. 模型目录（GET /v3/config）。
//
// 协议规格参考 corrinehu/dsh-workbuddy-connect（MIT）；它的 wire 行为又移植自
// Sliverkiss/workbuddy2api。
//
// ⚠️ 凭据：WorkBuddy 5.6 的加密桌面凭据依赖其自带 Electron 二进制的私有
// workbuddyStorage 绑定解密，Linux 上没有该二进制，无法解开。因此本桥只接受
// **明文**凭据文件（形态见 parseWBCredential）。明文凭据里的 refreshToken 可用于
// 无限续期，拿到一次即可长期使用。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// wbGlobalBase 是国际版（WorkBuddy AI）的 chat / 目录 / 计费同域基址。
	wbGlobalBase = "https://www.workbuddy.ai"
	// wbCnChatBase 是国内版 chat 基址；目录/计费另有 codebuddy.cn。
	wbCnChatBase = "https://copilot.tencent.com"
	// wbCnBillBase 是国内版目录/计费的 Origin/Referer 与计费基址。
	wbCnBillBase = "https://www.codebuddy.cn"

	// wbFallbackAppVer 是拿不到真实桌面版本时的兜底版本号（与上游插件一致）。
	wbFallbackAppVer = "5.5.6"

	// wbClientUA 是 CLI 形态的 UA，用于 refresh 与国内版目录。
	wbClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// wbJSONTimeout 是 refresh / 目录这类短请求的超时。
	wbJSONTimeout = 30 * time.Second

	// wbErrorBodyLimit 是错误响应体截断长度（只用于分类与回显）。
	wbErrorBodyLimit = 4096
)

// wbCredential 是 WorkBuddy 明文凭据。字段名与插件自留副本一致，便于直接互导。
type wbCredential struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	ExpiresAtMs  int64  `json:"expiresAtMs,omitempty"`
}

// wbRegion 由登录 domain 推导：workbuddy.ai 系 = 国际版，其余（含空）= 国内版。
func wbRegionOf(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

func (c *wbCredential) region() string { return wbRegionOf(c.Domain) }

// wbChatBase chat 端点基址。
func (c *wbCredential) chatBase() string {
	if c.region() == "global" {
		return wbGlobalBase
	}
	return wbCnChatBase
}

// wbOrigin 目录/refresh 用的 Origin / Referer 基址。
func (c *wbCredential) origin() string {
	if c.region() == "global" {
		return wbGlobalBase
	}
	return wbCnBillBase
}

// ---------- 凭据解析 ----------

// parseWBCredential 容忍两种明文形态：
//
//  1. 插件自留副本（扁平）：{accessToken, refreshToken, domain, uid, enterpriseId}
//  2. 桌面明文文件（嵌套）：{auth:{accessToken,refreshToken,domain}, identity:{uid,enterpriseId}}
//
// 只有 accessToken 是必需的；其余缺失时按“尽量可用”处理。加密的桌面文件（5.6 信封）
// 里 accessToken 是个密文对象、解析不出字符串，这里会明确报错并提示先导出明文。
func parseWBCredential(text string) (*wbCredential, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("凭据文件不是合法 JSON: %w", err)
	}
	cred := &wbCredential{
		AccessToken:  wbStr(doc["accessToken"]),
		RefreshToken: wbStr(doc["refreshToken"]),
		Domain:       wbStr(doc["domain"]),
		UID:          wbStr(doc["uid"]),
		EnterpriseID: wbStr(doc["enterpriseId"]),
	}
	if auth, ok := doc["auth"].(map[string]any); ok {
		if cred.AccessToken == "" {
			cred.AccessToken = wbStr(auth["accessToken"])
		}
		if cred.RefreshToken == "" {
			cred.RefreshToken = wbStr(auth["refreshToken"])
		}
		if cred.Domain == "" {
			cred.Domain = wbStr(auth["domain"])
		}
	}
	if ident, ok := doc["identity"].(map[string]any); ok {
		if cred.UID == "" {
			cred.UID = wbStr(ident["uid"])
		}
		if cred.EnterpriseID == "" {
			cred.EnterpriseID = wbStr(ident["enterpriseId"])
		}
	}
	if cred.AccessToken == "" {
		return nil, errors.New("凭据里没有可用的 accessToken；若这是 WorkBuddy 5.6 加密文件，" +
			"需先在有桌面版的机器上导出明文（见 docs/WORKBUDDY.md）")
	}
	return cred, nil
}

func wbStr(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// wbNum 宽容地取数值：encoding/json 解出来是 float64，但测试字面量/部分
// 上游变体会直接给 int，统一在这里收口。
func wbNum(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// ---------- 请求头 ----------

// wbRandomID 生成一个 UUIDv4 字符串（X-Conversation-* 三件套需要）。
func wbRandomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退化为时间戳，保证非空即可（这三件套只用来标记渠道，不承载状态）。
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// wbCommonHeaders 是 refresh / 目录请求共享的头（chat 会覆盖 UA）。
func wbCommonHeaders(cred *wbCredential) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Origin", cred.origin())
	h.Set("Referer", cred.origin()+"/")
	h.Set("User-Agent", wbClientUA)
	return h
}

// wbChatHeaders 组装 chat 请求头。international 时 UA 形如
// `WorkBuddy/<v> WorkBuddy AI/<v>`，国内版为 `WorkBuddy/<v> WorkBuddy/<v>`。
func wbChatHeaders(cred *wbCredential, appVer string) http.Header {
	h := wbCommonHeaders(cred)
	product := "WorkBuddy"
	if cred.region() == "global" {
		product = "WorkBuddy AI"
	}
	h.Set("User-Agent", fmt.Sprintf("WorkBuddy/%s %s/%s", appVer, product, appVer))
	h.Set("Content-Type", "application/json")
	if cred.UID == "" {
		h.Set("X-No-User-Id", "1")
	} else {
		h.Set("X-User-Id", cred.UID)
	}
	if cred.EnterpriseID == "" {
		h.Set("X-No-Enterprise-Id", "1")
	} else {
		h.Set("X-Enterprise-Id", cred.EnterpriseID)
	}
	if cred.Domain == "" {
		h.Set("X-No-Department-Info", "1")
	} else {
		h.Set("X-Domain", cred.Domain)
	}
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Version", appVer)
	h.Set("X-Product", "SaaS")
	// 2026-10-16 起国际版缺这三个头直接 400，国内版一并带上（实测两者都接受）。
	h.Set("X-Conversation-ID", wbRandomID())
	h.Set("X-Conversation-Request-ID", wbRandomID())
	h.Set("X-Conversation-Message-ID", wbRandomID())
	return h
}

// wbCatalogHeaders 组装模型目录请求头（app-shaped）。
func wbCatalogHeaders(cred *wbCredential, appVer string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	h.Set("Accept", "application/json")
	h.Set("Origin", cred.origin())
	h.Set("Referer", cred.origin()+"/")
	if cred.region() == "global" {
		h.Set("X-Requested-With", "XMLHttpRequest")
		h.Set("X-Product", "SaaS")
		h.Set("User-Agent", "WorkBuddyAI/"+appVer)
		return h
	}
	h.Set("User-Agent", wbClientUA)
	// 企业账号不给这对身份头会被静默回答个人版目录（issue #123），且必须成对出现。
	if cred.UID != "" && cred.EnterpriseID != "" {
		h.Set("X-User-Id", cred.UID)
		h.Set("X-Enterprise-Id", cred.EnterpriseID)
		h.Set("X-Tenant-Id", cred.EnterpriseID)
		if cred.Domain != "" {
			h.Set("X-Domain", cred.Domain)
		}
	}
	return h
}

// ---------- body 规整 ----------

// wbPrepareBody 把 OpenAI chat-completions 请求体规整成上游能接受的形态：
// 强制 stream=true、developer→system、tool_choice 拍平成字符串、去掉 off 档。
// 非 JSON / 非对象体原样返回（上游自己会拒，不在这里吞错）。
func wbPrepareBody(src []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil || obj == nil {
		return src
	}
	obj["stream"] = true
	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "developer" {
				mm["role"] = "system"
			}
		}
	}
	wbFlattenToolChoice(obj)
	if obj["reasoning_effort"] == "off" {
		delete(obj, "reasoning_effort")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// wbFlattenToolChoice 把对象形态的 tool_choice 拍平为上游接受的字符串形态。
func wbFlattenToolChoice(obj map[string]any) {
	choice, present := obj["tool_choice"]
	if !present {
		return
	}
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	if s, ok := choice.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
		return
	}
	m, ok := choice.(map[string]any)
	if !ok {
		delete(obj, "tool_choice")
		return
	}
	switch t, _ := m["type"].(string); strings.ToLower(strings.TrimSpace(t)) {
	case "none":
		delete(obj, "tool_choice")
		suppress()
	case "auto", "required":
		obj["tool_choice"] = strings.ToLower(strings.TrimSpace(t))
	case "function":
		name := ""
		if fn, ok := m["function"].(map[string]any); ok {
			name = wbStr(fn["name"])
		}
		if name == "" {
			name = wbStr(m["name"])
		}
		if name == "" {
			obj["tool_choice"] = "auto"
		} else {
			obj["tool_choice"] = name
		}
	default:
		delete(obj, "tool_choice")
	}
}

// ---------- 错误分类 ----------

// wbErrKind 是上游失败类别，映射到给客户端的不同 HTTP 状态。
type wbErrKind string

const (
	wbKindHardCredit  wbErrKind = "hard_credit"
	wbKindSoftRate    wbErrKind = "soft_rate"
	wbKindSessionDead wbErrKind = "session_dead"
	wbKindNotFound    wbErrKind = "not_found"
	wbKindServer      wbErrKind = "server"
	wbKindClient      wbErrKind = "client"
)

var (
	wbHardCreditMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "credit not enough", "not enough credit",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	}
	wbSessionDeadMarkers = []string{"Offline user session not found", "12153"}
)

// wbClassify 从 HTTP 状态与响应体摘要判断失败类别。
func wbClassify(status int, body string) wbErrKind {
	if status == 402 {
		return wbKindHardCredit
	}
	if status == 401 {
		return wbKindSessionDead
	}
	lower := strings.ToLower(body)
	for _, m := range wbHardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return wbKindHardCredit
		}
	}
	for _, m := range wbSessionDeadMarkers {
		if strings.Contains(body, m) {
			return wbKindSessionDead
		}
	}
	switch {
	case status == 429:
		return wbKindSoftRate
	case status == 404:
		return wbKindNotFound
	case status >= 500:
		return wbKindServer
	default:
		return wbKindClient
	}
}

// wbStatusFor 把失败类别映射为返回给客户端的 HTTP 状态。
func wbStatusFor(kind wbErrKind) int {
	switch kind {
	case wbKindHardCredit:
		return http.StatusPaymentRequired
	case wbKindSoftRate:
		return http.StatusTooManyRequests
	case wbKindSessionDead:
		return http.StatusUnauthorized
	case wbKindNotFound, wbKindServer:
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}

// wbUpstreamMessage 从上游 JSON 体里取可读错误信息（优先 displayMsg.zh/en，再 msg）。
func wbUpstreamMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "{") {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return ""
	}
	if dm, ok := obj["displayMsg"].(map[string]any); ok {
		if s := wbStr(dm["zh"]); s != "" {
			return s
		}
		if s := wbStr(dm["en"]); s != "" {
			return s
		}
	}
	return wbStr(obj["msg"])
}

// ---------- 目录模型 ----------

// wbModel 是目录里一个可用模型。
type wbModel struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ContextWindow  int    `json:"contextWindow,omitempty"`
	MaxOutputToken int    `json:"maxOutputTokens,omitempty"`
	SupportsImages bool   `json:"supportsImages,omitempty"`
}

// wbParseCatalog 解析 /v3/config 文档（兼容裸文档与 {code,msg,data} 信封两种形态）。
// 目录的权威顺序取自 agents 里 name=="cli" 的 models 列表，再按 id 从 models 行补齐。
func wbParseCatalog(doc map[string]any) ([]wbModel, error) {
	data := doc
	if d, ok := doc["data"].(map[string]any); ok {
		if _, hasModels := d["models"]; hasModels {
			data = d
		} else if _, hasAgents := d["agents"]; hasAgents {
			data = d
		}
	}
	rows, _ := data["models"].([]any)
	agents, _ := data["agents"].([]any)
	var cliIDs []string
	for _, a := range agents {
		am, ok := a.(map[string]any)
		if !ok || wbStr(am["name"]) != "cli" {
			continue
		}
		if ids, ok := am["models"].([]any); ok {
			for _, id := range ids {
				if s := wbStr(id); s != "" {
					cliIDs = append(cliIDs, s)
				}
			}
		}
		break
	}
	if len(cliIDs) == 0 {
		return nil, errors.New("目录里没有 cli agent 的模型列表")
	}
	byID := map[string]wbModel{}
	for _, r := range rows {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id := wbStr(rm["id"])
		if id == "" || rm["disabled"] == true {
			continue
		}
		m := wbModel{ID: id, Name: firstNonEmpty(wbStr(rm["name"]), id)}
		if n := wbNum(rm["maxInputTokens"]); n > 0 {
			m.ContextWindow = n
		}
		if n := wbNum(rm["maxOutputTokens"]); n > 0 {
			m.MaxOutputToken = n
		}
		if cw, ok := rm["contextWindow"].(map[string]any); ok {
			if n := wbNum(cw["defaultLength"]); n > 0 {
				m.ContextWindow = n
			}
		}
		if rm["supportsImages"] == true && rm["disabledMultimodal"] != true {
			m.SupportsImages = true
		}
		byID[id] = m
	}
	var out []wbModel
	for _, id := range cliIDs {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("目录解析后模型列表为空")
	}
	return out, nil
}

// ---------- 客户端 ----------

// workbuddyClient 是 WorkBuddy 上游客户端。凭据可热更新（refresh 后）。
type workbuddyClient struct {
	http   *http.Client
	appVer string

	mu        sync.RWMutex
	cred      *wbCredential
	refreshMu sync.Mutex

	// onRefresh 在续期成功后回调（用于把新凭据落盘，可选）。
	onRefresh func(*wbCredential)
	// baseOverride 供测试改写基址（为空则按凭据 region 选）。
	baseOverride string
}

func newWorkBuddyClient(cred *wbCredential, appVer string) *workbuddyClient {
	if strings.TrimSpace(appVer) == "" {
		appVer = wbFallbackAppVer
	}
	return &workbuddyClient{
		// 不设 Timeout：chat 是长连接 SSE，超时靠 context 控制。
		http:   &http.Client{},
		appVer: appVer,
		cred:   cred,
	}
}

func (c *workbuddyClient) credential() *wbCredential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cred
}

func (c *workbuddyClient) setCredential(cred *wbCredential) {
	c.mu.Lock()
	c.cred = cred
	c.mu.Unlock()
}

func (c *workbuddyClient) chatBase() string {
	if c.baseOverride != "" {
		return c.baseOverride
	}
	return c.credential().chatBase()
}

// chat 发起一次 chat 请求。成功时返回原始 SSE 响应体（未读取）。
// 遇 401 会自动续期并重试一次。
func (c *workbuddyClient) chat(ctx context.Context, body []byte) (*http.Response, error) {
	resp, err := c.chatOnce(ctx, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	// 401：可能是 access token 过期。吞掉旧响应体后尝试续期。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, wbErrorBodyLimit))
	_ = resp.Body.Close()
	if rerr := c.refresh(ctx); rerr != nil {
		return nil, fmt.Errorf("上游 401 且续期失败: %w", rerr)
	}
	return c.chatOnce(ctx, body)
}

func (c *workbuddyClient) chatOnce(ctx context.Context, body []byte) (*http.Response, error) {
	cred := c.credential()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase()+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = wbChatHeaders(cred, c.appVer)
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	return c.http.Do(req)
}

// refresh 用 refresh token 换新的 access token。单飞，避免并发重复续期。
func (c *workbuddyClient) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	cred := c.credential()
	if cred.RefreshToken == "" {
		return errors.New("凭据里没有 refreshToken，无法续期；请重新导出明文凭据")
	}
	rctx, cancel := context.WithTimeout(ctx, wbJSONTimeout)
	defer cancel()

	base := cred.chatBase()
	if c.baseOverride != "" {
		base = c.baseOverride
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, base+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	req.Header = wbCommonHeaders(cred)
	req.Header.Set("X-Refresh-Token", cred.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("续期失败 http %d: %s", resp.StatusCode, shrink(raw, 200))
	}
	var envelope struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("续期响应不是 JSON: %s", shrink(raw, 200))
	}
	if envelope.Code != 0 {
		return fmt.Errorf("续期被拒 code=%d: %s", envelope.Code, envelope.Msg)
	}
	access := wbStr(envelope.Data["accessToken"])
	if access == "" {
		return errors.New("续期响应没有 accessToken；请在 WorkBuddy App 重新登录后再导出凭据")
	}
	next := *cred
	next.AccessToken = access
	if v := wbStr(envelope.Data["refreshToken"]); v != "" {
		next.RefreshToken = v
	}
	if v := wbStr(envelope.Data["domain"]); v != "" {
		next.Domain = v
	}
	if n, ok := envelope.Data["expiresIn"].(float64); ok && n > 0 {
		next.ExpiresAtMs = time.Now().Add(time.Duration(n) * time.Second).UnixMilli()
	}
	c.setCredential(&next)
	if c.onRefresh != nil {
		c.onRefresh(&next)
	}
	return nil
}

// fetchCatalog 拉取模型目录。
func (c *workbuddyClient) fetchCatalog(ctx context.Context) ([]wbModel, error) {
	cred := c.credential()
	base := cred.chatBase()
	if c.baseOverride != "" {
		base = c.baseOverride
	}
	rctx, cancel := context.WithTimeout(ctx, wbJSONTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header = wbCatalogHeaders(cred, c.appVer)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("目录请求 http %d: %s", resp.StatusCode, shrink(raw, 200))
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("目录响应不是 JSON: %s", shrink(raw, 200))
	}
	if code, ok := doc["code"].(float64); ok && int(code) != 0 {
		return nil, fmt.Errorf("目录被拒 code=%d: %s", int(code), wbStr(doc["msg"]))
	}
	return wbParseCatalog(doc)
}

// shrink 截断字节串用于日志/错误回显。
func shrink(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
