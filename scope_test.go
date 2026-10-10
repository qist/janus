package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeIDE(t *testing.T) {
	cases := map[string]string{
		"hertz":                                "trae",
		"CodeBuddyIDE/4.12.1 CodeBuddy/4.12.1": "codebuddy",
		"":                                     "unknown",
		"SomeIDE/1.0":                          "someide",
	}
	for in, want := range cases {
		if got := normalizeIDE(in); got != want {
			t.Errorf("normalizeIDE(%q)=%q want %q", in, got, want)
		}
	}
}

func TestScopeKeyStableAndDistinct(t *testing.T) {
	a := scopeKey("codebuddy", "qist/tvfusion")
	b := scopeKey("codebuddy", "qist/tvfusion")
	c := scopeKey("trae", "qist/tvfusion")
	if a != b {
		t.Fatalf("同输入应稳定: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("不同 IDE 应不同: %s", a)
	}
	if a[:2] != "s:" {
		t.Fatalf("scope key 前缀应为 s: : %s", a)
	}
}

func TestExtractProjectRoot(t *testing.T) {
	// Trae：Primary working directory 优先，忽略系统/Trae 自身路径
	trae := []ChatMessage{
		{Role: "user", Content: MessageContent{Text: "<system-reminder>\n# Environment\n- Primary working directory: /opt/tvgate\n"}},
		{Role: "tool", Content: MessageContent{Text: "read /usr/local/x /root/.trae-cn/y"}},
	}
	if got := extractProjectRoot(trae); got != "/opt/tvgate" {
		t.Fatalf("trae got %q want /opt/tvgate", got)
	}
	// Copilot Chat：workspace folders
	cop := []ChatMessage{
		{Role: "user", Content: MessageContent{Text: "<workspace_info> I am working in a workspace with the following folders: - /opt/sqlite I am working in a workspace that has the following structure:"}},
	}
	if got := extractProjectRoot(cop); got != "/opt/sqlite" {
		t.Fatalf("copilot got %q want /opt/sqlite", got)
	}
	// 兜底：最频繁路径（排除系统目录）
	msgs := []ChatMessage{
		{Role: "tool", Content: MessageContent{Text: "read /opt/tvfusion/a.go and /opt/tvfusion/b.go"}},
		{Role: "assistant", Content: MessageContent{Text: "cd /opt/tvfusion/x"}},
	}
	if got := extractProjectRoot(msgs); got != "/opt/tvfusion" {
		t.Fatalf("got %q want /opt/tvfusion", got)
	}
	if got := extractProjectRoot(nil); got != "" {
		t.Fatalf("empty got %q", got)
	}
}

func TestProjectIdentityPriority(t *testing.T) {
	s := &Server{cfg: Config{Project: "", ProjectMap: map[string]string{`D:\proj\tvfusion`: "qist/tvfusion"}}}
	// map 命中（Windows 路径归一）
	if got := s.projectIdentity(`D:\proj\tvfusion`, nil); got != "qist/tvfusion" {
		t.Errorf("map: got %q", got)
	}
	// 显式 BRIDGE_PROJECT 最高
	s.cfg.Project = "explicit"
	if got := s.projectIdentity("/whatever", nil); got != "explicit" {
		t.Errorf("explicit: got %q", got)
	}
	// Workspace Folder 兜底 → 取 basename
	s.cfg.Project = ""
	msgs := []ChatMessage{{Role: "user", Content: MessageContent{Text: "<user_info> Workspace Folder: /home/u/tvfusion\nOS: linux"}}}
	if got := s.projectIdentity("", msgs); got != "tvfusion" {
		t.Errorf("workspace folder: got %q", got)
	}
	// 都没有 → default
	if got := s.projectIdentity("", nil); got != "default" {
		t.Errorf("default: got %q", got)
	}
}

// TestSessionDir 验证远程 + mode B 的会话目录选择链：
// header（存在才用）> 消息抽取的项目根（存在才用）> per-scope 中性目录。
func TestSessionDir(t *testing.T) {
	ws := t.TempDir()
	s := &Server{cfg: Config{WorkspacesDir: ws, Directory: "/deploy"}}
	sk := scopeKey("testide", "testproj")
	neutral := filepath.Join(ws, strings.TrimPrefix(sk, scopeKeyPrefix))

	// 1. header 目录在 janus 主机上真实存在 → 直接用
	exists := t.TempDir()
	if got := s.sessionDir(exists, nil, sk); got != exists {
		t.Fatalf("existing header dir: got %q want %q", got, exists)
	}

	// 2. header 是客户端侧路径、janus 上不存在（远程 + mode B，如 Windows 路径）
	//    → 不直接使用，落到 per-scope 中性目录，且目录被创建
	got := s.sessionDir(`C:\Users\dev\proj`, nil, sk)
	if got != neutral {
		t.Fatalf("missing header dir: got %q want %q", got, neutral)
	}
	if fi, err := os.Stat(neutral); err != nil || !fi.IsDir() {
		t.Fatalf("neutral dir not created: %v", err)
	}

	// 3. header 为空，消息抽到真实存在的项目根 → 用它
	msgs := []ChatMessage{{Role: "user", Content: MessageContent{Text: "Primary working directory: " + exists}}}
	if got := s.sessionDir("", msgs, sk); got != exists {
		t.Fatalf("msg-extracted dir: got %q want %q", got, exists)
	}

	// 4. header 为空且消息里抽不到存在的路径 → 中性目录
	if got := s.sessionDir("", nil, sk); got != neutral {
		t.Fatalf("fallback dir: got %q want %q", got, neutral)
	}

	// 5. header 是客户端侧路径、但消息里能抽到真实存在的目录 → 用消息抽取的
	if got := s.sessionDir(`/nonexistent-on-janus`, msgs, sk); got != exists {
		t.Fatalf("header missing + msg found: got %q want %q", got, exists)
	}
}
// ---------- 出向路径改写 ----------

// TestPathRewriter 覆盖：完整替换、多匹配、不同 scope 不误伤、跨 delta 切断、终态 flush。
func TestPathRewriter(t *testing.T) {
	from := "/var/lib/janus/workspaces/9a1138b442f6519"
	to := `D:\project\tvfusion`
	rw := newPathRewriter(from, to)
	if rw == nil {
		t.Fatal("rewriter should be created")
	}

	// 完整路径 + 后缀，一次改写（含多处）
	want := "看 " + `D:\project\tvfusion` + "/extract_routes.py 和 " + `D:\project\tvfusion` + "/gen.py"
	if got := rw.rewrite("看 " + from + "/extract_routes.py 和 " + from + "/gen.py"); got != want {
		t.Fatalf("full rewrite: got %q want %q", got, want)
	}

	// 不同 scope 的路径不能误伤
	other := "/var/lib/janus/workspaces/ffffffffffffffff/x"
	if got := rw.rewrite(other); got != other {
		t.Fatalf("other scope must not be rewritten: %q", got)
	}

	// 跨 delta：路径被切成两半，第一段先扣住、拼全后才放行
	rw2 := newPathRewriter(from, to)
	if got := rw2.rewrite("cwd is /var/lib/janus/wor"); got != "cwd is " {
		t.Fatalf("held prefix should not be emitted: %q", got)
	}
	if got := rw2.rewrite("kspaces/9a1138b442f6519/file.py done"); got != `D:\project\tvfusion`+"/file.py done" {
		t.Fatalf("split rewrite: %q", got)
	}

	// 扣住后路径没拼全（换话题）：flush 原样补发，且幂等
	rw3 := newPathRewriter(from, to)
	if got := rw3.rewrite("see /var/lib/janus/work"); got != "see " {
		t.Fatalf("rw3 emit: %q", got)
	}
	if got := rw3.flush(); got != "/var/lib/janus/work" {
		t.Fatalf("flush should emit held tail: %q", got)
	}
	if got := rw3.flush(); got != "" {
		t.Fatalf("flush must be idempotent: %q", got)
	}

	// 扣住与完整匹配并存
	rw4 := newPathRewriter(from, to)
	if got := rw4.rewrite("a " + from + " b /var/lib/janus/work"); got != "a "+to+" b " {
		t.Fatalf("mixed: %q", got)
	}
	if got := rw4.flush(); got != "/var/lib/janus/work" {
		t.Fatalf("mixed flush: %q", got)
	}

	// 规则不成立时不建改写器（同机部署）
	if newPathRewriter("/same", "/same") != nil {
		t.Fatal("identical from/to should disable rewriting")
	}
	if newPathRewriter("", "/x") != nil || newPathRewriter("/x", "") != nil {
		t.Fatal("empty side should disable rewriting")
	}
}

// TestSetPathRewrite 验证改写规则的成立条件（只在 workspaces 兜底目录 + 有客户端路径时）。
func TestSetPathRewrite(t *testing.T) {
	ws := t.TempDir()
	s := &Server{cfg: Config{WorkspacesDir: ws, Directory: "/deploy"}}
	sk := scopeKey("testide", "testproj")
	neutral := filepath.Join(ws, strings.TrimPrefix(sk, scopeKeyPrefix))

	// 同机部署：会话目录就是客户端目录（不在 workspaces 下）→ 无规则
	conv := &Conversation{}
	s.setPathRewrite(conv, "/home/dev/proj", "/home/dev/proj", nil)
	if conv.rewriteFrom != "" || conv.rewriteTo != "" {
		t.Fatal("same-host session must not create rewrite rule")
	}

	// 远程 mode B + header 声明了客户端路径 → 规则成立（保留客户端路径原样）
	s.setPathRewrite(conv, neutral, `C:\Users\dev\proj`, nil)
	if conv.rewriteFrom != neutral || conv.rewriteTo != `C:\Users\dev\proj` {
		t.Fatalf("header rule: from=%q to=%q", conv.rewriteFrom, conv.rewriteTo)
	}

	// 无 header，消息里有 Primary working directory → 用消息里的项目根
	msgs := []ChatMessage{{Role: "user", Content: MessageContent{Text: "Primary working directory: /home/u/app"}}}
	s.setPathRewrite(conv, neutral, "", msgs)
	if conv.rewriteTo != "/home/u/app" {
		t.Fatalf("msg rule: to=%q", conv.rewriteTo)
	}

	// 啥都抽不到 → 不建规则（续链请求无项目信息时由调用方沿用既有规则）
	conv2 := &Conversation{}
	s.setPathRewrite(conv2, neutral, "", nil)
	if conv2.rewriteFrom != "" {
		t.Fatal("no client info must not create rule")
	}
}

// TestExecutorRewrite 验证 delta 流经 executor 时被改写、serverText 保持上游原文
//（对账基准不受扰动）、终态 snapshot 补发扣住尾部且流式顺序正确。
func TestExecutorRewrite(t *testing.T) {
	from := "/var/lib/janus/workspaces/9a1138b442f6519"
	to := "/home/dev/proj"
	ex := newExecutor(&Server{log: NewLogger("error")}, "s", "m", false)
	ex.setRewrite(from, to)
	var streamed []string
	ex.writeText = func(s string) error { streamed = append(streamed, s); return nil }

	_ = ex.addText("工作区在 ")
	_ = ex.addText(from[:20]) // 半截路径：先扣住，不下发
	_ = ex.addText(from[20:] + " 下没有文件\n")
	_ = ex.addText("完事")

	res := ex.snapshot()
	want := "工作区在 " + to + " 下没有文件\n完事"
	if res.text != want {
		t.Fatalf("text = %q, want %q", res.text, want)
	}
	if got := strings.Join(streamed, ""); got != want {
		t.Fatalf("streamed = %q, want %q", got, want)
	}
	if raw := ex.serverText.String(); strings.Contains(raw, to) || !strings.Contains(raw, from) {
		t.Fatalf("serverText must keep upstream raw text: %q", raw)
	}
}

// TestToolSessionRewriteArgs 验证客户端工具参数的工作区路径改写：
// 路径被替换为 JSON 转义后的客户端路径，整段 args 保持合法 JSON；无规则时原样返回。
func TestToolSessionRewriteArgs(t *testing.T) {
	from := "/var/lib/janus/workspaces/9a1138b442f6519"
	sess := &toolSession{}
	sess.setRewritePaths(from, `D:\work\alpha`)

	args := `{"command":"ls","args":["` + from + `/notes","/etc"]}`
	got := sess.rewriteArgs(args)
	want := `{"command":"ls","args":["D:\\work\\alpha/notes","/etc"]}`
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	// 改写后必须是合法 JSON，且还原回真实客户端路径
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("rewritten args is not valid JSON: %v (%s)", err, got)
	}
	if lst := m["args"].([]any); lst[0] != `D:\work\alpha/notes` {
		t.Fatalf("decoded path = %v", lst[0])
	}

	// 无规则 → 原样
	plain := &toolSession{}
	if got := plain.rewriteArgs(args); got != args {
		t.Fatalf("no-rule args must be unchanged: %q", got)
	}
}

