// Package config 负责 janus 的启动引导配置与运行期设置。
//
// 设计（见 docs/REFACTOR.md）：
//   - **只有 bootstrap 走环境变量**：监听地址、数据库路径、主密钥、数据目录、
//     日志级别、首启 admin token。这些是"打开数据库之前"就必须知道的项，
//     写进数据库会形成循环依赖。
//   - **其余全部落库**：providers / credentials / models / routes / keys /
//     agents / settings 都是数据，经 /api/v1 管理，UI 可改。
//   - 旧 env（BRIDGE_* / OPENCODE_* / WORKBUDDY_*）不再读取，也不做兼容层。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"janus/internal/paths"
	"janus/internal/useragent"
)

// BootstrapKeys 是唯一允许走环境变量的配置项。
var BootstrapKeys = []string{
	"JANUS_ADDR",
	"JANUS_DB",
	"JANUS_DB_KEY",
	"JANUS_DATA_DIR",
	"JANUS_LOG_LEVEL",
	"JANUS_BOOTSTRAP_TOKEN",
}

// Config 同时承载 bootstrap 与运行期设置。
//
// 标注 json:"-" 的字段是 bootstrap（env 提供，不入库）；其余字段会被整体
// 序列化成 settings 表里的 runtime 行，因此可以在 Web 面板里修改。
type Config struct {
	// ---------- bootstrap（env） ----------
	Addr           string `json:"-"` // JANUS_ADDR
	DBPath         string `json:"-"` // JANUS_DB
	DBKey          string `json:"-"` // JANUS_DB_KEY
	DataDir        string `json:"-"` // JANUS_DATA_DIR
	BootstrapToken string `json:"-"` // JANUS_BOOTSTRAP_TOKEN，可选
	ConfigFile     string `json:"-"` // 保留字段：新版本不再读配置文件

	// APIKey 是进程内静态 key（只用于测试与应急），不读 env、不入库。
	// 生产鉴权全部走 api_keys 表。
	APIKey string `json:"-"`

	// ---------- 运行期（DB） ----------
	Upstream     string `json:"upstream"`      // OpenCode server 地址（agent 记录未就绪时的兜底）
	UpstreamAuto bool   `json:"upstream_auto"` // 自动发现 OpenCode 端点

	AutostartUpstream bool   `json:"autostart_upstream"`
	OpencodeBin       string `json:"opencode_bin"`
	OpencodeClient    string `json:"opencode_client"`
	ReuseExternal     bool   `json:"reuse_external"`
	UpstreamCleanup   bool   `json:"upstream_cleanup"`

	// 反向注入：把 janus 自己注册成 OpenCode 的模型 provider，让 OpenCode 不再
	// 持有厂商凭据（凭据只在 janus 的 credentials 表里）。默认关闭。
	OpencodeProviderInject  bool     `json:"opencode_provider_inject"`
	OpencodeProviderBaseURL string   `json:"opencode_provider_base_url"`
	OpencodeProviderAPIKey  string   `json:"opencode_provider_api_key"`
	OpencodeProviderModels  []string `json:"opencode_provider_models"`

	Username string `json:"username"`
	Password string `json:"password"`

	Directory    string `json:"directory"`
	Agent        string `json:"agent"`
	DefaultModel string `json:"default_model"`

	// Project / ProjectMap / ScopeKey / WorkspacesDir 决定会话分桶与工作目录。
	Project       string            `json:"project"`
	ProjectMap    map[string]string `json:"project_map"`
	ScopeKey      bool              `json:"scope_key"`
	WorkspacesDir string            `json:"workspaces_dir"`

	MCPTransport   string `json:"mcp_transport"`
	WedgeForensics bool   `json:"wedge_forensics"`

	// WorkBuddy 渠道（凭据在 credentials 表，这里只留展示/预算档位）。
	WorkBuddyContext string `json:"workbuddy_context"`
	OpenCodeContext  int    `json:"opencode_context"`

	SessionTTL        time.Duration `json:"session_ttl"`
	SharedSessionTTL  time.Duration `json:"shared_session_ttl"`
	RequestTimeout    time.Duration `json:"request_timeout"`
	ReconcileInterval time.Duration `json:"reconcile_interval"`
	IdlePollInterval  time.Duration `json:"idle_poll_interval"`
	PromptGracePeriod time.Duration `json:"prompt_grace_period"`
	StreamHeartbeat   time.Duration `json:"stream_heartbeat"`
	StreamIdleTimeout time.Duration `json:"stream_idle_timeout"`

	CORSOrigin string `json:"cors_origin"`
	LogLevel   string `json:"log_level"`

	ToolAnnotations  bool  `json:"tool_annotations"`
	MaxConversations int   `json:"max_conversations"`
	MaxBodyBytes     int64 `json:"max_body_bytes"`

	UsageEnabled bool          `json:"usage_enabled"`
	UsageTTL     time.Duration `json:"usage_ttl"`
	OpencodeDB   string        `json:"opencode_db"`
	ConsoleURL   string        `json:"console_url"`
	GoUsageURL   string        `json:"go_usage_url"`

	RateLimitPerMin    int  `json:"rate_limit_per_min"`
	RateLimitBurst     int  `json:"rate_limit_burst"`
	RateLimitGlobalRPM int  `json:"rate_limit_global_rpm"`
	TrustProxy         bool `json:"trust_proxy"`
	MetricsPublic      bool `json:"metrics_public"`

	ResponsesEnabled bool          `json:"responses_enabled"`
	ResponseTTL      time.Duration `json:"response_ttl"`
	AnthropicEnabled bool          `json:"anthropic_enabled"`
	WebSearchEnabled bool          `json:"websearch_enabled"`
	ModelEcho        string        `json:"model_echo"`

	ToolCalling      bool            `json:"tool_calling"`
	ToolSoftFail     bool            `json:"tool_soft_fail"`
	ToolReregister   time.Duration   `json:"tool_reregister"`
	ToolCallWait     time.Duration   `json:"tool_call_wait"`
	ToolCallWaitFast time.Duration   `json:"tool_call_wait_fast"`
	ToolFastTools    map[string]bool `json:"tool_fast_tools"`
	ToolCompanions   bool            `json:"tool_companions"`
	ToolOrphanWait   time.Duration   `json:"tool_orphan_wait"`
	MCPPublicURL     string          `json:"mcp_public_url"`

	// CLI 型 harness（codex / claude-code）的可执行文件：janus 驱动它们时启动的子进程。
	CodexBin  string `json:"codex_bin"`
	ClaudeBin string `json:"claude_bin"`

	PermissionReply string `json:"permission_reply"`
	MCPAllow        string `json:"mcp_allow"`
	UserAgent       string `json:"user_agent"`

	HistoryMaxBytes int           `json:"history_max_bytes"`
	ConvTTL         time.Duration `json:"conv_ttl"`
}

