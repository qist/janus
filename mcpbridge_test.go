package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 桥脚本落点：workspace 会话进 scope 目录（opencode 的 workspace 驱动在沙箱内
// 跑子进程，实测 /tmp 不可见）；同机项目会话进中央数据目录，不弄脏用户项目。
func TestBridgeScriptPath(t *testing.T) {
	ws := "/var/lib/janus/workspaces"
	if got := bridgeScriptPath(ws, ws+"/abc"); got != filepath.Join(ws, "abc", mcpBridgeFileName) {
		t.Fatalf("workspace path = %q", got)
	}
	// dir 恰好等于 WorkspacesDir 本身不算 scope 目录
	if got := bridgeScriptPath(ws, ws); got != filepath.Join(ws, mcpBridgeFileName) && !strings.HasSuffix(got, "mcp-bridge.js") {
		t.Fatalf("root path = %q", got)
	}
	// 同机项目：中央数据目录（janusDataDir 为空时退回 dir）
	got := bridgeScriptPath(ws, "/home/u/proj")
	if janusDataDir() != "" {
		if !strings.HasSuffix(got, filepath.Join(janusDataDir(), "mcp-bridge.js")) {
			t.Fatalf("non-workspace path = %q", got)
		}
	} else if got != filepath.Join("/home/u/proj", mcpBridgeFileName) {
		t.Fatalf("fallback path = %q", got)
	}
	// WorkspacesDir 未配置：dir 不在 workspace 语义下，同样走中央数据目录
	got2 := bridgeScriptPath("", "/home/u/proj")
	if janusDataDir() != "" {
		if !strings.HasSuffix(got2, filepath.Join(janusDataDir(), "mcp-bridge.js")) {
			t.Fatalf("no-ws path = %q", got2)
		}
	} else if got2 != filepath.Join("/home/u/proj", mcpBridgeFileName) {
		t.Fatalf("fallback path = %q", got2)
	}
}

// 幂等写入：内容一致时不重写（保留 mtime，避免无谓 churn）。
func TestEnsureBridgeScriptIdempotent(t *testing.T) {
	dir := t.TempDir()
	p1, err := ensureBridgeScript(filepath.Dir(dir), dir)
	if err != nil {
		t.Fatal(err)
	}
	st1, err := os.Stat(p1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ensureBridgeScript(filepath.Dir(dir), dir)
	if err != nil || p2 != p1 {
		t.Fatalf("second call: %q %v", p2, err)
	}
	st2, _ := os.Stat(p2)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("内容一致时应跳过重写")
	}
}

// AddMCPStdio 的注册配置：local 类型、codemode=false、环境变量注入、
// execution 超时透传、桥脚本 fetch 超时 = ToolCallWait + 30s。
func TestAddMCPStdioConfig(t *testing.T) {
	var cfg map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/api/experimental/mcp/ob-test") {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		query = r.URL.RawQuery
		var body struct {
			Config map[string]any `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cfg = body.Config
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u := NewUpstream(Config{Upstream: srv.URL, Username: "opencode", Password: "x"}, NewLogger("error"))
	err := u.AddMCPStdio(context.Background(), "/ws/a", "ob-test",
		"/usr/bin/opencode", "/ws/a/.janus-mcp-bridge.js", "http://127.0.0.1:2810/mcp/tok", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "location%5Bdirectory%5D=%2Fws%2Fa") {
		t.Fatalf("location query = %q", query)
	}
	if cfg["type"] != "local" || cfg["codemode"] != false {
		t.Fatalf("type/codemode = %v/%v", cfg["type"], cfg["codemode"])
	}
	cmd, _ := cfg["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "/usr/bin/opencode" || cmd[1] != "/ws/a/.janus-mcp-bridge.js" {
		t.Fatalf("command = %v", cmd)
	}
	env, _ := cfg["environment"].(map[string]any)
	if env["BUN_BE_BUN"] != "1" || env["JANUS_MCP_URL"] != "http://127.0.0.1:2810/mcp/tok" {
		t.Fatalf("environment = %v", env)
	}
	// 30m ToolCallWait + 30s 余量 = 1830000ms
	if env["JANUS_MCP_FETCH_TIMEOUT"] != "1830000" {
		t.Fatalf("fetch timeout = %v", env["JANUS_MCP_FETCH_TIMEOUT"])
	}
	to, _ := cfg["timeout"].(map[string]any)
	if to["execution"] != float64(1800000) {
		t.Fatalf("execution = %v", to["execution"])
	}
}
