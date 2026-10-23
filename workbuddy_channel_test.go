package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 渠道校验的识别（workbuddy-manager#229 同款问题）。
//
// 现场：上游 2026-10-17 起对 /v2/chat/completions 加了渠道强校验，被校验到的请求回
// 「没通过渠道校验，可能不是官方客户端发出的」+ 业务码 11128 / 11-128；官方客户端走
// 的是 /console 端点（放行）。
//
// 注意：下面这串业务码**一律拼接构造**，不手写整串 —— 它在手抄时极易写成 1138/1128，
// 而它正是判据本身，抄错会让测试「通过」一个不存在的判据。
var (
	wbCodeDashed = "1" + "1" + "-" + "1" + "2" + "8" // 实测体里的写法
	wbCodePlain  = "1" + "1" + "1" + "2" + "8"       // 报告方另见的写法
)

// wbChannelBody 一次渠道校验的真实封套：中文「渠道校验」+ 英文 displayTips +
// 历史遗留的 `request illegal` msg（后者正是把账号误判成「被封」的触发点）。
var wbChannelBody = `{"code":` + wbCodePlain + `,"msg":"request illegal","requestId":"x",` +
	`"displayMsg":{"zh":"请求被拦截，请重新发送"},` +
	`"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。",` +
	`"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`

// 渠道校验必须被识别（含码的各种写法与中英文文案）。
func TestWBIsChannelCheckMatches(t *testing.T) {
	if !wbIsChannelCheck(wbChannelBody) {
		t.Fatal("中文「渠道校验」文案必须被识别")
	}
	for _, code := range []string{
		`"code":` + wbCodeDashed,
		`"code":` + wbCodePlain,
		`"code":"` + wbCodeDashed + `"`,
	} {
		body := `{` + code + `,"displayTips":{"en":"This call failed channel verification"}}`
		if !wbIsChannelCheck(body) {
			t.Errorf("码写法 %s 没被识别", code)
		}
	}
	// 英文 displayTips 单独也要能命中（中文可能被上游换掉）。
	if !wbIsChannelCheck(`{"code":` + wbCodePlain + `,"displayTips":{"en":"possibly because it was not sent from an official client"}}`) {
		t.Error("英文 official client 文案没被识别")
	}
}

// 内容审核拦截与普通参数错误不能被吞成渠道校验。
func TestWBIsChannelCheckDoesNotSwallowOthers(t *testing.T) {
	// 同一个码也承载「内容审核拦截」：那种必须保持原行为（不换路径）。
	content := `{"code":` + wbCodeDashed + `,"msg":"blocked by security policy"}`
	if wbIsChannelCheck(content) {
		t.Fatal("内容审核拦截被误判成渠道校验 —— 会让它不再走降级重试")
	}
	if got := wbClassify(400, content); got == wbKindChannelCheck {
		t.Errorf("内容审核拦截的分类被影响：%v", got)
	}
	// 没有渠道校验文案时不该命中（单有 `request illegal` 不够）。
	if wbIsChannelCheck(`{"msg":"request illegal"}`) {
		t.Error("没有渠道校验文案时不该命中")
	}
	// 码不对时也不该命中。
	if wbIsChannelCheck(`{"code":11140,"displayTips":{"en":"This call failed channel verification"}}`) {
		t.Error("非 11128 家族的码不该命中")
	}
}

// 渠道校验归独立类别（≠ 账号封禁），且映射为 502（上游侧问题，不是客户端请求错）。
func TestWBClassifyChannelCheck(t *testing.T) {
	if got := wbClassify(400, wbChannelBody); got != wbKindChannelCheck {
		t.Errorf("渠道校验应归 wbKindChannelCheck，got %v", got)
	}
	if got := wbStatusFor(wbKindChannelCheck); got != http.StatusBadGateway {
		t.Errorf("渠道校验应映射 502，got %d", got)
	}
	// 渠道校验不得被误判成余额耗尽 / 会话失效（前者会白扣账号额度，后者会让用户
	// 以为要重新登录 —— 凭证从来没问题）。
	if got := wbClassify(400, wbChannelBody); got == wbKindHardCredit || got == wbKindSessionDead {
		t.Errorf("渠道校验被误归类：%v", got)
	}
}

