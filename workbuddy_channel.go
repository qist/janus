package main

// workbuddy_channel.go —— WorkBuddy 上游的「渠道校验」识别、出站路径兜底与会话头族。
//
// 背景（2026-10-17 起）：上游对 /v2/chat/completions 启用了渠道强校验 —— 请求不是
// 官方客户端发出的形态时，回 11128 / 11-128「没通过渠道校验，可能不是官方客户端
// 发出」（HTTP 400）。官方客户端走的是 **/console/chat/completions**，该路径放行。
//
// 上游 workbuddy2api 对同一现象的修复（见 workbuddy-manager issue #229 方向的
// 0006/0005 补丁）口径是：
//
//  1. 先识别「渠道校验」：业务码（11128 / 11-128）**且**文案出现校验措辞，两者同时
//     成立才算 —— 同一个码还承载「内容审核拦截」，那种绝不能被吞成渠道问题；
//  2. 命中后换 /console 路径重试 —— 这是**路径级**拒绝，不是账号被封（凭据是好的，
//     让用户删号重扫是白折腾）；
//  3. 会话头族按官方 CLI 形态出站（X-Conversation-* 恒 32 位 hex 且会话内稳定、
//     X-Request-ID = 消息级 ID、链路族 X-Trace-ID / X-B3-* 齐备），并带官方风控
//     闸门头 X-CodeBuddy-Request: 1 —— 所有 API 请求必带。
//
// 本文件按同口径实现：wbIsChannelCheck 判定、wbChatPaths 兜底顺序、wbConversationIDs
// 会话头族派生、wbEnsureConsoleSystem 首条 system 兜底。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// wbChatPathV2 是上游 chat 的主路径（上游 2026-10-17 起对这条路径加了渠道强校验）。
	wbChatPathV2 = "/v2/chat/completions"
	// wbChatPathConsole 是官方客户端走的 chat 路径：渠道校验只拦 /v2，这条放行。
	wbChatPathConsole = "/console/chat/completions"
)

// wbChatPaths 返回出站 chat 的候选路径（顺序即尝试顺序）。
//
// 与上游同口径：**只有**命中「渠道校验」才换路径；404 / 405 之类不回落（那是路径
// 写错，不是校验问题，换过去只是把错误搬个家）。
func wbChatPaths() []string {
	return []string{wbChatPathV2, wbChatPathConsole}
}

// wbIsChannelCheck 判上游的「渠道校验」拒绝（11128 / 11-128 家族，2026-10-17）。
//
// 指纹要**两条同时成立**：业务码（11128 或 11-128）**且**文案里出现渠道校验的
// 措辞。只认码不行 —— 同一个 11-128 还承载「内容审核拦截」
// （blocked by security policy），那种必须保持原行为（不换路径、不当成渠道问题）。
// 文案两种语言都可能出现：中文「渠道校验」与英文 displayTips 的 channel
// verification / official client；`request illegal` 这类历史 msg 也可能随同一封套
// 出现，所以不能拿 msg 当判据。
func wbIsChannelCheck(body string) bool {
	lower := strings.ToLower(body)
	if !wbCodeMarker(lower, "11128") && !wbCodeMarker(lower, "11-128") {
		return false
	}
	return strings.Contains(body, "渠道校验") ||
		strings.Contains(lower, "channel verification") ||
		strings.Contains(lower, "official client")
}

// wbCodeMarker 判 JSON code 字段命中（`"code":N` / `"code": N` / `"code":"N"` 等形态，
// 入参需已转小写）。与上游 codeMarker 同口径。
func wbCodeMarker(lower, code string) bool {
	for _, v := range []string{
		`"code":` + code,
		`"code": ` + code,
		`"code":"` + code + `"`,
		`"code": "` + code + `"`,
		`"code":" ` + code + `"`,
		`"code": '` + code + `'`,
	} {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return false
}

// wbBodyCode 取上游封套里的业务码原文（数字或 11-128 这种带横线的字符串），供错误
// 提示引用 —— 排障按真实码去搜方向，写死历史码会把人带偏。
func wbBodyCode(body string) string {
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return ""
	}
	switch n := doc["code"].(type) {
	case float64:
		return fmt.Sprintf("%d", int64(n))
	case string:
		return strings.TrimSpace(n)
	}
	return ""
}

// wbCodeSuffix 把业务码拼成「（code 11128）」形式；无码时返回空串。
func wbCodeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return "（code " + code + "）"
}

