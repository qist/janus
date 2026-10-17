// workbuddy.go —— WorkBuddy（腾讯 CodeBuddy / workbuddy.ai）上游客户端。
//
// 目标：把 WorkBuddy 桌面 App 的模型接入本桥，使其在客户端 /v1/models 里以
// providerID=workbuddy 出现（模型引用形如 workbuddy/<id>）。
//
// 上游本身就是 OpenAI chat-completions 兼容端点，所以本桥只做四件事：
//
//  1. 桌面身份头（X-IDE-* / X-User-Id / X-Conv* 等，缺一个都可能被渠道校验拦掉）；
//  2. body 规整（强制 stream、developer→system、tool_choice 拍平、去掉 off 档）；
//  3. access token 过期时用 refresh token 续期；
//  4. 模型目录（GET /v3/config）。
//
// 协议规格参考 corrinehu/dsh-workbuddy-connect（MIT）；它的 wire 行为又移植自
// Sliverkiss/workbuddy2api。
//
// ⚠️ 凭据：WorkBuddy 5.6 的加密桌面凭据依赖其自带 Electron 二进制的私有
// workbuddyStorage 绑定解密，Linux 上没有该二进制，无法解开。因此本桥只接受
// **明文**凭据文件（形态见 parseCredential）。明文凭据里的 refreshToken 可用于
// 无限续期，拿到一次即可长期使用。
package workbuddy

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
	// globalBase 是国际版（WorkBuddy AI）的 chat / 目录 / 计费同域基址。
	globalBase = "https://www.workbuddy.ai"
	// cnChatBase 是国内版 chat 基址；目录/计费另有 codebuddy.cn。
	cnChatBase = "https://copilot.tencent.com"
	// cnBillBase 是国内版目录/计费的 Origin/Referer 与计费基址。
	cnBillBase = "https://www.codebuddy.cn"

	// fallbackAppVer 是拿不到真实桌面版本时的兜底版本号（与上游插件一致）。
	fallbackAppVer = "5.5.6"

	// clientUA 是 CLI 形态的 UA，用于 refresh 与国内版目录。
	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// jsonTimeout 是 refresh / 目录这类短请求的超时。
	jsonTimeout = 30 * time.Second

	// errorBodyLimit 是错误响应体截断长度（只用于分类与回显）。
	errorBodyLimit = 4096
)

// Credential 是 WorkBuddy 明文凭据。字段名与插件自留副本一致，便于直接互导。
type Credential struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	ExpiresAtMs  int64  `json:"expiresAtMs,omitempty"`
}

