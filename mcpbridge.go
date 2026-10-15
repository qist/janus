package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// mcpbridge.go —— local(stdio) 工具桥的落地：把内嵌的桥脚本写到 opencode 进程
// 可见的路径，供 AddMCPStdio 注册时引用。
//
// 背景：Bun（opencode 运行时）的 fetch 对 remote 请求有 ~360s 总时长硬顶
// （无视 AbortSignal 与数据流动，实测复现），remote MCP 的长工具调用必死。
// 改为 local(stdio) 注册后，桥脚本自管超时绕开该上限。
//
// 脚本落点：
//   - workspace 会话（dir 在 WorkspacesDir 下）：opencode 的 workspace 驱动在
//     scope 目录里跑子进程，只见沙箱内路径 → 脚本必须写进 dir（实测 /tmp 不可见）。
//   - 同机项目会话（dir 为真实项目目录）：无沙箱 → 写中央数据目录，不弄脏用户项目。

//go:embed mcpbridge.js
var mcpBridgeScript string

const mcpBridgeFileName = ".janus-mcp-bridge.js"

// bridgeScriptPath 计算桥脚本应落地的绝对路径。
func bridgeScriptPath(workspacesDir, dir string) string {
	root := filepath.Clean(workspacesDir)
	clean := filepath.Clean(dir)
	if root != "" && strings.HasPrefix(clean+string(filepath.Separator), root+string(filepath.Separator)) {
		return filepath.Join(dir, mcpBridgeFileName)
	}
	if data := janusDataDir(); data != "" {
		return filepath.Join(data, "mcp-bridge.js")
	}
	// 连数据目录都没有（极简环境）：退回 dir，至少能用。
	return filepath.Join(dir, mcpBridgeFileName)
}

// ensureBridgeScript 幂等写入桥脚本（内容不变则跳过），返回其绝对路径。
func ensureBridgeScript(workspacesDir, dir string) (string, error) {
	target := bridgeScriptPath(workspacesDir, dir)
	if cur, err := os.ReadFile(target); err == nil && string(cur) == mcpBridgeScript {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(mcpBridgeScript), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return target, nil
}