// wbEnsureConsoleSystem 在首条消息不是 system 时前置一条兜底 system。
//
// console 域上游对「首条非 system」的请求回 11-128；出站补一条即绕开该形态校验。
// 首条已是 system 时**不动** body（不重复注入）。
func wbEnsureConsoleSystem(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	if first, ok := msgs[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body
		}
	}
	obj["messages"] = append([]any{
		map[string]any{"role": "system", "content": "You are a helpful assistant."},
	}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ---------- 会话头族 ----------

// wbConvIDs 是一次出站 chat 携带的会话头族 ID（对齐官方 CLI 形态）。
type wbConvIDs struct {
	// ConversationID 是会话级：同一会话多轮恒同（上游按此聚合对话）。
	ConversationID string
	// ConversationRequestID 是对话轮级：一次 user send 内恒同（换轮即换键）。
	ConversationRequestID string
	// MessageID 是消息级：每条请求独立。
	MessageID string
}

// wbConvSalt 是会话 ID 派生盐（进程级随机；重启换新 —— 重启后旧对话已结束）。
var wbConvSalt = wbRandomHex(16)

// wbRandomHex 返回 n 字节随机 hex（crypto/rand 失败时退化为时间戳派生，保证非空）。
func wbRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("wb-random-%d", len(b))))
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(b)
}

// wbStableID 返回键的稳定派生 ID：sha256(盐|键) 前 16 字节的 hex（恒 32 位 hex，
// 与官方 X-Request-ID 同形态）。同键恒同、异键互异。
func wbStableID(key string) string {
	sum := sha256.Sum256([]byte(wbConvSalt + "|" + key))
	return hex.EncodeToString(sum[:16])
}

// wbConversationIDs 推导一次 chat 的会话头族 ID：
//
//   - X-Conversation-ID：客户端给了会话 ID 就透传（上游按它聚合，伪造会打乱后台的
//     会话维度）；否则按**首条** user 消息派生 —— 一次会话内它不变，故同会话多轮恒同，
//     不同会话首条消息不同故不会被并成一个会话；首条也没有就随机。
//   - X-Conversation-Request-ID：按**末条** user 消息派生（一次 user send 内的多次
//     上游调用恒同 → 后台聚合成一条，而不是一条请求一条记录）。
//   - X-Conversation-Message-ID：每条请求独立（消息级）。
//
// 官方客户端的这些 ID 都是 32 位 hex（UUID 去横线的长度形态），故派生与随机都走
// 32 hex，不用带横线的 UUID。
func wbConversationIDs(body []byte) wbConvIDs {
	ids := wbConvIDs{MessageID: wbRandomHex(16)}
	first, last := wbUserAnchors(body)
	if cid := wbResolveConversationID(body); cid != "" {
		ids.ConversationID = cid
	} else if first != "" {
		ids.ConversationID = wbStableID("conv:" + first)
	} else {
		ids.ConversationID = ids.MessageID
	}
	if last != "" {
		ids.ConversationRequestID = wbStableID("turn:" + last)
	} else {
		ids.ConversationRequestID = ids.MessageID
	}
	return ids
}

// wbResolveConversationID 从请求体提取客户端给的会话 ID（metadata 优先、snake 优先于
// camel）。缺失返回 ""（不伪造：客户端没给就派生，不透传空串）。
func wbResolveConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := wbStr(meta["conversation_id"]); v != "" {
			return v
		}
		if v := wbStr(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := wbStr(obj["conversation_id"]); v != "" {
		return v
	}
	return wbStr(obj["conversationId"])
}

// wbChatMsg 是请求体里的一条消息（解析会话锚点用）。
type wbChatMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// wbUserAnchors 返回请求体里首条与末条 user 消息的锚点：首条用作会话级锚点，末条
// 用作对话轮级锚点（带序号，区分「两轮问了同一句」）。content 支持字符串与 parts
// 数组两种形态。
//
// 与上游同口径：某条 user 消息无可签名内容（空串 / null / 空 parts）时**不**往前
// 找 —— 往前找会让键随会话推进而漂移（同一位置一旦带上文本，键就变了）。
func wbUserAnchors(body []byte) (first, last string) {
	if len(body) == 0 {
		return "", ""
	}
	var obj struct {
		Messages []wbChatMsg `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", ""
	}
	for i, m := range obj.Messages {
		if m.Role != "user" {
			continue
		}
		text := wbContentText(m.Content)
		if first == "" {
			// 首条 user 消息无内容 → 会话锚点就此为空（不往后找）。
			first = text
			if text == "" {
				return "", ""
			}
		}
		// 末条 user 消息无内容 → 轮级锚点为空（不往前找）。
		last = wbTurnKey(i, text)
	}
	return first, last
}

// wbTurnKey 组成对话轮级锚点：序号 + 文本。带上序号是为了区分「两轮问了同一句」
// （内容相同但不是同一轮）。
func wbTurnKey(idx int, text string) string {
	return fmt.Sprintf("u%d:%s", idx, text)
}

// wbContentText 取消息 content 的文本：字符串形态直接取；数组形态拼接各 part 的
// text 字段。其它形态返回 ""。
func wbContentText(content any) string {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var b strings.Builder
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// wbAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识（跨重启固定、
// 账号间互异），对齐官方桌面端「每账号一台固定虚拟设备」语义 —— 上游按设备指纹
// 做风控，这两头恒定的头必须带上。uid 为空返回 ""（不注入）。
func wbAccountStableID(uid, purpose string) string {
	if uid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("wb:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}