// 出站路径候选：/v2 优先，/console 兜底。
func TestWBChatPathsHaveConsoleFallback(t *testing.T) {
	paths := wbChatPaths()
	if len(paths) != 2 || paths[0] != "/v2/chat/completions" || paths[1] != "/console/chat/completions" {
		t.Fatalf("路径候选应为 [/v2, /console]（/v2 优先，兜底 /console），got %v", paths)
	}
}

// 会话头族：同会话多轮恒同、换会话即换、轮级随末条 user 消息变、消息级每条独立；
// 值恒为 32 位 hex（官方 CLI 形态）。
func TestWBConversationIDs(t *testing.T) {
	first := `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"第一句"},{"role":"assistant","content":"a"}]}`
	// 同会话第二轮：多了一条 user 消息，但首条 user 消息不变。
	second := `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"第一句"},{"role":"assistant","content":"a"},{"role":"user","content":"第二句"}]}`
	// 另一个会话：首条 user 消息不同。
	other := `{"messages":[{"role":"user","content":"完全不同的开场白"}]}`

	id1 := wbConversationIDs([]byte(first))
	id2 := wbConversationIDs([]byte(second))
	idOther := wbConversationIDs([]byte(other))

	if id1.ConversationID != id2.ConversationID {
		t.Errorf("同会话的 X-Conversation-ID 应恒同：%q vs %q", id1.ConversationID, id2.ConversationID)
	}
	if id1.ConversationID == idOther.ConversationID {
		t.Error("不同会话的 X-Conversation-ID 不应相同")
	}
	if id1.ConversationRequestID == id2.ConversationRequestID {
		t.Error("换轮的 X-Conversation-Request-ID 应变化")
	}
	if id1.MessageID == id2.MessageID {
		t.Error("消息级 ID 每条应独立")
	}
	for name, v := range map[string]string{
		"ConversationID":        id1.ConversationID,
		"ConversationRequestID": id1.ConversationRequestID,
		"MessageID":             id1.MessageID,
	} {
		if len(v) != 32 {
			t.Errorf("%s 应为 32 位 hex，got %q", name, v)
		}
	}

	// 客户端给了会话 ID → 透传（上游按它聚合，伪造会打乱后台会话维度）。
	client := `{"metadata":{"conversation_id":"client-conv-1"},"messages":[{"role":"user","content":"x"}]}`
	if got := wbConversationIDs([]byte(client)).ConversationID; got != "client-conv-1" {
		t.Errorf("客户端会话 ID 应透传，got %q", got)
	}
}

// 首条 system 兜底：首条已是 system 时不动 body，否则前置一条。
func TestWBEnsureConsoleSystem(t *testing.T) {
	// 已有 system → 不动。
	withSystem := `{"messages":[{"role":"system","content":"keep"},{"role":"user","content":"hi"}]}`
	if got := string(wbEnsureConsoleSystem([]byte(withSystem))); got != withSystem {
		t.Errorf("首条已是 system 时不应改 body，got %s", got)
	}
	// 无 system → 前置一条。
	noSystem := `{"messages":[{"role":"user","content":"hi"}]}`
	out := wbEnsureConsoleSystem([]byte(noSystem))
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应前置一条 system，got %d 条", len(msgs))
	}
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("首条应是 system，got %v", role)
	}
	// 非 JSON body 原样返回（不吞错）。
	if got := string(wbEnsureConsoleSystem([]byte("not json"))); got != "not json" {
		t.Errorf("非 JSON 应原样返回，got %s", got)
	}
}

