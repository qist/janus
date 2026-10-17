package config

import (
	"os"
	"testing"
	"time"
)

// ---------- 配置文件解析 ----------

func writeTempCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/janus.env"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigFileParsesQuotesAndComments(t *testing.T) {
	p := writeTempCfg(t, `
# 注释
BRIDGE_API_KEY="sk-with space"
BRIDGE_DIRECTORY=/tmp/proj   # 行尾注释
export BRIDGE_AGENT='plan'

   # 空行与缩进
BRIDGE_SESSION_TTL=45m
BRIDGE_TOOL_ANNOTATIONS=false
BRIDGE_MAX_CONVERSATIONS=7
`)
	m, err := loadConfigFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["BRIDGE_API_KEY"] != "sk-with space" {
		t.Errorf("quoted value: %q", m["BRIDGE_API_KEY"])
	}
	if m["BRIDGE_DIRECTORY"] != "/tmp/proj" {
		t.Errorf("inline comment not stripped: %q", m["BRIDGE_DIRECTORY"])
	}
	if m["BRIDGE_AGENT"] != "plan" {
		t.Errorf("export/single quote: %q", m["BRIDGE_AGENT"])
	}
	if m["BRIDGE_SESSION_TTL"] != "45m" || m["BRIDGE_TOOL_ANNOTATIONS"] != "false" ||
		m["BRIDGE_MAX_CONVERSATIONS"] != "7" {
		t.Errorf("parse: %+v", m)
	}
}

func TestConfigFileRejectsBadLine(t *testing.T) {
	p := writeTempCfg(t, "GOOD=1\nthis line has no equals\n")
	if _, err := loadConfigFile(p); err == nil {
		t.Fatal("expected error for malformed line")
	}
}

func TestConfigFileMissingIsNotError(t *testing.T) {
	m, err := loadConfigFile("/nonexistent/janus.env")
	if err != nil || m != nil {
		t.Fatalf("m=%v err=%v", m, err)
	}
}

// 环境变量必须压过配置文件。
func TestEnvOverridesConfigFile(t *testing.T) {
	t.Setenv("BRIDGE_CONFIG", writeTempCfg(t, "BRIDGE_API_KEY=from-file\nBRIDGE_ADDR=127.0.0.1:1111\n"))
	t.Setenv("BRIDGE_API_KEY", "from-env")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "from-env" {
		t.Errorf("env should win, got %q", cfg.APIKey)
	}
	if cfg.Addr != "127.0.0.1:1111" {
		t.Errorf("file value should apply when env unset, got %q", cfg.Addr)
	}
}

func TestConfigFileDurationAndBool(t *testing.T) {
	t.Setenv("BRIDGE_CONFIG", writeTempCfg(t, "BRIDGE_REQUEST_TIMEOUT=120\nBRIDGE_USAGE_ENABLED=0\n"))
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestTimeout != 120*time.Second {
		t.Errorf("timeout = %v", cfg.RequestTimeout)
	}
	if cfg.UsageEnabled {
		t.Error("BRIDGE_USAGE_ENABLED=0 应解析为 false")
	}
}

// ---------- 取值归一化 ----------

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
		if got := normalizePermissionReply(in); got != want {
			t.Errorf("normalizePermissionReply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseToolSet(t *testing.T) {
	got := parseToolSet("Grep, Read ,grep,,RunCommand")
	if !got["grep"] || !got["read"] || !got["runcommand"] {
		t.Fatalf("解析结果缺项: %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("去重/去空失败: %+v", got)
	}
	for _, off := range []string{"", "none", "off", "-"} {
		if s := parseToolSet(off); len(s) != 0 {
			t.Fatalf("parseToolSet(%q) 应为空: %+v", off, s)
		}
	}
}

func TestParseModelMap(t *testing.T) {
	m := parseModelMap(" claude-3-5-sonnet = opencode-go/gpt-6-luna , claude-3-5-haiku=opencode-go/glm-5.3-flash ,bad,=")
	if m["claude-3-5-sonnet"] != "opencode-go/gpt-6-luna" {
		t.Fatalf("sonnet 映射: %+v", m)
	}
	if m["claude-3-5-haiku"] != "opencode-go/glm-5.3-flash" {
		t.Fatalf("haiku 映射: %+v", m)
	}
	if len(m) != 2 {
		t.Fatalf("只应有 2 条: %+v", m)
	}
	if parseModelMap("") == nil || len(parseModelMap("")) != 0 {
		t.Fatal("空串应返回空 map")
	}
}

func TestNormalizeModelEcho(t *testing.T) {
	if normalizeModelEcho("req") != "request" || normalizeModelEcho("") != "real" {
		t.Error("normalizeModelEcho 归一化错误")
	}
}

// durTTL 支持 never/off/none/0 = Never，普通值照常解析。
func TestDurTTL(t *testing.T) {
	l := &cfgLoader{file: map[string]string{
		"T_NEVER": "never",
		"T_OFF":   "off",
		"T_NONE":  "none",
		"T_ZERO":  "0",
		"T_DUR":   "30m",
		"T_SEC":   "120",
	}}
	for _, k := range []string{"T_NEVER", "T_OFF", "T_NONE", "T_ZERO"} {
		if got := l.durTTL(k, time.Hour); got != Never {
			t.Fatalf("durTTL(%s) = %v, want Never", k, got)
		}
	}
	if got := l.durTTL("T_DUR", time.Hour); got != 30*time.Minute {
		t.Fatalf("durTTL(T_DUR) = %v, want 30m", got)
	}
	if got := l.durTTL("T_SEC", time.Hour); got != 120*time.Second {
		t.Fatalf("durTTL(T_SEC) = %v, want 120s", got)
	}
	if got := l.durTTL("T_UNSET", time.Hour); got != time.Hour {
		t.Fatalf("未设置应返回默认, got %v", got)
	}
}