// ---------- bootstrap 加载 ----------

// Load 只读取 bootstrap 环境变量；运行期设置由 ApplySettings 从数据库填充。
func Load() Config {
	cfg := Config{
		Addr:           env("JANUS_ADDR", "0.0.0.0:2810"),
		DBPath:         env("JANUS_DB", paths.DefaultDBPath()),
		DBKey:          env("JANUS_DB_KEY", ""),
		DataDir:        env("JANUS_DATA_DIR", paths.DataDir()),
		LogLevel:       env("JANUS_LOG_LEVEL", "info"),
		BootstrapToken: strings.TrimSpace(os.Getenv("JANUS_BOOTSTRAP_TOKEN")),
	}
	cfg.applySeed()
	cfg.applyDefaults()
	return cfg
}

// ApplySettings 把 settings 表里的 runtime 行覆盖到 cfg 上。
// decode 由调用方注入（config 包不依赖 db，避免循环导入）。
func (c *Config) ApplySettings(raw []byte, decode func([]byte, any) error) error {
	if len(raw) == 0 {
		c.applySeed()
		c.applyDefaults()
		return nil
	}
	// 先铺默认值，再用库里的值覆盖：新增设置项对老库自动生效。
	// 铺的是 seed（含"默认开启"的开关），库里的显式 false/0 才能正确生效。
	base := *c
	base.applySeed()
	base.applyDefaults()
	*c = base
	if err := decode(raw, c); err != nil {
		return err
	}
	c.applyDefaults()
	return nil
}

// Encode 把运行期设置序列化成 settings 表的值。
func (c Config) Encode(encode func(any) ([]byte, error)) ([]byte, error) {
	return encode(c)
}

