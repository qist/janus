package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// ---------- bootstrap（env） ----------

func TestLoadReadsBootstrapEnv(t *testing.T) {
	t.Setenv("JANUS_ADDR", "127.0.0.1:1234")
	t.Setenv("JANUS_DB", "/tmp/janus.db")
	t.Setenv("JANUS_DATA_DIR", "/tmp/janus-data")
	t.Setenv("JANUS_LOG_LEVEL", "debug")
	t.Setenv("JANUS_BOOTSTRAP_TOKEN", "tok")

	cfg := Load()
	if cfg.Addr != "127.0.0.1:1234" || cfg.DBPath != "/tmp/janus.db" ||
		cfg.DataDir != "/tmp/janus-data" || cfg.LogLevel != "debug" ||
		cfg.BootstrapToken != "tok" {
		t.Fatalf("bootstrap env 未生效: %+v", cfg)
	}
	// 运行期字段应有默认值，且不受 env 影响。
	if cfg.Upstream != "" || !cfg.UpstreamAuto {
		t.Errorf("upstream 默认应为 auto: %q auto=%v", cfg.Upstream, cfg.UpstreamAuto)
	}
	if cfg.RequestTimeout != 600*time.Second {
		t.Errorf("request_timeout 默认 = %v", cfg.RequestTimeout)
	}
}

// 旧 env 不再被读取（不做兼容层）。
func TestLoadIgnoresLegacyEnv(t *testing.T) {
	t.Setenv("BRIDGE_ADDR", "1.2.3.4:1")
	t.Setenv("BRIDGE_API_KEY", "legacy")
	cfg := Load()
	if cfg.Addr == "1.2.3.4:1" {
		t.Error("BRIDGE_ADDR 不应再被读取")
	}
	if cfg.APIKey != "" {
		t.Errorf("BRIDGE_API_KEY 不应再被读取: %q", cfg.APIKey)
	}
}

// ---------- 运行期设置（DB） ----------

func TestApplySettingsOverridesRuntimeOnly(t *testing.T) {
	cfg := Load()
	cfg.Addr = "127.0.0.1:9999"
	cfg.DBPath = "/tmp/keep.db"

	raw := []byte(`{"directory":"/work","default_model":"p/m","request_timeout":"45s"}`)
	if err := cfg.ApplySettings(raw, json.Unmarshal); err != nil {
		t.Fatal(err)
	}
	if cfg.Directory != "/work" || cfg.DefaultModel != "p/m" {
		t.Fatalf("运行期设置未覆盖: %+v", cfg)
	}
	if cfg.RequestTimeout != 45*time.Second {
		t.Fatalf("duration 解析失败: %v", cfg.RequestTimeout)
	}
	// bootstrap 字段不能被库里的值覆盖。
	if cfg.Addr != "127.0.0.1:9999" || cfg.DBPath != "/tmp/keep.db" {
		t.Fatalf("bootstrap 被覆盖: %q %q", cfg.Addr, cfg.DBPath)
	}
	// 未出现的项回落默认值。
	if cfg.SessionTTL != 30*time.Minute {
		t.Fatalf("缺省项未回落默认: %v", cfg.SessionTTL)
	}
}

func TestApplySettingsEmptyKeepsDefaults(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:1"}
	if err := cfg.ApplySettings(nil, json.Unmarshal); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBodyBytes <= 0 || cfg.RequestTimeout <= 0 || cfg.SessionTTL <= 0 {
		t.Fatalf("空设置应补齐默认值: %+v", cfg)
	}
	if cfg.Addr != "127.0.0.1:1" {
		t.Fatalf("addr 被改写: %q", cfg.Addr)
	}
}

