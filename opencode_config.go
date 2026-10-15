package main

import "encoding/json"

// opencodeConfigContentEnv 是注入 OpenCode 内联配置的环境变量名。
// 同时也是「janus 托管 opencode」的识别标记之一（见 discover.go 的
// isManagedOpenCode）：旧版本拉起的实例没有 JANUS_MANAGED_UPSTREAM，
// 但都会带上这个 env。
const opencodeConfigContentEnv = "OPENCODE_CONFIG_CONTENT"

// ---------- 为 OpenCode 自动生成配置（内联注入） ----------
//
// janus 通过环境变量 OPENCODE_CONFIG_CONTENT 把「自己需要的 agent 定义」
// 内联注入到它拉起的 OpenCode 进程，优先级高于全局/项目配置，因此：
//   - 不用手写 ~/.config/opencode/opencode.jsonc；
//   - 不覆盖用户自己的配置（用户配置仍在，janus 的只覆盖同名 agent）；
//   - OpenCode 升级/改工具名时，只要改这里的生成逻辑即可，不会漏。
//
// 注意：只有 janus 自己拉起的 OpenCode 能拿到这个 env；复用外部已在跑的
// OpenCode 时注入不了（那种情况 mode B 不保证，应让 janus 自管上游）。

// opencodeInlineConfig 生成注入 OpenCode 的内联配置 JSON。
// 返回空字符串表示不注入。
func opencodeInlineConfig(cfg *Config) string {
	agents := map[string]any{}

	// orchestrator：给 mode B/C 用。白名单而非黑名单——
	// 黑名单会随 OpenCode 版本改工具名而失效（实测 v2 就漏过）。
	//   * deny            → 禁掉所有内置工具
	//   read allow        → 免费档必需：opencode.ai 的免费档（opencode/*-free）
	//                       只放行"看起来像 OpenCode 客户端"的请求，判定依据是请求里
	//                       带标准内置工具；工具集为空时一律 403 FreeTierError
	//                       （"free tier can only be used from within OpenCode"）。
	//                       实测 deny* 只 allow read 即可放行；execute/ob-*/bash/
	//                       edit/write/glob/grep 都不行（v0.3.50 定位）。
	//                       不使用 read 即不受影响；用它换免费档可用，其余文件/命令
	//                       操作仍必须由客户端声明的 ob-* 工具执行。
	//   execute allow     → 必须放行，否则 Code Mode 里一个 MCP 工具都看不到
	//   <mcpNamePrefix>*  → 只放行 janus 工具桥注册的客户端工具（ob-*）
	agents["orchestrator"] = map[string]any{
		"description": "Only calls tools declared by the API client; never touches the host filesystem. " +
			"Used by janus so the agent runs centrally while file/shell work executes on the client.",
		"mode": "primary",
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "deny"},
			{"action": "read", "resource": "*", "effect": "allow"},
			{"action": "execute", "resource": "*", "effect": "allow"},
			{"action": mcpNamePrefix + "*", "resource": "*", "effect": "allow"},
		},
	}

	// 扩展点：后续可按 role/模型生成 providers / 每模型 settings 等，
	// 都从这里一并注入（见 docs/ARCHITECTURE.md 的「模型访问层」）。

	b, err := json.Marshal(map[string]any{"agents": agents})
	if err != nil {
		return ""
	}
	return string(b)
}
