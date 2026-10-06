package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 全部来自「环境变量 > 配置文件 > 内置默认值」。
//
// 配置文件是简单的 KEY=VALUE 文本（见 janus.env.example）：
//   - 以 # 开头的行为注释
//   - 值两端的引号会被去掉
//   - 不展开 ${VAR}
//   - 已存在的真实环境变量优先，便于临时覆盖
type Config struct {
	Addr     string // bridge 监听地址
	Upstream string // OpenCode server 地址

	// UpstreamAuto 为 true 时启动时/断连时自动发现 OpenCode 端点，
	// 覆盖 Upstream/Username/Password。OpenCode 桌面端每次重启都会换
	// 随机端口和随机密码，靠它才能免配置续上。
	UpstreamAuto bool

	// AutostartUpstream：自动发现找不到运行中的 OpenCode 时，由本桥以随机端口
	// 把 `opencode serve` 拉起来（OPENCODE_AUTOSTART，默认 true）。
	// OpencodeBin 可选，显式指定 opencode 可执行文件（OPENCODE_BIN）。
	AutostartUpstream bool
	OpencodeBin       string

	// ReuseExternal：是否复用已在跑的外部 OpenCode（OPENCODE_REUSE_EXTERNAL，默认 true）。
	// 设为 false 时 janus 总是自己拉起一个，从而能通过 OPENCODE_CONFIG_CONTENT
	// 注入自己生成的 agent 配置（复用外部实例时注入不生效）。
	ReuseExternal bool

	Username string // Basic 用户名，OpenCode 固定为 "opencode"
	Password string // 即 OPENCODE_SERVER_PASSWORD

	APIKey string // 对客户端的鉴权；空 = 接受任意 Bearer（会打 WARN）

	Directory    string // 会话默认工作目录
	Agent        string // 默认 agent
	DefaultModel string // default/auto/空别名实际使用的模型；空=跟随上游默认

	// ModelMap 模型别名映射（如 Claude Code 发的 claude-* → 真实后端模型）。
	// 形如 "claude-3-5-sonnet=opencode-go/gpt-6-luna,claude-3-5-haiku=opencode-go/glm-5.3-flash"。
	ModelMap map[string]string

	// Project 显式项目身份（BRIDGE_PROJECT）；空=自动归一（见 scope.go）。
	Project string
	// ProjectMap 客户端目录 → 项目名（BRIDGE_PROJECT_MAP），
	// 如 "/opt/tvfusion=tvfusion,D:\\project\\tvfusion=tvfusion"。用于跨设备路径归一。
	ProjectMap map[string]string
	// ScopeKey=true 时按 scope(=hash(IDE+项目)) 定位会话（没有会话 id 时的兜底），
	// 而不是按 (system+user+dir)。默认 false。
	ScopeKey bool
	// WorkspacesDir 是远程场景下 per-scope 的中性工作目录根（BRIDGE_WORKSPACES_DIR）。
	// 客户端项目路径在 janus 主机上不存在时，用它当会话目录，避免误认成别的项目。
	WorkspacesDir string

	SessionTTL        time.Duration // 会话空闲回收
	SharedSessionTTL  time.Duration // 共享（scope）会话空闲回收；<0 = 永不回收
	RequestTimeout    time.Duration // 单次补全超时
	ReconcileInterval time.Duration // 事件流对账间隔
	IdlePollInterval  time.Duration // 空闲轮询间隔（终端信号兜底）
	PromptGracePeriod time.Duration // prompt 后多久才开始信任 idle 信号
	StreamHeartbeat   time.Duration // SSE 心跳间隔
	StreamIdleTimeout time.Duration // 流式：多久没有真实数据就判卡死（心跳不算）；0=不启用

	CORSOrigin string
	LogLevel   string

	ToolAnnotations  bool // 是否把 agent 工具活动以注释形式写进 content
	MaxConversations int  // 内存里最多保留多少个会话
	MaxBodyBytes     int64

	// 用量/余额查询（走 OpenCode console API，需要读本地凭据库）
	UsageEnabled bool
	UsageTTL     time.Duration
	OpencodeDB   string // OpenCode 的 SQLite 凭据库路径
	ConsoleURL   string // OpenCode console API 基址
	GoUsageURL   string // OpenCode Go 套餐用量端点（inference 主机，与 console 不同域）

	// 限流
	RateLimitPerMin    int  // 每 key/IP 每分钟请求数；0=不限
	RateLimitBurst     int  // 突发容量；0=同 RateLimitPerMin
	RateLimitGlobalRPM int  // 全局每分钟上限；0=不限
	TrustProxy         bool // 是否信任 X-Forwarded-For（影响限流分桶）

	// 指标
	MetricsPublic bool // /metrics 是否免鉴权（默认需要 API key）

	// Responses API
	ResponsesEnabled bool
	ResponseTTL      time.Duration

	// Anthropic Messages API（/v1/messages，供 Claude Code / Anthropic SDK）
	AnthropicEnabled bool
	// WebSearchEnabled 是否支持 Claude Code 的 web_search 服务端工具
	// （由桥内部调用 OpenCode 的 /api/websearch 执行）。
	WebSearchEnabled bool

	// ModelEcho 决定响应里 model 字段回显什么：
	//   real    （默认）解析后的真实模型，如 opencode-go/mimo-v2.5-pro
	//   request 客户端请求里的原始模型名，如 claude-sonnet-4-5
	ModelEcho string

	// 工具调用（把客户端 tools 经内置 MCP server 暴露给 OpenCode agent）
	ToolCalling    bool          // 是否启用
	ToolSoftFail   bool          // true=注册失败时降级为无工具继续；false=直接报错
	ToolReregister time.Duration // 注册多久后主动续注册（上游重启会丢注册）
	ToolCallWait   time.Duration // 挂起等客户端回填结果的最长时间（执行类工具）
	// ToolCallWaitFast 是只读/编辑类工具的短等待：这类工具正常秒回，卡住基本是客户端卡死，
	// 快速判失败能让模型继续，而不是干等到 ToolCallWait 撞上 IDE 自身超时。
	// <=0 或 >= ToolCallWait 时不启用（全部走 ToolCallWait）。
	ToolCallWaitFast time.Duration
	// ToolFastTools 是走短等待的工具名集合（小写，来自 BRIDGE_TOOL_CALL_WAIT_FAST_TOOLS）。
	// 只列已知的只读/编辑类；未知工具默认走长等待，避免误杀长任务。
	ToolFastTools map[string]bool
	MCPPublicURL  string // 注册给 OpenCode 的 MCP 基址；空=自动用本机回环

	// PermissionReply 自动应答 OpenCode 的权限请求（agent 访问会话目录之外时
	// OpenCode 会先 ask；headless 桥无人应答就会一直挂住，直到客户端超时）。
	// once=仅本次放行，always=放行并记住，reject=拒绝，off=不自动应答。
	PermissionReply string

	// MCPAllow 可选：限制内置 MCP 端点 /mcp/{token} 的来源（逗号分隔的 IP/CIDR）。
	// 留空 = 不限制（OpenCode 与本桥同机时是回环，默认即可）。
	MCPAllow string

	// UserAgent 覆盖所有出站请求的 User-Agent（BRIDGE_USER_AGENT）。
	// 留空 = 用内置默认 `janus/<version> (...)`。Go 默认的 `Go-http-client/1.1`
	// 容易被网关/风控当脚本拦掉，故默认换成 janus 标识。
	UserAgent string

	// DBPath 持久化库路径（SQLite，github.com/qist/sqlite）。
	// 空 / memory / off = 纯内存。默认见 defaultDBPath()。
	DBPath string

	// HistoryMaxBytes 落库的 Chat 历史快照上限；超过则只存会话映射（不存历史）。
	HistoryMaxBytes int

	// ConvTTL 持久化的"会话键 → sessionID/历史"保留时长（janitor 清理）。<0 = 永不清理。
	ConvTTL time.Duration

	ConfigFile string // 实际加载的配置文件路径（空 = 没加载）
}