func TestApplySettingsRejectsBadJSON(t *testing.T) {
	cfg := Load()
	if err := cfg.ApplySettings([]byte("{"), json.Unmarshal); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// 旧版 BRIDGE_* 里默认开启的开关与默认时长，重构后必须仍然生效。
func TestLoadSeedsLegacyDefaults(t *testing.T) {
	cfg := Load()
	for _, c := range []struct {
		name string
		got  bool
	}{
		{"autostart_upstream", cfg.AutostartUpstream},
		{"reuse_external", cfg.ReuseExternal},
		{"upstream_cleanup", cfg.UpstreamCleanup},
		{"usage_enabled", cfg.UsageEnabled},
		{"responses_enabled", cfg.ResponsesEnabled},
		{"anthropic_enabled", cfg.AnthropicEnabled},
		{"websearch_enabled", cfg.WebSearchEnabled},
		{"tool_calling", cfg.ToolCalling},
		{"tool_companions", cfg.ToolCompanions},
	} {
		if !c.got {
			t.Errorf("%s 默认应为 true", c.name)
		}
	}
	if cfg.StreamIdleTimeout != 120*time.Second {
		t.Errorf("stream_idle_timeout 默认 = %v", cfg.StreamIdleTimeout)
	}
	if cfg.ToolCallWaitFast != 90*time.Second {
		t.Errorf("tool_call_wait_fast 默认 = %v", cfg.ToolCallWaitFast)
	}
}

// 库里的显式 false / 0 不能被 seed 覆盖。
func TestApplySettingsKeepsExplicitOff(t *testing.T) {
	cfg := Load()
	raw := []byte(`{"anthropic_enabled":false,"responses_enabled":false,` +
		`"tool_calling":false,"stream_idle_timeout":"0s","tool_call_wait_fast":"30s"}`)
	if err := cfg.ApplySettings(raw, json.Unmarshal); err != nil {
		t.Fatal(err)
	}
	if cfg.AnthropicEnabled || cfg.ResponsesEnabled || cfg.ToolCalling {
		t.Fatalf("显式关闭被重新打开: %+v", cfg)
	}
	if cfg.StreamIdleTimeout != 0 {
		t.Fatalf("stream_idle_timeout 显式 0 被改写: %v", cfg.StreamIdleTimeout)
	}
	if cfg.ToolCallWaitFast != 30*time.Second {
		t.Fatalf("tool_call_wait_fast 显式值被改写: %v", cfg.ToolCallWaitFast)
	}
	// 未出现的开关仍应保持默认开启。
	if !cfg.UsageEnabled || !cfg.WebSearchEnabled || !cfg.ToolCompanions {
		t.Fatalf("缺省项未回落默认: %+v", cfg)
	}
}

// "never" 表示关闭流式空闲超时，不能被当成未设置而回填 120s。
func TestApplySettingsStreamIdleNever(t *testing.T) {
	cfg := Load()
	if err := cfg.ApplySettings([]byte(`{"stream_idle_timeout":"never"}`), json.Unmarshal); err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != 0 {
		t.Fatalf("never 应折算为 0，got %v", cfg.StreamIdleTimeout)
	}
}

// Encode 应只序列化运行期字段（bootstrap 标了 json:"-"）。
func TestEncodeOmitsBootstrap(t *testing.T) {
	cfg := Load()
	cfg.DBKey = "secret-key"
	raw, err := cfg.Encode(json.Marshal)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"addr", "db_path", "db_key", "bootstrap_token", "api_key"} {
		if _, ok := m[k]; ok {
			t.Errorf("bootstrap 字段 %q 不应入库", k)
		}
	}
	if _, ok := m["request_timeout"]; !ok {
		t.Error("运行期字段应被序列化")
	}
}

// ---------- 归一化 ----------

func TestNormalizePermissionReply(t *testing.T) {
	cases := map[string]string{
		"":        "once",
		"once":    "once",
		"ALWAYS":  "always",
		" reject": "reject",
		"off":     "off",
		"bogus":   "once",
	}
	for in, want := range cases {
		if got := NormalizePermissionReply(in); got != want {
			t.Errorf("NormalizePermissionReply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseToolSet(t *testing.T) {
	got := ParseToolSet("Grep, Read ,grep,,RunCommand")
	if !got["grep"] || !got["read"] || !got["runcommand"] {
		t.Fatalf("解析结果缺项: %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("去重/去空失败: %+v", got)
	}
	for _, off := range []string{"", "none", "off", "-"} {
		if s := ParseToolSet(off); len(s) != 0 {
			t.Fatalf("ParseToolSet(%q) 应为空: %+v", off, s)
		}
	}
}

func TestNormalizeModelEcho(t *testing.T) {
	if normalizeModelEcho("req") != "request" || normalizeModelEcho("") != "real" {
		t.Error("normalizeModelEcho 归一化错误")
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		def  time.Duration
		want time.Duration
	}{
		{"", time.Hour, time.Hour},
		{"30m", time.Hour, 30 * time.Minute},
		{"120", time.Hour, 120 * time.Second},
		{"bogus", time.Hour, time.Hour},
		{" 45s ", time.Hour, 45 * time.Second},
	}
	for _, c := range cases {
		if got := ParseDuration(c.in, c.def); got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestKeyFilePath(t *testing.T) {
	cfg := Config{DataDir: "/data"}
	if got, want := cfg.KeyFilePath(), filepath.Join("/data", "bootstrap.key"); got != want {
		t.Fatalf("KeyFilePath = %q, want %q", got, want)
	}
	if got := (Config{}).KeyFilePath(); got != "" {
		t.Fatalf("无 data dir 应返回空: %q", got)
	}
}