// Never 表示"永不回收/清理"（会话 / 流空闲等 TTL 用）。
const Never = time.Duration(-1)

// MarshalJSON 把 time.Duration 字段写成 "45s" 形式，面板可直接编辑；
// 其余字段沿用标准编码。bootstrap 字段带 json:"-"，不会出现。
func (c Config) MarshalJSON() ([]byte, error) {
	t := reflect.TypeOf(c)
	v := reflect.ValueOf(c)
	out := make(map[string]any, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		key := jsonKey(f)
		if key == "" {
			continue
		}
		if f.Type == reflect.TypeOf(time.Duration(0)) {
			out[key] = time.Duration(v.Field(i).Int()).String()
			continue
		}
		out[key] = v.Field(i).Interface()
	}
	return json.Marshal(out)
}

// UnmarshalJSON 兼容两种时长写法：字符串（"45s"）与数字（纳秒）。
// 只覆盖出现在 JSON 里的字段，bootstrap 与缺省项保持调用方已填的值。
func (c *Config) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t := reflect.TypeOf(*c)
	v := reflect.ValueOf(c).Elem()
	fields := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if key := jsonKey(t.Field(i)); key != "" {
			fields[key] = i
		}
	}
	for key, val := range raw {
		i, ok := fields[key]
		if !ok {
			continue // 未知键（旧版本残留）直接忽略
		}
		fv := v.Field(i)
		if fv.Type() == reflect.TypeOf(time.Duration(0)) {
			d, err := parseDurationJSON(val)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			fv.SetInt(int64(d))
			continue
		}
		if err := json.Unmarshal(val, fv.Addr().Interface()); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// jsonKey 返回字段的 JSON 名；json:"-" 或没有 tag 的返回空。
func jsonKey(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	key := strings.Split(tag, ",")[0]
	if key == "" || key == "-" {
		return ""
	}
	return key
}

// parseDurationJSON 解析时长：字符串走 ParseDuration（支持 never/off），
// 数字按秒（与 ParseDuration 的纯数字语义一致）。
func parseDurationJSON(raw json.RawMessage) (time.Duration, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "never", "off", "none", "no":
			return Never, nil
		}
		return ParseDuration(s, 0), nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("duration must be a string or seconds, got %s", string(raw))
	}
	return time.Duration(n) * time.Second, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ApplyDefaults 补齐零值（供直接构造 Config 的调用方使用）。
//
// 只兜零值，不铺"默认开启"的开关：结构体字面量里的 false 无法与未设置
// 区分，铺 true 会覆盖调用方显式关闭的意图。需要旧版默认值的调用方请走
// Load / ApplySettings。
func (c *Config) ApplyDefaults() { c.applyDefaults() }

// applySeed 铺"无法用零值表达"的默认值：
//   - 默认开启的布尔开关（旧版 BRIDGE_*_ENABLED 等默认 true）
//   - 默认非零的时长（旧版 durTTL/dur 的默认值）
//
// 必须在 decode 之前调用，库里的显式 false/0 才能覆盖它；放进 applyDefaults
// 会把显式关闭重新打开。
func (c *Config) applySeed() {
	c.AutostartUpstream = true
	c.ReuseExternal = true
	c.UpstreamCleanup = true

	c.UsageEnabled = true
	c.ResponsesEnabled = true
	c.AnthropicEnabled = true
	c.WebSearchEnabled = true
	c.ToolCalling = true
	c.ToolCompanions = true

	if c.StreamIdleTimeout == 0 {
		c.StreamIdleTimeout = 120 * time.Second
	}
	if c.ToolCallWaitFast == 0 {
		c.ToolCallWaitFast = 90 * time.Second
	}
}