// ---------- 配置来源 ----------

type cfgLoader struct {
	file map[string]string
}

func (l *cfgLoader) get(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, true
	}
	if l.file != nil {
		if v, ok := l.file[key]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func (l *cfgLoader) str(key, def string) string {
	if v, ok := l.get(key); ok {
		return v
	}
	return def
}

func (l *cfgLoader) boolean(key string, def bool) bool {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func (l *cfgLoader) integer(key string, def int) int {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// dur 支持 "30m" / "600s"，也支持纯秒数 "600"。
func (l *cfgLoader) dur(key string, def time.Duration) time.Duration {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// ttlNever 表示"永不回收/清理"（给 durTTL 用）。
const ttlNever = time.Duration(-1)

// durTTL 与 dur 类似，但额外支持 never/off/none/0 = 永不回收（返回 ttlNever）。
// 用于会话/映射的保留时长：设 0 或 never 就是不按时间回收。
func (l *cfgLoader) durTTL(key string, def time.Duration) time.Duration {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "never", "off", "none", "no", "0":
		return ttlNever
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// loadConfigFile 解析 KEY=VALUE 文本。找不到文件返回 (nil, "")；
// 文件存在但语法有误则报错，避免静默用错配置。
func loadConfigFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		// 允许 `export KEY=VALUE`
		raw = strings.TrimPrefix(raw, "export ")
		eq := strings.Index(raw, "=")
		if eq <= 0 {
			return nil, &configError{Path: path, Line: line, Msg: "expected KEY=VALUE"}
		}
		key := strings.TrimSpace(raw[:eq])
		val := strings.TrimSpace(raw[eq+1:])
		// 去掉成对引号
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		// 行尾注释（仅在无引号包裹时处理）：`KEY=value  # comment`
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type configError struct {
	Path string
	Line int
	Msg  string
}

func (e *configError) Error() string {
	return e.Path + ":" + strconv.Itoa(e.Line) + ": " + e.Msg
}

// resolveConfigPath 决定用哪个配置文件：
// JANUS_CONFIG / BRIDGE_CONFIG 显式指定 > 可执行文件同目录 janus.env > 当前目录 janus.env
func resolveConfigPath() string {
	for _, key := range []string{"JANUS_CONFIG", "BRIDGE_CONFIG"} {
		if p := os.Getenv(key); p != "" {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "janus.env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("janus.env"); err == nil {
		return "janus.env"
	}
	return ""
}

// ---------- 加载 ----------

func LoadConfig() (Config, error) {
	loader := &cfgLoader{}
	cfgPath := resolveConfigPath()
	if cfgPath != "" {
		fileCfg, err := loadConfigFile(cfgPath)
		if err != nil {
			return Config{}, err
		}
		loader.file = fileCfg
	}

	// 密码优先取 OPENCODE_PASSWORD，退化到 OPENCODE_SERVER_PASSWORD
	// （OpenCode server 进程本身用的就是后者）。
	pw := loader.str("OPENCODE_PASSWORD", "")
	if pw == "" {
		pw = loader.str("OPENCODE_SERVER_PASSWORD", "")
	}

	upstream := loader.str("OPENCODE_URL", "auto")
	auto := upstream == "" || strings.EqualFold(upstream, "auto")

	cfg := Config{
		Addr:         loader.str("BRIDGE_ADDR", "0.0.0.0:2810"),
		Upstream:     strings.TrimRight(upstream, "/"),
		UpstreamAuto: auto,

		AutostartUpstream: loader.boolean("OPENCODE_AUTOSTART", true),
		OpencodeBin:       loader.str("OPENCODE_BIN", ""),
		ReuseExternal:     loader.boolean("OPENCODE_REUSE_EXTERNAL", true),

		Username: loader.str("OPENCODE_USERNAME", "opencode"),
		Password: pw,

		APIKey: loader.str("BRIDGE_API_KEY", ""),

		Directory:     loader.str("BRIDGE_DIRECTORY", defaultProjectDir()),
		Agent:         loader.str("BRIDGE_AGENT", "build"),
		DefaultModel:  loader.str("BRIDGE_DEFAULT_MODEL", ""),
		ModelMap:      parseModelMap(loader.str("BRIDGE_MODEL_MAP", "")),
		Project:       loader.str("BRIDGE_PROJECT", ""),
		ProjectMap:    parseProjectMap(loader.str("BRIDGE_PROJECT_MAP", "")),
		ScopeKey:      loader.boolean("BRIDGE_SCOPE_KEY", false),
		WorkspacesDir: loader.str("BRIDGE_WORKSPACES_DIR", defaultWorkspacesDir()),

		SessionTTL:        loader.durTTL("BRIDGE_SESSION_TTL", 30*time.Minute),
		SharedSessionTTL:  loader.durTTL("BRIDGE_SHARED_SESSION_TTL", 24*time.Hour),
		RequestTimeout:    loader.dur("BRIDGE_REQUEST_TIMEOUT", 600*time.Second),
		ReconcileInterval: loader.dur("BRIDGE_RECONCILE_INTERVAL", 3*time.Second),
		IdlePollInterval:  loader.dur("BRIDGE_IDLE_POLL_INTERVAL", 1*time.Second),
		PromptGracePeriod: loader.dur("BRIDGE_PROMPT_GRACE", 2*time.Second),
		StreamHeartbeat:   loader.dur("BRIDGE_STREAM_HEARTBEAT", 15*time.Second),
		StreamIdleTimeout: loader.durTTL("BRIDGE_STREAM_IDLE_TIMEOUT", 120*time.Second),

		CORSOrigin: loader.str("BRIDGE_CORS_ORIGIN", ""),
		LogLevel:   loader.str("BRIDGE_LOG_LEVEL", "info"),

		ToolAnnotations:  loader.boolean("BRIDGE_TOOL_ANNOTATIONS", true),
		MaxConversations: loader.integer("BRIDGE_MAX_CONVERSATIONS", 256),
		MaxBodyBytes:     8 << 20,

		UsageEnabled: loader.boolean("BRIDGE_USAGE_ENABLED", true),
		UsageTTL:     loader.dur("BRIDGE_USAGE_TTL", 30*time.Second),
		OpencodeDB:   loader.str("OPENCODE_DB", defaultOpencodeDB()),
		ConsoleURL:   strings.TrimRight(loader.str("OPENCODE_CONSOLE", "https://opencode.ai/console/api"), "/"),
		GoUsageURL:   loader.str("OPENCODE_GO_USAGE", "https://opencode.ai/inference/go/v1/usage"),

		MetricsPublic: loader.boolean("BRIDGE_METRICS_PUBLIC", false),

		RateLimitPerMin:    loader.integer("BRIDGE_RATE_LIMIT", 0),
		RateLimitBurst:     loader.integer("BRIDGE_RATE_BURST", 0),
		RateLimitGlobalRPM: loader.integer("BRIDGE_RATE_LIMIT_GLOBAL", 0),
		TrustProxy:         loader.boolean("BRIDGE_TRUST_PROXY", false),

		ResponsesEnabled: loader.boolean("BRIDGE_RESPONSES_ENABLED", true),
		ResponseTTL:      loader.dur("BRIDGE_RESPONSE_TTL", 30*time.Minute),
		AnthropicEnabled: loader.boolean("BRIDGE_ANTHROPIC_ENABLED", true),
		WebSearchEnabled: loader.boolean("BRIDGE_WEBSEARCH_ENABLED", true),
		ModelEcho:        normalizeModelEcho(loader.str("BRIDGE_MODEL_ECHO", "real")),

		ToolCalling:      loader.boolean("BRIDGE_TOOL_CALLING", true),
		ToolSoftFail:     loader.boolean("BRIDGE_TOOL_SOFT_FAIL", false),
		ToolReregister:   loader.dur("BRIDGE_TOOL_REREGISTER", 10*time.Minute),
		ToolCallWait:     loader.dur("BRIDGE_TOOL_CALL_WAIT", 5*time.Minute),
		ToolCallWaitFast: loader.dur("BRIDGE_TOOL_CALL_WAIT_FAST", 90*time.Second),
		ToolFastTools:    parseToolSet(loader.str("BRIDGE_TOOL_CALL_WAIT_FAST_TOOLS", defaultFastTools)),
		MCPPublicURL:     strings.TrimRight(loader.str("BRIDGE_MCP_URL", ""), "/"),

		PermissionReply: normalizePermissionReply(loader.str("BRIDGE_PERMISSION_REPLY", "once")),
		MCPAllow:        loader.str("BRIDGE_MCP_ALLOW", ""),
		UserAgent:       loader.str("BRIDGE_USER_AGENT", ""),
		DBPath:          loader.str("BRIDGE_DB", defaultDBPath()),
		HistoryMaxBytes: loader.integer("BRIDGE_HISTORY_MAX_BYTES", 1<<20),
		ConvTTL:         loader.durTTL("BRIDGE_CONV_TTL", 7*24*time.Hour),

		ConfigFile: cfgPath,
	}
	if cfg.UserAgent != "" {
		outboundUA = cfg.UserAgent
	}
	return cfg, nil
}

// defaultOpencodeDB 见 paths.go（跨平台）。

// defaultProjectDir 未配置 BRIDGE_DIRECTORY 时用桥启动时的当前工作目录，
// 避免把某个固定路径写死进默认值。
func defaultProjectDir() string {
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// normalizePermissionReply 校验权限自动应答策略。
// 非法值一律回退到 once：宁可多答一次，也不能让工具挂死。
// parseModelMap 解析 "from=to,from2=to2"。键不区分大小写，忽略非法项。
func parseModelMap(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i <= 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(part[:i]))
		v := strings.TrimSpace(part[i+1:])
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

// parseProjectMap 解析 "路径=项目名,路径2=项目名2"。键（路径）保留原样，忽略非法项。
func parseProjectMap(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		v := strings.TrimSpace(part[i+1:])
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

// defaultFastTools 是默认走「短等待」的工具（只读/编辑类，正常秒回）。
// 命令执行类（RunCommand/execute_command…）不在此列，保持 BRIDGE_TOOL_CALL_WAIT 长等待。
const defaultFastTools = "Grep,Read,Glob,LS,WebFetch,Write,SearchReplace,DeleteFile"

// parseToolSet 解析逗号分隔的工具名集合（转小写）。设成 none/off/- 表示空集（禁用短等待）。
func parseToolSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		switch name {
		case "", "none", "off", "-":
			continue
		}
		out[name] = true
	}
	return out
}

func normalizePermissionReply(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "once", "always", "reject", "off":
		return s
	default:
		return "once"
	}
}

// normalizeModelEcho 归一化 BRIDGE_MODEL_ECHO：只有 request 才回显请求名，其余一律 real。
func normalizeModelEcho(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "request", "req", "client", "alias":
		return "request"
	default:
		return "real"
	}
}