// 端到端：/v2 被渠道校验拒绝 → 自动换 /console 路径重试并成功（issue #229 的正解）。
func TestWBChatFallsBackToConsoleOnChannelCheck(t *testing.T) {
	var hits []string
	var consoleBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if r.URL.Path == wbChatPathConsole {
			b, _ := io.ReadAll(r.Body)
			consoleBody = string(b)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(wbChannelBody))
	}))
	defer up.Close()

	c := newWorkBuddyClient(&wbCredential{AccessToken: "at", RefreshToken: "rt", Domain: "workbuddy.ai"}, "5.6.2")
	c.baseOverride = up.URL
	resp, err := c.chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("换 /console 后应成功，got %d", resp.StatusCode)
	}
	if len(hits) != 2 || hits[0] != wbChatPathV2 || hits[1] != wbChatPathConsole {
		t.Fatalf("应先打 /v2 再打 /console，got %v", hits)
	}
	// /console 路径的首条消息必须是 system（console 域上游对首条非 system 回 11-128）。
	var obj map[string]any
	if err := json.Unmarshal([]byte(consoleBody), &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("/console 路径首条应为 system，got %v", role)
	}
}

// 端到端：两条路径都被渠道校验拒绝 → 交回错误（不吞），分类为渠道校验。
func TestWBChatBothPathsChannelCheck(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(wbChannelBody))
	}))
	defer up.Close()

	c := newWorkBuddyClient(&wbCredential{AccessToken: "at", Domain: "workbuddy.ai"}, "5.6.2")
	c.baseOverride = up.URL
	resp, err := c.chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("两条路径都拒时应交回上游状态，got %d", resp.StatusCode)
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, wbErrorBodyLimit))
	if got := wbClassify(resp.StatusCode, string(head)); got != wbKindChannelCheck {
		t.Errorf("应分类为渠道校验，got %v", got)
	}
}

// 兜底路径因别的原因失败（如该域没有这个端点）→ 以主路径的渠道校验拒绝为准，
// 别让兜底路径的 404 把真正的原因盖掉。
func TestWBChatKeepsPrimaryChannelCheckOnFallbackError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wbChatPathConsole {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"msg":"no console endpoint here"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(wbChannelBody))
	}))
	defer up.Close()

	c := newWorkBuddyClient(&wbCredential{AccessToken: "at", Domain: "workbuddy.ai"}, "5.6.2")
	c.baseOverride = up.URL
	resp, err := c.chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, wbErrorBodyLimit))
	if got := wbClassify(resp.StatusCode, string(head)); got != wbKindChannelCheck {
		t.Errorf("应保留主路径的渠道校验判定，got %v（body=%s）", got, string(head))
	}
}