// applyDefaults 兜底零值：零值很危险（MaxBodyBytes=0 会让所有 POST 读到 EOF）。
func (c *Config) applyDefaults() {
	if c.Addr == "" {
		c.Addr = "0.0.0.0:2810"
	}
	if c.DataDir == "" {
		c.DataDir = paths.DataDir()
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Upstream == "" {
		c.Upstream = "auto"
	}
	if c.Upstream == "auto" || strings.EqualFold(c.Upstream, "auto") {
		c.UpstreamAuto = true
		c.Upstream = ""
	}
	if c.Username == "" {
		c.Username = "opencode"
	}
	if c.Agent == "" {
		c.Agent = "build"
	}
	if c.Directory == "" {
		c.Directory = defaultProjectDir()
	}
	if c.WorkspacesDir == "" {
		c.WorkspacesDir = paths.DefaultWorkspacesDir()
	}
	if c.MCPTransport == "" {
		c.MCPTransport = "local"
	}
	if c.WorkBuddyContext == "" {
		c.WorkBuddyContext = "max"
	}
	if c.OpencodeDB == "" {
		c.OpencodeDB = paths.DefaultOpencodeDB()
	}
	if c.ConsoleURL == "" {
		c.ConsoleURL = "https://opencode.ai/console/api"
	}
	if c.GoUsageURL == "" {
		c.GoUsageURL = "https://opencode.ai/inference/go/v1/usage"
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 30 * time.Minute
	}
	if c.SharedSessionTTL == 0 {
		c.SharedSessionTTL = 24 * time.Hour
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 600 * time.Second
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 3 * time.Second
	}
	if c.IdlePollInterval <= 0 {
		c.IdlePollInterval = time.Second
	}
	if c.PromptGracePeriod <= 0 {
		c.PromptGracePeriod = 2 * time.Second
	}
	if c.StreamHeartbeat <= 0 {
		c.StreamHeartbeat = 15 * time.Second
	}
	if c.StreamIdleTimeout < 0 {
		c.StreamIdleTimeout = 0
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 8 << 20
	}
	if c.MaxConversations <= 0 {
		c.MaxConversations = 256
	}
	if c.UsageTTL <= 0 {
		c.UsageTTL = 30 * time.Second
	}
	if c.ResponseTTL <= 0 {
		c.ResponseTTL = 30 * time.Minute
	}
	if c.ModelEcho == "" {
		c.ModelEcho = "real"
	}
	c.ModelEcho = normalizeModelEcho(c.ModelEcho)
	if c.ToolCallWait <= 0 {
		c.ToolCallWait = 30 * time.Minute
	}
	if c.ToolCallWaitFast <= 0 || c.ToolCallWaitFast >= c.ToolCallWait {
		c.ToolCallWaitFast = 0
	}
	if c.ToolReregister <= 0 {
		c.ToolReregister = 10 * time.Minute
	}
	if c.ToolOrphanWait <= 0 {
		c.ToolOrphanWait = 30 * time.Second
	}
	if c.CodexBin == "" {
		c.CodexBin = "codex"
	}
	if c.ClaudeBin == "" {
		c.ClaudeBin = "claude"
	}
	if c.ToolFastTools == nil {
		c.ToolFastTools = ParseToolSet(defaultFastTools)
	}
	if c.PermissionReply == "" {
		c.PermissionReply = "once"
	}
	c.PermissionReply = NormalizePermissionReply(c.PermissionReply)
	if c.HistoryMaxBytes <= 0 {
		c.HistoryMaxBytes = 1 << 20
	}
	if c.ConvTTL == 0 {
		c.ConvTTL = 7 * 24 * time.Hour
	}
	if c.UserAgent != "" {
		useragent.Default = c.UserAgent
	}
}

func defaultProjectDir() string {
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// NormalizePermissionReply 校验权限自动应答策略，非法值回退 once。
func NormalizePermissionReply(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "once", "always", "reject", "off":
		return s
	default:
		return "once"
	}
}

func normalizeModelEcho(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "request", "req", "client", "alias":
		return "request"
	default:
		return "real"
	}
}

// ParseToolSet 解析逗号分隔的工具名集合（转小写）。none/off/- 表示空集。
func ParseToolSet(s string) map[string]bool {
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

// defaultFastTools 是默认走「短等待」的工具（只读/编辑类，正常秒回）。
const defaultFastTools = "Grep,Read,Glob,LS,WebFetch,Write,SearchReplace,DeleteFile,CheckCommandStatus,check_command_status"

// KeyFilePath 返回首启 admin 凭据落盘位置（0600）。
func (c Config) KeyFilePath() string {
	if c.DataDir == "" {
		return ""
	}
	return filepath.Join(c.DataDir, "bootstrap.key")
}

// ParseDuration 解析 "30m" / "600s" / 纯秒数；空串返回 def。
func ParseDuration(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