// regionOf 由登录 domain 推导：workbuddy.ai 系 = 国际版，其余（含空）= 国内版。
func regionOf(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

func (c *Credential) region() string { return regionOf(c.Domain) }

// chatBase chat 端点基址。
func (c *Credential) chatBase() string {
	if c.region() == "global" {
		return globalBase
	}
	return cnChatBase
}

// origin 目录/refresh 用的 Origin / Referer 基址。
func (c *Credential) origin() string {
	if c.region() == "global" {
		return globalBase
	}
	return cnBillBase
}

// ---------- 凭据解析 ----------

// parseCredential 容忍两种明文形态：
//
//  1. 插件自留副本（扁平）：{accessToken, refreshToken, domain, uid, enterpriseId}
//  2. 桌面明文文件（嵌套）：{auth:{accessToken,refreshToken,domain}, identity:{uid,enterpriseId}}
//
// 只有 accessToken 是必需的；其余缺失时按“尽量可用”处理。加密的桌面文件（5.6 信封）
// 里 accessToken 是个密文对象、解析不出字符串，这里会明确报错并提示先导出明文。
func parseCredential(text string) (*Credential, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("凭据文件不是合法 JSON: %w", err)
	}
	cred := &Credential{
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

// randomID 生成一个 UUIDv4 字符串（X-Conversation-* 三件套需要）。
func randomID() string {
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

// commonHeaders 是 refresh / 目录请求共享的头（chat 会覆盖 UA）。
func commonHeaders(cred *Credential) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Origin", cred.origin())
	h.Set("Referer", cred.origin()+"/")
	h.Set("User-Agent", clientUA)
	return h
}

// chatHeaders 组装 chat 请求头。international 时 UA 形如
// `WorkBuddy/<v> WorkBuddy AI/<v>`，国内版为 `WorkBuddy/<v> WorkBuddy/<v>`。
func chatHeaders(cred *Credential, appVer string) http.Header {
	h := commonHeaders(cred)
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
	h.Set("X-Conversation-ID", randomID())
	h.Set("X-Conversation-Request-ID", randomID())
	h.Set("X-Conversation-Message-ID", randomID())
	return h
}

// catalogHeaders 组装模型目录请求头（app-shaped）。
func catalogHeaders(cred *Credential, appVer string) http.Header {
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
	h.Set("User-Agent", clientUA)
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

// prepareBody 把 OpenAI chat-completions 请求体规整成上游能接受的形态：
// 强制 stream=true、developer→system、tool_choice 拍平成字符串、去掉 off 档。
// 非 JSON / 非对象体原样返回（上游自己会拒，不在这里吞错）。
func prepareBody(src []byte) []byte {
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
	flattenToolChoice(obj)
	if obj["reasoning_effort"] == "off" {
		delete(obj, "reasoning_effort")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// flattenToolChoice 把对象形态的 tool_choice 拍平为上游接受的字符串形态。
func flattenToolChoice(obj map[string]any) {
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

// errKind 是上游失败类别，映射到给客户端的不同 HTTP 状态。
type errKind string

const (
	kindHardCredit  errKind = "hard_credit"
	kindSoftRate    errKind = "soft_rate"
	kindSessionDead errKind = "session_dead"
	kindNotFound    errKind = "not_found"
	kindServer      errKind = "server"
	kindClient      errKind = "client"
)

var (
	hardCreditMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "credit not enough", "not enough credit",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	}
	sessionDeadMarkers = []string{"Offline user session not found", "12153"}
)

// classify 从 HTTP 状态与响应体摘要判断失败类别。
func classify(status int, body string) errKind {
	if status == 402 {
		return kindHardCredit
	}
	if status == 401 {
		return kindSessionDead
	}
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return kindHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return kindSessionDead
		}
	}
	switch {
	case status == 429:
		return kindSoftRate
	case status == 404:
		return kindNotFound
	case status >= 500:
		return kindServer
	default:
		return kindClient
	}
}

// statusFor 把失败类别映射为返回给客户端的 HTTP 状态。
func statusFor(kind errKind) int {
	switch kind {
	case kindHardCredit:
		return http.StatusPaymentRequired
	case kindSoftRate:
		return http.StatusTooManyRequests
	case kindSessionDead:
		return http.StatusUnauthorized
	case kindNotFound, kindServer:
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}

// upstreamMessage 从上游 JSON 体里取可读错误信息（优先 displayMsg.zh/en，再 msg）。
func upstreamMessage(body string) string {
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

// Model 是目录里一个可用模型。
type Model struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ContextWindow  int    `json:"contextWindow,omitempty"`
	MaxOutputToken int    `json:"maxOutputTokens,omitempty"`
	SupportsImages bool   `json:"supportsImages,omitempty"`
	// Credits 是计费倍率的展示形态（如 "x0.79"）。上游目录行顶层 credits，
	// 有的行带单位词（"x0.79 credits"）——统一去掉单位词，与插件的
	// normalizeCredits 行为一致。
	Credits string `json:"credits,omitempty"`

	// Reasoning 元数据（行顶层 supportsReasoning / onlyReasoning + reasoning 对象）。
	// SupportedEfforts 为 nil 表示上游未声明档位——此时不注入 variants，
	// 让 OpenCode 保持通用默认档；非推理模型则注入空 variants 压掉自动档，
	// 避免客户端选到上游会 400 的档位（实测：通用档 low/medium/high 不是
	// 每个模型都收）。
	Reasoning          bool     `json:"reasoning,omitempty"`
	SupportedEfforts   []string `json:"supportedEfforts,omitempty"`
	DefaultEffort      string   `json:"defaultEffort,omitempty"`
	CanDisableThinking bool     `json:"canDisableThinking,omitempty"`
	OnlyReasoning      bool     `json:"onlyReasoning,omitempty"`

	// SupportedContexts 是上游声明的上下文档位（contextWindow.supportedLengths）。
	// ContextWindow 只是默认预算（实测常为 300k），模型硬上限是 maxInputTokens；
	// 上游不收 context 字段，选大档位 = 注入 OpenCode 的 limit.context 放宽
	// 客户端预算（见 WORKBUDDY_CONTEXT）。
	SupportedContexts []int `json:"supportedContexts,omitempty"`
}

// wbMaxContext 返回上下文注入值：mode=max 时取声明档位的最大值（至少不低于
// 默认预算），否则用默认预算。
func (m Model) wbMaxContext(mode string) int {
	if mode != "max" || len(m.SupportedContexts) == 0 {
		return m.ContextWindow
	}
	best := m.ContextWindow
	for _, v := range m.SupportedContexts {
		if v > best {
			best = v
		}
	}
	return best
}

// effortValues 是 WorkBuddy 线上承认的思考强度拼写（与插件 EFFORT_VALUES 一致）。
var effortValues = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// parseCatalog 解析 /v3/config 文档（兼容裸文档与 {code,msg,data} 信封两种形态）。
// 目录的权威顺序取自 agents 里 name=="cli" 的 models 列表，再按 id 从 models 行补齐。
func parseCatalog(doc map[string]any) ([]Model, error) {
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
	byID := map[string]Model{}
	for _, r := range rows {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id := wbStr(rm["id"])
		if id == "" || rm["disabled"] == true {
			continue
		}
		m := Model{ID: id, Name: firstNonEmpty(wbStr(rm["name"]), id)}
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
			if lens, ok := cw["supportedLengths"].([]any); ok {
				for _, v := range lens {
					if n := wbNum(v); n > 0 {
						m.SupportedContexts = append(m.SupportedContexts, n)
					}
				}
			}
		}
		if rm["supportsImages"] == true && rm["disabledMultimodal"] != true {
			m.SupportsImages = true
		}
		// 计费倍率：行顶层 credits（实测 billing 为 null，倍率不在里面）。
		m.Credits = normalizeCredits(wbStr(rm["credits"]))
		// 思考元数据：行顶层 supportsReasoning/onlyReasoning + reasoning 对象
		// （转录自插件 resolveUpstreamReasoning；档位只认已知拼写）。
		m.Reasoning = rm["supportsReasoning"] == true
		m.OnlyReasoning = rm["onlyReasoning"] == true
		if r, ok := rm["reasoning"].(map[string]any); ok {
			if efforts, ok := r["supportedEfforts"].([]any); ok {
				for _, e := range efforts {
					if s, ok := e.(string); ok && effortValues[s] {
						m.SupportedEfforts = append(m.SupportedEfforts, s)
					}
				}
			}
			if s := wbStr(r["defaultEffort"]); effortValues[s] {
				m.DefaultEffort = s
			} else if s := wbStr(r["effort"]); effortValues[s] {
				m.DefaultEffort = s
			}
			m.CanDisableThinking = r["canDisableThinking"] == true
		}
		byID[id] = m
	}
	var out []Model
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

// ---------- 计费（积分余额） ----------

// normalizeCredits 把目录里的计费倍率字符串规范成裸倍率（"x0.79 credits" →
// "x0.79"）。与插件的 normalizeCredits 一致：去首尾空白、去掉结尾的单位词
// credits/credit（大小写不敏感）、只剩单位词或空串视为没有倍率。
func normalizeCredits(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	lowered := strings.ToLower(t)
	if lowered == "credits" || lowered == "credit" {
		return ""
	}
	for _, suffix := range []string{" credits", " credit"} {
		if strings.HasSuffix(lowered, suffix) {
			t = strings.TrimSpace(t[:len(t)-len(suffix)])
			break
		}
	}
	if strings.EqualFold(t, "credits") || strings.EqualFold(t, "credit") {
		return ""
	}
	return t
}

// CreditAccount 是一个计费套餐的剩余额度（个人账户的 Accounts 行，或
// 企业账户展开出的单行）。
type CreditAccount struct {
	Package   string  `json:"package"`
	Remain    float64 `json:"remain"`
	Used      float64 `json:"used,omitempty"` // 周期已用（周期制）或 容量-剩余（总量制）
	Size      float64 `json:"size,omitempty"`
	EndTimeMs int64   `json:"end_time_ms,omitempty"` // 0 = 未知
	Unlimited bool    `json:"unlimited,omitempty"`   // 上游 -1 = 无上限
}

// CreditExpiryBucket 是一个到期时间窗内的待失效积分汇总。
type CreditExpiryBucket struct {
	Total    float64 `json:"total"`
	Packages int     `json:"packages"`
}

// CreditExpiry 是「即将过期」的三个时间窗（面板横幅用）。
type CreditExpiry struct {
	Within24h CreditExpiryBucket `json:"within_24h"`
	Within3d  CreditExpiryBucket `json:"within_3d"`
	Within7d  CreditExpiryBucket `json:"within_7d"`
}

// ReqLog 是一次 WorkBuddy 上游调用的极简记录（面板「最近请求」用）。
// console 的 request-logs 只统计 Zen 渠道，workbuddy 请求 janus 自己记。
type ReqLog struct {
	Time   string `json:"time"`
	Model  string `json:"model"`
	Status int    `json:"status"` // janus 侧响应码（200=成功）
	DurMs  int64  `json:"dur_ms"`
}

// Credits 是一次积分查询的聚合结果。与插件 WorkBuddyCredits 同构。
type Credits struct {
	Total          float64         `json:"total"`
	TotalUsed      float64         `json:"total_used,omitempty"` // 已消耗积分合计
	Unlimited      bool            `json:"unlimited,omitempty"`
	Accounts       []CreditAccount `json:"accounts,omitempty"`
	Expiry         *CreditExpiry   `json:"expiry,omitempty"`
	CycleResetTime string          `json:"cycle_reset_time,omitempty"` // 仅 CN 企业账户
	Region         string          `json:"region"`
	FetchedAt      time.Time       `json:"fetched_at"`
	// Recent 是面板展示用的最近请求（handleUsage 组装时附加，非上游字段）。
	Recent []ReqLog `json:"recent,omitempty"`
}

// flexFloat 宽松取数：接受 JSON number，也接受数字字符串（不同版本上游
// 金额字段偶有字符串形态，插件按 number 实测，这里放宽一点不伤语义）。
func flexFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func flexFloatOr(v any, def float64) float64 {
	if n, ok := flexFloat(v); ok {
		return n
	}
	return def
}

// billingBase 计费端点基址（国际版与 chat 同域；国内版在 codebuddy.cn）。
func (c *Credential) billingBase() string {
	if c.region() == "global" {
		return globalBase
	}
	return cnBillBase
}

// billingHeaders 计费请求头（与插件 billingHeaders 一致：Bearer token +
// 账号身份；企业号带 X-Enterprise-Id/X-Tenant-Id）。
func billingHeaders(cred *Credential) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+cred.AccessToken)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	if cred.UID != "" {
		h.Set("X-User-Id", cred.UID)
	}
	if cred.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", cred.EnterpriseID)
		h.Set("X-Tenant-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		h.Set("X-Domain", cred.Domain)
	}
	return h
}

// fetchCredits 拉取账号剩余积分。CN 企业账户走单周期配额端点，其余走套餐列表
// 端点（转录自插件 fetchCredits / fetchEnterpriseCredits，行为逐条对齐）。
func (c *workbuddyClient) fetchCredits(ctx context.Context) (*Credits, error) {
	cred := c.credential()
	if cred.region() == "cn" && cred.EnterpriseID != "" {
		return c.fetchEnterpriseCredits(ctx, cred)
	}
	return c.fetchPersonalCredits(ctx, cred)
}

// fetchPersonalCredits 是套餐列表端点：返回有效期内（Status 0/3）的 p_tcaca
// 套餐及其周期余量。remain 选取：有周期容量用周期余量；否则看周期字段是否
// 出现过；再否则退回总量余量。负数按 0 计。
func (c *workbuddyClient) fetchPersonalCredits(ctx context.Context, cred *Credential) (*Credits, error) {
	base := cred.billingBase()
	if c.baseOverride != "" {
		base = c.baseOverride
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, err := c.billingPost(ctx, base+"/v2/billing/meter/get-user-resource", cred, body)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("计费响应不是 JSON: %s", shrink(raw, 200))
	}
	// 信封：{code,data:{Response:{Data:{Accounts}}}}（与插件逐层一致；兼容
	// 无 data/Response 的扁平形态）。
	root := doc
	if d, ok := doc["data"].(map[string]any); ok {
		root = d
	}
	resp, _ := root["Response"].(map[string]any)
	if resp == nil {
		resp = root
	}
	data, _ := resp["Data"].(map[string]any)
	if data == nil {
		data = resp
	}
	rows, _ := data["Accounts"].([]any)
	out := &Credits{Region: cred.region(), FetchedAt: time.Now().UTC()}
	for _, row := range rows {
		acc, ok := row.(map[string]any)
		if !ok {
			continue
		}
		size := flexFloatOr(acc["CycleCapacitySize"], 0)
		cycleRemain := flexFloatOr(acc["CycleCapacityRemain"], 0)
		cycleUsed := flexFloatOr(acc["CycleCapacityUsed"], 0)
		var remain float64
		switch {
		case size > 0:
			remain = cycleRemain
		case cycleRemain > 0 || cycleUsed > 0:
			remain = cycleRemain
		default:
			remain = flexFloatOr(acc["CapacityRemain"], 0)
		}
		if remain < 0 {
			remain = 0
		}
		entry := CreditAccount{
			Package:   firstNonEmpty(wbStr(acc["PackageName"]), "(unnamed)"),
			Remain:    remain,
			Size:      size,
			EndTimeMs: parseCycleEndTimeMs(acc["CycleEndTime"]),
		}
		if entry.Size == 0 {
			entry.Size = flexFloatOr(acc["CapacitySize"], 0)
		}
		// 已用：容量-剩余 推导（实测国际版端点的 CycleCapacityUsed 恒 0，
		// 消耗真实体现在 remain 上）；无容量信息时才退回 CycleCapacityUsed。
		switch {
		case entry.Size > 0:
			entry.Used = entry.Size - remain
		case cycleUsed > 0:
			entry.Used = cycleUsed
		}
		if entry.Used < 0 {
			entry.Used = 0
		}
		out.Total += remain
		out.TotalUsed += entry.Used
		out.Accounts = append(out.Accounts, entry)
	}
	out.Expiry = summariseExpiry(out.Accounts, time.Now())
	return out, nil
}

// fetchEnterpriseCredits 是 CN 企业账户端点：body 空对象，身份只走 header，
// 返回单周期配额（limitNum/limit_num + credit/used_num 双拼写都认）。
// limit=-1 是上游的「无上限」标记，必须显式携带不能当余额渲染。
func (c *workbuddyClient) fetchEnterpriseCredits(ctx context.Context, cred *Credential) (*Credits, error) {
	base := cred.billingBase()
	if c.baseOverride != "" {
		base = c.baseOverride
	}
	raw, err := c.billingPost(ctx, base+"/v2/billing/meter/get-enterprise-user-usage", cred, map[string]any{})
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("计费响应不是 JSON: %s", shrink(raw, 200))
	}
	// 与插件一致：候选读位 = data.data、data、信封本身，取第一个带配额字段的。
	var sources []map[string]any
	for _, candidate := range []any{doc["data"], doc} {
		rec, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := rec["data"].(map[string]any); ok {
			sources = append(sources, inner)
		}
		sources = append(sources, rec)
	}
	var limit, used float64
	var haveLimit, haveUsed bool
	var resetTime string
	for _, src := range sources {
		l, ok := flexFloat(src["limitNum"])
		if !ok {
			l, ok = flexFloat(src["limit_num"])
		}
		if !ok {
			continue
		}
		limit = l
		haveLimit = true
		u, ok := flexFloat(src["credit"])
		if !ok {
			u, ok = flexFloat(src["used_num"])
		}
		if ok {
			used = u
			haveUsed = true
		}
		if s := wbStr(src["cycleResetTime"]); s != "" {
			resetTime = s
		}
		break
	}
	if !haveLimit {
		return nil, fmt.Errorf("企业计费响应没有可识别的配额字段（期望 limitNum/limit_num + credit/used_num）: %s", shrink(raw, 200))
	}
	out := &Credits{Region: cred.region(), FetchedAt: time.Now().UTC(), CycleResetTime: resetTime}
	if limit == -1 {
		out.Unlimited = true
		out.Accounts = []CreditAccount{{Package: "enterprise", Unlimited: true}}
		return out, nil
	}
	if !haveUsed {
		return nil, fmt.Errorf("企业计费响应有配额上限但没有可识别的用量字段（期望 credit/used_num）: %s", shrink(raw, 200))
	}
	remain := limit - used
	if remain < 0 {
		remain = 0
	}
	out.Total = remain
	out.TotalUsed = used
	out.Accounts = []CreditAccount{{Package: "enterprise", Remain: remain, Used: used, Size: limit}}
	return out, nil
}

// billingPost 发一次计费 POST 并校验信封 code==0。
func (c *workbuddyClient) billingPost(ctx context.Context, url string, cred *Credential, body any) ([]byte, error) {
	bctx, cancel := context.WithTimeout(ctx, jsonTimeout)
	defer cancel()
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(bctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header = billingHeaders(cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("计费请求 http %d: %s", resp.StatusCode, shrink(raw, 200))
	}
	// 信封校验容错：个别形态可能没有 code 字段（按成功处理），有且非 0 才拒绝。
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err == nil {
		if code, ok := probe["code"].(float64); ok && int(code) != 0 {
			return nil, fmt.Errorf("计费被拒 code=%d: %s", int(code), wbStr(probe["msg"]))
		}
	}
	return raw, nil
}

// parseCycleEndTimeMs 解析套餐到期时间（"YYYY-MM-DD HH:mm:ss" 本地时区）。
// 形态不对返回 0（未知），与插件的 parseCycleEndTime 语义一致。
func parseCycleEndTimeMs(v any) int64 {
	s := strings.TrimSpace(wbStr(v))
	if s == "" {
		return 0
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// summariseExpiry 汇总三个时间窗内将失效的积分。只算正余额与未来的到期
// （已过去的套餐不算「即将过期」），与插件 summariseExpiry 一致。
func summariseExpiry(accounts []CreditAccount, now time.Time) *CreditExpiry {
	hasEnd := false
	for _, a := range accounts {
		if a.EndTimeMs != 0 {
			hasEnd = true
			break
		}
	}
	if !hasEnd {
		return nil
	}
	day := 24 * time.Hour
	bucket := func(horizon time.Duration) CreditExpiryBucket {
		var b CreditExpiryBucket
		for _, a := range accounts {
			if a.Remain <= 0 || a.EndTimeMs == 0 {
				continue
			}
			end := time.UnixMilli(a.EndTimeMs)
			if end.After(now) && !end.After(now.Add(horizon)) {
				b.Total += a.Remain
				b.Packages++
			}
		}
		return b
	}
	return &CreditExpiry{
		Within24h: bucket(day),
		Within3d:  bucket(3 * day),
		Within7d:  bucket(7 * day),
	}
}

// ---------- 客户端 ----------

// workbuddyClient 是 WorkBuddy 上游客户端。凭据可热更新（refresh 后）。
type workbuddyClient struct {
	http   *http.Client
	appVer string

	mu        sync.RWMutex
	cred      *Credential
	refreshMu sync.Mutex

	// onRefresh 在续期成功后回调（用于把新凭据落盘，可选）。
	onRefresh func(*Credential)
	// baseOverride 供测试改写基址（为空则按凭据 region 选）。
	baseOverride string
}

func newWorkBuddyClient(cred *Credential, appVer string) *workbuddyClient {
	if strings.TrimSpace(appVer) == "" {
		appVer = fallbackAppVer
	}
	return &workbuddyClient{
		// 不设 Timeout：chat 是长连接 SSE，超时靠 context 控制。
		http:   &http.Client{},
		appVer: appVer,
		cred:   cred,
	}
}

func (c *workbuddyClient) credential() *Credential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cred
}

func (c *workbuddyClient) setCredential(cred *Credential) {
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
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
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
	req.Header = chatHeaders(cred, c.appVer)
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
	rctx, cancel := context.WithTimeout(ctx, jsonTimeout)
	defer cancel()

	base := cred.chatBase()
	if c.baseOverride != "" {
		base = c.baseOverride
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, base+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	req.Header = commonHeaders(cred)
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
func (c *workbuddyClient) fetchCatalog(ctx context.Context) ([]Model, error) {
	cred := c.credential()
	base := cred.chatBase()
	if c.baseOverride != "" {
		base = c.baseOverride
	}
	rctx, cancel := context.WithTimeout(ctx, jsonTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header = catalogHeaders(cred, c.appVer)
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
	return parseCatalog(doc)
}

// shrink 截断字节串用于日志/错误回显。
func shrink(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// firstNonEmpty 返回第一个非空白值（与 app 包同名工具语义一致）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
