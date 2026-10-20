package api

import (
	"crypto/sha256"
	"encoding/hex"
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
// 语义：/mcp/<bindingToken> 解析到「该 key 名下最近活跃的会话」的工具集；
// 没有活跃会话时走空会话路径（与未知 token 相同），不报错、不判死连接。
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
