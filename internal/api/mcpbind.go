package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// mcpbind.go —— 稳定 MCP 端点（按 agent key 绑定）。
//
// 背景：联网 harness（codex / claude-code）要把 janus 的 MCP server 写进自己的
// 静态配置，URL 必须**稳定**；而工具桥原生的 token 是「一次聊天会话一个」
// （toolSession.token，随会话生灭），harness 启动时无从预知。
//
// 这里给每个 agent key 一个确定性 token：
//
//	bindingToken = "b" + sha256("janus-mcp:" + keyID)[:16]
//
// 语义：/mcp/<bindingToken> 是 key 级**回退入口**，只在候选会话唯一（或唯一
// 有在飞请求）时解析到该会话；并发下存在多个候选时宁可落到空会话路径，
// 也不按请求到达顺序猜「最近活跃」。
//
// 需要并发的场景（janus 自己拉起 CLI harness）应使用会话级精确 token
// （toolSession.token）——见 Server.runtimeMCPURL。
//
// 安全：keyID 是随机主键，token 与 key 一一对应且不可从别处推出；token 本身就是
// 该端点的凭据（与 /mcp/{token} 的既有约定一致）。

const (
	mcpBindingPrefix = "b"
	// mcpBindingLen = len(prefix) + 32 个 hex 字符。
	mcpBindingLen = 33
)

// MCPBindingToken 返回 agent key 对应的稳定 MCP token；keyID 为空返回 ""。
func MCPBindingToken(keyID string) string {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return ""
	}
	h := sha256.Sum256([]byte("janus-mcp:" + keyID))
	return mcpBindingPrefix + hex.EncodeToString(h[:16])
}

// isMCPBindingToken 判断 token 形态是否是稳定绑定 token。
//
// 随机会话 token 是 32 位 hex（randomToken），长度不同；两者不会互相误判。
func isMCPBindingToken(tok string) bool {
	return len(tok) == mcpBindingLen && strings.HasPrefix(tok, mcpBindingPrefix)
}

// mcpURLToken 提取 MCP 端点 URL 里的 token（/mcp/{token}）。
//
// 只认最后两段是 mcp/{token} 的形态；其它 URL（包括外部 MCP server）
// 返回 ""，调用方据此判断是否需要改写。
func mcpURLToken(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "mcp" {
		return ""
	}
	return parts[len(parts)-1]
}

// runtimeMCPURL 决定传给 CLI harness 的 MCP 地址。
//
// 调用方给的是 key 级稳定绑定地址、且 sessionKey 能定位到本进程的会话时，
// 改写成该会话的精确 token：稳定绑定在并发下只能 fail-closed，精确地址
// 才能保证多个 CLI 子进程各自拿到自己会话的工具。其它形态原样返回。
func (s *Server) runtimeMCPURL(raw, sessionKey string) string {
	raw = strings.TrimSpace(raw)
	sessionKey = strings.TrimSpace(sessionKey)
	if raw == "" || sessionKey == "" || !isMCPBindingToken(mcpURLToken(raw)) {
		return raw
	}
	sess := s.tools.ByKey(sessionKey)
	if sess == nil {
		return raw
	}
	return s.mcpEndpoint(sess.token)
}