// TestJsonStringValue 验证任意路径可安全嵌入 JSON 文本。
func TestJsonStringValue(t *testing.T) {
	if got := jsonStringValue(`D:\work\alpha`); got != `D:\\work\\alpha` {
		t.Fatalf("windows path: %q", got)
	}
	if got := jsonStringValue("/var/lib/x"); got != "/var/lib/x" {
		t.Fatalf("plain path: %q", got)
	}
	// 嵌回 JSON 后能无损还原
	var s string
	if err := json.Unmarshal([]byte(`"`+jsonStringValue("a\"b\\c")+`"`), &s); err != nil || s != "a\"b\\c" {
		t.Fatalf("roundtrip: %q %v", s, err)
	}
}

// TestPathMappingNote 验证首轮注入条件与内容。
func TestPathMappingNote(t *testing.T) {
	conv := &Conversation{rewriteFrom: "/var/lib/janus/workspaces/abc", rewriteTo: "/home/u/proj"}
	s := &Server{}

	if got := s.pathMappingNote(conv, false); got != "" {
		t.Fatalf("non-first turn must not inject: %q", got)
	}
	got := s.pathMappingNote(conv, true)
	if !strings.Contains(got, conv.rewriteFrom) || !strings.Contains(got, conv.rewriteTo) {
		t.Fatalf("note must mention both paths: %q", got)
	}

	if got := s.pathMappingNote(&Conversation{}, true); got != "" {
		t.Fatalf("no rule must not inject: %q", got)
	}
}