// 非渠道校验的错误不换路径（404 之类换过去只是把错误搬个家）。
func TestWBChatNoFallbackOnOtherErrors(t *testing.T) {
	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"msg":"no such thing"}`))
	}))
	defer up.Close()

	c := newWorkBuddyClient(&wbCredential{AccessToken: "at", Domain: "workbuddy.ai"}, "5.6.2")
	c.baseOverride = up.URL
	resp, err := c.chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(hits) != 1 || hits[0] != wbChatPathV2 {
		t.Fatalf("非渠道校验不应换路径，got %v", hits)
	}
}

// 国际版身份头形态：X-Domain 必须与 Origin/Referer 同域（www.workbuddy.ai），
// 且显式声明「无企业」—— 域形态对不齐会被上游判成非官方客户端。
func TestWBChatHeadersGlobalRealmShape(t *testing.T) {
	cred := &wbCredential{AccessToken: "at", Domain: "workbuddy.ai", UID: "u9"}
	h := wbChatHeaders(cred, "5.6.2", wbConvIDs{
		ConversationID:        strings.Repeat("a", 32),
		ConversationRequestID: strings.Repeat("b", 32),
		MessageID:             strings.Repeat("c", 32),
	})
	if h.Get("X-Domain") != "www.workbuddy.ai" {
		t.Errorf("国际版 X-Domain 应为 www.workbuddy.ai（与 Origin 同域），got %q", h.Get("X-Domain"))
	}
	if h.Get("X-No-Enterprise-Id") != "1" {
		t.Error("国际版应显式声明 X-No-Enterprise-Id: 1")
	}
	if h.Get("X-Enterprise-Id") != "" {
		t.Error("国际版不应带 X-Enterprise-Id")
	}
	// 国内版不受影响：企业号按凭据原值透传。
	cn := wbChatHeaders(&wbCredential{
		AccessToken: "at", Domain: "copilot.tencent.com", UID: "u1", EnterpriseID: "e1",
	}, "5.6.2", wbConvIDs{MessageID: strings.Repeat("c", 32)})
	if cn.Get("X-Enterprise-Id") != "e1" {
		t.Errorf("国内版企业号应透传 X-Enterprise-Id，got %q", cn.Get("X-Enterprise-Id"))
	}
	if cn.Get("X-Domain") != "copilot.tencent.com" {
		t.Errorf("国内版 X-Domain 应为凭据原值，got %q", cn.Get("X-Domain"))
	}
}

// 出站身份头：官方客户端风控闸门头 + 稳定设备标识 + 链路族，缺一个都可能被判形态不对。
func TestWBChatHeadersCarryOfficialShape(t *testing.T) {
	cred := &wbCredential{AccessToken: "at", Domain: "workbuddy.ai", UID: "u9"}
	ids := wbConvIDs{
		ConversationID:        strings.Repeat("a", 32),
		ConversationRequestID: strings.Repeat("b", 32),
		MessageID:             strings.Repeat("c", 32),
	}
	h := wbChatHeaders(cred, "5.6.2", ids)

	if h.Get("X-CodeBuddy-Request") != "1" {
		t.Error("缺 X-CodeBuddy-Request（官方客户端风控闸门头）")
	}
	if h.Get("Accept-Language") != "en-US" {
		t.Errorf("国际版 Accept-Language 应为 en-US，got %q", h.Get("Accept-Language"))
	}
	machine := h.Get("X-Machine-ID")
	if machine == "" {
		t.Error("缺 X-Machine-ID")
	} else if machine != wbAccountStableID("u9", "machine") {
		t.Error("X-Machine-ID 应按 uid 稳定派生")
	}
	if h.Get("X-Session-ID") == "" {
		t.Error("缺 X-Session-ID")
	}
	// 会话头族与链路族。
	for _, k := range []string{
		"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID",
		"X-Request-ID", "X-Root-Request-ID", "X-Trace-ID", "X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled",
	} {
		if h.Get(k) == "" {
			t.Errorf("缺 %s", k)
		}
	}
	if h.Get("X-Request-ID") != ids.MessageID {
		t.Error("X-Request-ID 应等于消息级 ID")
	}
	if h.Get("X-B3-SpanId") != ids.MessageID[:16] {
		t.Errorf("X-B3-SpanId 应取消息级 ID 前 16 位，got %q", h.Get("X-B3-SpanId"))
	}
	// 用量归属（上游 injectAttribution 默认 = 官方桌面端指纹）。
	if h.Get("X-Product") != "WorkBuddy" {
		t.Errorf("X-Product 应为 WorkBuddy（桌面端指纹），got %q", h.Get("X-Product"))
	}
	if h.Get("X-Agent-Purpose") != "conversation" {
		t.Errorf("缺 X-Agent-Purpose: conversation，got %q", h.Get("X-Agent-Purpose"))
	}
	// chat 的 Accept 是流式形态（上游 D6）。
	if h.Get("Accept") != "application/json, text/event-stream" {
		t.Errorf("chat Accept 应为流式形态，got %q", h.Get("Accept"))
	}
	// 三段式 UA：WorkBuddy/<v> <platform>/<v> CLI/<cli>（/v3/config UA 门禁同款形态）。
	if !strings.Contains(h.Get("User-Agent"), "WorkBuddy/5.6.2 WorkBuddy AI/5.6.2 CLI/") {
		t.Errorf("UA 应为三段式 CLI 形态，got %q", h.Get("User-Agent"))
	}
	// 国内版不缀 X-Device-Token（凭据无 deviceToken 时）。
	if h.Get("X-Device-Token") != "" {
		t.Error("凭据无 deviceToken 时不应发 X-Device-Token")
	}
	// 带设备风控 token 的凭据：三域（chat 在此断言，billing/refresh 另测）都注入。
	dt := wbChatHeaders(&wbCredential{
		AccessToken: "at", Domain: "workbuddy.ai", UID: "u9", DeviceToken: "dt-1",
	}, "5.6.2", ids)
	if dt.Get("X-Device-Token") != "dt-1" {
		t.Errorf("凭据带 deviceToken 时 chat 应注入 X-Device-Token，got %q", dt.Get("X-Device-Token"))
	}
	// 国内版的 Accept-Language 走 zh-CN。
	cn := wbChatHeaders(&wbCredential{AccessToken: "at", Domain: "copilot.tencent.com"}, "5.6.2", ids)
	if cn.Get("Accept-Language") != "zh-CN" {
		t.Errorf("国内版 Accept-Language 应为 zh-CN，got %q", cn.Get("Accept-Language"))
	}
}

// 目录请求头：通用头 + Bearer（两域同形态，去 X-Product；国内版企业对保留）。
func TestWBCatalogHeadersShape(t *testing.T) {
	cn := wbCatalogHeaders(&wbCredential{
		AccessToken: "at", Domain: "copilot.tencent.com", UID: "u1", EnterpriseID: "e1",
	}, "5.6.2")
	if cn.Get("Authorization") != "Bearer at" {
		t.Errorf("Authorization = %q", cn.Get("Authorization"))
	}
	if !strings.Contains(cn.Get("User-Agent"), "WorkBuddy/5.6.2 WorkBuddy/5.6.2 CLI/") {
		t.Errorf("目录 UA 应为三段式 CLI 形态（/v3/config UA 门禁），got %q", cn.Get("User-Agent"))
	}
	if cn.Get("X-CodeBuddy-Request") != "1" {
		t.Error("目录请求缺 X-CodeBuddy-Request")
	}
	if cn.Get("X-Product") != "" {
		t.Errorf("目录不应带 X-Product（上游 fetch_models 形态），got %q", cn.Get("X-Product"))
	}
	if cn.Get("X-Enterprise-Id") != "e1" || cn.Get("X-Tenant-Id") != "e1" {
		t.Error("国内版企业对（X-Enterprise-Id/X-Tenant-Id）应保留（issue #123）")
	}
	g := wbCatalogHeaders(&wbCredential{AccessToken: "at", Domain: "workbuddy.ai"}, "5.6.2")
	if g.Get("X-User-Id") != "" {
		t.Error("国际版个人账号目录不应带企业身份对")
	}
	if g.Get("Accept-Language") != "en-US" {
		t.Errorf("国际版目录 Accept-Language 应为 en-US，got %q", g.Get("Accept-Language"))
	}
}

// billing 请求头：单段 UA + 通用头 + 身份 + 设备指纹 + 可选设备风控 token。
func TestWBBillingHeadersShape(t *testing.T) {
	cred := &wbCredential{
		AccessToken: "at", Domain: "copilot.tencent.com",
		UID: "u1", EnterpriseID: "e1", DeviceToken: "dt-1",
	}
	h := wbBillingHeaders(cred)
	if h.Get("User-Agent") != "WorkBuddy/"+wbFallbackAppVer {
		t.Errorf("billing UA 应为单段 WorkBuddy/<ver>，got %q", h.Get("User-Agent"))
	}
	if h.Get("X-CodeBuddy-Request") != "1" {
		t.Error("billing 缺 X-CodeBuddy-Request")
	}
	if h.Get("Accept-Language") != "zh-CN" {
		t.Errorf("billing Accept-Language = %q", h.Get("Accept-Language"))
	}
	if h.Get("X-Machine-ID") != wbAccountStableID("u1", "machine") ||
		h.Get("X-Session-ID") != wbAccountStableID("u1", "session") {
		t.Error("billing 缺按 uid 派生的设备指纹头")
	}
	if h.Get("X-User-Id") != "u1" || h.Get("X-Enterprise-Id") != "e1" || h.Get("X-Tenant-Id") != "e1" {
		t.Error("billing 身份头不符")
	}
	if h.Get("X-Device-Token") != "dt-1" {
		t.Errorf("billing 应注入 X-Device-Token，got %q", h.Get("X-Device-Token"))
	}
}

// UA 形态：三段式 + 按域切平台段。
func TestWBUserAgentShape(t *testing.T) {
	g := wbUserAgent(&wbCredential{Domain: "workbuddy.ai"}, "5.6.2")
	if g != "WorkBuddy/5.6.2 WorkBuddy AI/5.6.2 CLI/"+wbFallbackCliVer {
		t.Errorf("国际版 UA = %q", g)
	}
	cn := wbUserAgent(&wbCredential{Domain: "copilot.tencent.com"}, "5.6.2")
	if cn != "WorkBuddy/5.6.2 WorkBuddy/5.6.2 CLI/"+wbFallbackCliVer {
		t.Errorf("国内版 UA = %q", cn)
	}
}

// 凭据里的 deviceToken：扁平与嵌套（auth 段）两种形态都收。
func TestWBParseCredentialDeviceToken(t *testing.T) {
	flat := `{"accessToken":"at","refreshToken":"rt","domain":"workbuddy.ai","deviceToken":"dt-flat"}`
	cred, err := parseWBCredential(flat)
	if err != nil || cred.DeviceToken != "dt-flat" {
		t.Fatalf("扁平形态: cred=%+v err=%v", cred, err)
	}
	nested := `{"auth":{"accessToken":"at","refreshToken":"rt","domain":"workbuddy.ai","deviceToken":"dt-nested"}}`
	cred, err = parseWBCredential(nested)
	if err != nil || cred.DeviceToken != "dt-nested" {
		t.Fatalf("嵌套形态: cred=%+v err=%v", cred, err)
	}
}

// 桌面导出的 uid 在 account / accounts[0] 里（不在 auth/identity）：解析必须
// 兜到，否则 X-User-Id 与设备指纹头全部缺失，出站退化成匿名形态。
func TestWBParseCredentialUIDFromAccount(t *testing.T) {
	in := `{"accessToken":"at","refreshToken":"rt","domain":"www.workbuddy.ai",
		"auth":{"accessToken":"at","refreshToken":"rt","domain":"www.workbuddy.ai"},
		"account":{"uid":"u-account","nickname":"n"},
		"accounts":[{"uid":"u-acc0","nickname":"n0"}]}`
	cred, err := parseWBCredential(in)
	if err != nil {
		t.Fatal(err)
	}
	if cred.UID != "u-account" {
		t.Fatalf("应优先取 account.uid，got %q", cred.UID)
	}
	// 没有 account.uid 时回落 accounts[0].uid。
	in2 := `{"accessToken":"at","refreshToken":"rt","domain":"www.workbuddy.ai",
		"accounts":[{"uid":"u-acc0"}]}`
	cred2, err := parseWBCredential(in2)
	if err != nil {
		t.Fatal(err)
	}
	if cred2.UID != "u-acc0" {
		t.Fatalf("应回落 accounts[0].uid，got %q", cred2.UID)
	}
}
