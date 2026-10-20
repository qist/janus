package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/logx"
)

// fakeCodex 写一个充当 codex CLI 的 shell 脚本：把它收到的 argv 追加到 argvLog，
// 然后吐一段真实的 JSONL 事件（thread.started + agent_message + turn.completed）。
func fakeCodex(t *testing.T, argvLog string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	body := "#!/bin/sh\n" +
		`printf '%s\n' "$@" >> ` + argvLog + "\n" +
		`printf '%s\n' '{"type":"thread.started","thread_id":"th-1"}'` + "\n" +
		`printf '%s\n' '{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"hello from codex"}}'` + "\n" +
		`printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":2}}'` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newCLIServer(t *testing.T, bin string) *Server {
	t.Helper()
	return &Server{
		cfg: config.Config{
			MaxBodyBytes:   1 << 20,
			RequestTimeout: 10 * time.Second,
			CodexBin:       bin,
			ClaudeBin:      bin,
		},
		log: logx.NewLogger("error"),
	}
}

func runtimePromptReq(body string, pathValue string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runtimes/codex/prompt", strings.NewReader(body))
	r.SetPathValue("name", pathValue)
	return r
}

func TestRuntimePromptNonStream(t *testing.T) {
	srv := newCLIServer(t, fakeCodex(t, filepath.Join(t.TempDir(), "argv.log")))
	rec := httptest.NewRecorder()
	srv.handleRuntimePrompt(rec, runtimePromptReq(`{"prompt":"hi","session_key":"k1","stream":false}`, "codex"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runtime   string           `json:"runtime"`
		SessionID string           `json:"session_id"`
		Finish    string           `json:"finish"`
		Events    []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if resp.Runtime != "codex" || resp.SessionID != "k1" || resp.Finish != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
	// text + usage；done 不计入 events
	if len(resp.Events) != 2 {
		t.Fatalf("events = %+v", resp.Events)
	}
	if resp.Events[0]["type"] != "text" || resp.Events[0]["text"] != "hello from codex" {
		t.Fatalf("events[0] = %+v", resp.Events[0])
	}
	if resp.Events[1]["type"] != "usage" {
		t.Fatalf("events[1] = %+v", resp.Events[1])
	}
	if u, ok := resp.Events[1]["usage"].(map[string]any); !ok || u["input_tokens"] != float64(5) {
		t.Fatalf("usage = %+v", resp.Events[1]["usage"])
	}
}

func TestRuntimePromptStream(t *testing.T) {
	srv := newCLIServer(t, fakeCodex(t, filepath.Join(t.TempDir(), "argv.log")))
	rec := httptest.NewRecorder()
	srv.handleRuntimePrompt(rec, runtimePromptReq(`{"prompt":"hi","stream":true}`, "codex"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"text":"hello from codex"`) {
		t.Fatalf("SSE 里没有正文事件:\n%s", body)
	}
	if !strings.Contains(body, `"type":"done"`) {
		t.Fatalf("SSE 里没有 done:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("SSE 未以 [DONE] 收尾:\n%s", body)
	}
}

// 复用同一 session_key → 第二轮应带上 codex 的 resume <thread_id>。
func TestRuntimePromptResumesSession(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	srv := newCLIServer(t, fakeCodex(t, argvLog))

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		srv.handleRuntimePrompt(rec, runtimePromptReq(`{"prompt":"turn","session_key":"same"}`, "codex"))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 轮 status = %d, body = %s", i+1, rec.Code, rec.Body.String())
		}
	}

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	invocations := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// 两轮各把 argv 逐行写入；按 "exec" 切分统计。
	joined := strings.Join(invocations, "\n")
	if strings.Count(joined, "\nresume\n") != 1 {
		t.Fatalf("应恰好一轮使用 resume，argv 日志：\n%s", joined)
	}
	if !strings.Contains(joined, "th-1") {
		t.Fatalf("resume 未带上 thread_id：\n%s", joined)
	}
}

func TestRuntimePromptErrors(t *testing.T) {
	srv := newCLIServer(t, fakeCodex(t, filepath.Join(t.TempDir(), "argv.log")))

	rec := httptest.NewRecorder()
	srv.handleRuntimePrompt(rec, runtimePromptReq(`{"prompt":"hi"}`, "nope"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 runtime 应 404，得到 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.handleRuntimePrompt(rec, runtimePromptReq(`{}`, "codex"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 prompt 应 400，得到 %d", rec.Code)
	}

	// 可执行文件不存在 → 503（而不是 500/panic）
	missing := &Server{cfg: config.Config{MaxBodyBytes: 1 << 20, RequestTimeout: time.Second, CodexBin: filepath.Join(t.TempDir(), "nope")}, log: logx.NewLogger("error")}
	rec = httptest.NewRecorder()
	missing.handleRuntimePrompt(rec, runtimePromptReq(`{"prompt":"hi"}`, "codex"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("二进制缺失应 503，得到 %d (%s)", rec.Code, rec.Body.String())
	}
}

// 注册表要包含 CLI 型 runtime，且 /api/v1/runtimes 能看到。
func TestListRuntimesIncludesCLI(t *testing.T) {
	srv := newCLIServer(t, "/bin/true")

	names := srv.agentRegistry().Names()
	for _, want := range []string{"codex", "claude-code", "opencode", "direct"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("runtime %q 未注册（names=%v）", want, names)
		}
	}

	rec := httptest.NewRecorder()
	srv.handleListRuntimes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runtimes", nil))
	var resp struct {
		Runtimes []struct {
			Name string `json:"name"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Runtimes) != 4 {
		t.Fatalf("runtimes = %+v", resp.Runtimes)
	}
}
