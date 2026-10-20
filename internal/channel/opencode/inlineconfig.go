package opencode

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"

	"janus/internal/config"
)

// ---------- 为 OpenCode 自动生成配置（内联注入） ----------
//
// janus 通过环境变量 OPENCODE_CONFIG_CONTENT 把「自己需要的 agent 定义」和
// 「janus provider（反向注入）」内联注入到它拉起的 OpenCode 进程，优先级高于
// 全局/项目配置，因此：
//   - 不用手写 ~/.config/opencode/opencode.jsonc；
//   - 不覆盖用户自己的配置（用户配置仍在，janus 的只覆盖同名 agent）；
//   - OpenCode 升级/改工具名时，只要改这里的生成逻辑即可，不会漏。
//
// 注意：只有 janus 自己拉起的 OpenCode 能拿到这个 env；复用外部已在跑的
// OpenCode 时注入不了（那种情况 mode B 不保证，应让 janus 自管上游）。

// InlineConfig 生成注入 OpenCode 的内联配置 JSON。
// toolPrefix 是工具桥注册到 OpenCode 的 MCP server 名前缀（如 "ob-"）。
// 返回空字符串表示不注入。
func InlineConfig(cfg *config.Config, toolPrefix string) string {
	inline := map[string]any{
		"agents": map[string]any{"orchestrator": OrchestratorAgent(toolPrefix)},
	}

	// 反向注入 janus provider：让 OpenCode 用 janus 作为模型来源，
	// 厂商凭据只留在 janus 的 credentials 表里（见 docs/REFACTOR.md 的「Agent 面」）。
	if prov := JanusProviderConfig(cfg); prov != nil {
		inline["provider"] = map[string]any{"janus": prov}
	}

	b, err := json.Marshal(inline)
	if err != nil {
		return ""
	}
	return string(b)
}

// OrchestratorAgent 组装 OpenCode 的 orchestrator agent 定义（mode B/C 用）。
//
// 白名单而非黑名单——黑名单会随 OpenCode 版本改工具名而失效（实测 v2 就漏过）。
//
//   - deny            → 禁掉所有内置工具
//     read allow        → 免费档必需：opencode.ai 的免费档（opencode/*-free）
//     只放行"看起来像 OpenCode 客户端"的请求，判定依据是请求里
//     带标准内置工具；工具集为空时一律 403 FreeTierError
//     （"free tier can only be used from within OpenCode"）。
//     实测 deny* 只 allow read 即可放行；execute/ob-*/bash/
//     edit/write/glob/grep 都不行（v0.3.50 定位）。
//     不使用 read 即不受影响；用它换免费档可用，其余文件/命令
//     操作仍必须由客户端声明的 ob-* 工具执行。
//     execute allow     → 必须放行，否则 Code Mode 里一个 MCP 工具都看不到
//     <toolPrefix>*     → 只放行 janus 工具桥注册的客户端工具（ob-*）
func OrchestratorAgent(toolPrefix string) map[string]any {
	return map[string]any{
		"description": "Only calls tools declared by the API client; never touches the host filesystem. " +
			"Used by janus so the agent runs centrally while file/shell work executes on the client.",
		"mode": "primary",
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "deny"},
			{"action": "read", "resource": "*", "effect": "allow"},
			{"action": "execute", "resource": "*", "effect": "allow"},
			{"action": toolPrefix + "*", "resource": "*", "effect": "allow"},
		},
	}
}

// JanusProviderConfig 组装 OpenCode 的 janus provider 配置块。
// 未开启注入、或缺少 base URL / API key 时返回 nil（调用方跳过注入）。
func JanusProviderConfig(cfg *config.Config) map[string]any {
	if cfg == nil || !cfg.OpencodeProviderInject {
		return nil
	}
	base := strings.TrimSpace(cfg.OpencodeProviderBaseURL)
	if base == "" {
		base = DeriveJanusBaseURL(cfg.Addr)
	}
	return ProviderConfig(base, cfg.OpencodeProviderAPIKey, cfg.OpencodeProviderModels)
}

// ProviderConfig 按显式参数组装 janus provider 块（内联注入与 provision 共用）。
// baseURL 会自动补 /v1；apiKey 或 baseURL 为空返回 nil。
func ProviderConfig(baseURL, apiKey string, modelNames []string) map[string]any {
	key := strings.TrimSpace(apiKey)
	base := strings.TrimSpace(baseURL)
	if key == "" || base == "" {
		return nil
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	models := map[string]any{}
	for _, m := range modelNames {
		if m = strings.TrimSpace(m); m != "" {
			models[m] = map[string]any{}
		}
	}
	return map[string]any{
		"npm":     "@ai-sdk/openai-compatible",
		"name":    "Janus",
		"options": map[string]any{"baseURL": base, "apiKey": key},
		"models":  models,
	}
}

// DeriveJanusBaseURL 从监听地址推导出本机可直连的基址。
//
// 0.0.0.0 / :: / 空 host 一律换成 127.0.0.1（OpenCode 与 janus 同机）；
// 端口缺失时退回默认 2810。返回空字符串表示无法推导。
func DeriveJanusBaseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 没有端口：整串当 host
		host, port = addr, ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		host = "127.0.0.1"
	}
	if port == "" {
		port = "2810"
	}
	if _, err := strconv.Atoi(port); err != nil {
		port = "2810"
	}
	return "http://" + net.JoinHostPort(host, port)
}
