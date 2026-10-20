package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 测试脚手架 ----------

// fakeBin 写一个可执行 shell 脚本充当 CLI harness。
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-harness")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// collect 收齐事件直到通道关闭（或超时失败）。
func collect(t *testing.T, ch <-chan Event, limit time.Duration) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(limit)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-deadline:
			t.Fatalf("等事件超时，已收到 %+v", out)
		}
	}
}

type fakeLine struct {
	SID  string `json:"sid"`
	Text string `json:"text"`
}

func fakeParse(line []byte) ([]Event, string) {
	var l fakeLine
	if json.Unmarshal(line, &l) != nil {
		return nil, "" // 非 JSON 行忽略
	}
	var evs []Event
	if l.Text != "" {
		evs = append(evs, Event{Type: EventText, Text: l.Text})
	}
	return evs, l.SID
}

func newFakeRuntime(t *testing.T, bin string) *CLIRuntime {
	t.Helper()
	return NewCLIRuntime(CLISpec{
		Name:    "fake",
		Bin:     bin,
		ArgsNew: func(cfg SessionConfig, prompt string) []string { return []string{prompt} },
		Parse:   fakeParse,
	})
}

// ---------- 基础链路 ----------

func TestCLIRuntimeStreamsAndLearnsSessionID(t *testing.T) {
	bin := fakeBin(t, `
printf '%s\n' '{"sid":"S1"}'
printf '%s\n' '{"text":"hello"}'
printf '%s\n' '{"text":" world"}'
`)
	rt := newFakeRuntime(t, bin)
	sid, err := rt.CreateSession(context.Background(), SessionSpec{Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "k1" {
		t.Fatalf("sid = %q, want k1（应沿用逻辑键）", sid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := rt.Prompt(ctx, sid, PromptReq{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch, 5*time.Second)

	if len(evs) != 3 {
		t.Fatalf("事件数 = %d (%+v)，想要 2 段正文 + done", len(evs), evs)
	}
	if evs[0].Type != EventText || evs[0].Text != "hello" || evs[1].Text != " world" {
		t.Fatalf("正文事件不对: %+v", evs[:2])
	}
	if last := evs[2]; last.Type != EventDone || last.Finish != "stop" {
		t.Fatalf("收尾事件 = %+v，想要 done/stop", last)
	}
	if got := rt.HarnessID(sid); got != "S1" {
		t.Fatalf("harness 会话 id = %q, want S1", got)
	}
}

// 会话键相同 → 第二轮走 resume，且带上首轮学到的 harness 会话 id。
func TestCLIRuntimeResumesAfterFirstTurn(t *testing.T) {
	bin := fakeBin(t, `printf '%s\n' '{"sid":"S1"}'`)

	var newCalls, resumeCalls [][]string
	rt := NewCLIRuntime(CLISpec{
		Name: "fake",
		Bin:  bin,
		ArgsNew: func(cfg SessionConfig, prompt string) []string {
			newCalls = append(newCalls, []string{prompt})
			return []string{prompt}
		},
		ArgsResume: func(cfg SessionConfig, harnessID, prompt string) []string {
			resumeCalls = append(resumeCalls, []string{harnessID, prompt})
			return []string{harnessID, prompt}
		},
		Parse: fakeParse,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	run := func(prompt string) {
		t.Helper()
		sid, err := rt.CreateSession(ctx, SessionSpec{Key: "same"})
		if err != nil {
			t.Fatal(err)
		}
		ch, err := rt.Prompt(ctx, sid, PromptReq{Messages: []Message{{Role: "user", Content: prompt}}})
		if err != nil {
			t.Fatal(err)
		}
		collect(t, ch, 5*time.Second)
	}
	run("first")
	run("second")

	if len(newCalls) != 1 || newCalls[0][0] != "first" {
		t.Fatalf("首轮应走 ArgsNew(first)，实际 %v", newCalls)
	}
	if len(resumeCalls) != 1 || resumeCalls[0][0] != "S1" || resumeCalls[0][1] != "second" {
		t.Fatalf("次轮应走 ArgsResume(S1, second)，实际 %v", resumeCalls)
	}
}

// Interrupt 杀掉子进程：收尾应为 interrupted，且不报成执行错误。
func TestCLIRuntimeInterrupt(t *testing.T) {
	bin := fakeBin(t, `sleep 30`)
	rt := newFakeRuntime(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sid, err := rt.CreateSession(ctx, SessionSpec{Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := rt.Prompt(ctx, sid, PromptReq{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 等子进程起来
	if err := rt.Interrupt(ctx, sid); err != nil {
		t.Fatal(err)
	}

	evs := collect(t, ch, 5*time.Second)
	if len(evs) == 0 {
		t.Fatal("没有事件")
	}
	last := evs[len(evs)-1]
	if last.Type != EventDone || last.Finish != "interrupted" {
		t.Fatalf("收尾 = %+v，想要 done/interrupted（evs=%+v）", last, evs)
	}
	for _, e := range evs {
		if e.Type == EventError {
			t.Fatalf("打断不应报错: %+v", e)
		}
	}
}

// 非零退出：把 stderr 带回来，收尾标 error。
func TestCLIRuntimeReportsStderrOnFailure(t *testing.T) {
	bin := fakeBin(t, `echo "boom: unknown flag" >&2; exit 2`)
	rt := newFakeRuntime(t, bin)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, _ := rt.CreateSession(ctx, SessionSpec{Key: "k"})
	ch, err := rt.Prompt(ctx, sid, PromptReq{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch, 5*time.Second)

	if len(evs) != 2 || evs[0].Type != EventError || !strings.Contains(evs[0].Error, "boom: unknown flag") {
		t.Fatalf("期望一条 error 事件带 stderr，实际 %+v", evs)
	}
	if evs[1].Type != EventDone || evs[1].Finish != "error" {
		t.Fatalf("收尾 = %+v，想要 done/error", evs[1])
	}
}

// ---------- 参数与错误处理 ----------

func TestCLIRuntimeArgErrors(t *testing.T) {
	bin := fakeBin(t, `true`)
	rt := newFakeRuntime(t, bin)
	ctx := context.Background()

	if _, err := rt.Prompt(ctx, "nope", PromptReq{Messages: []Message{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("未知会话应报错")
	}
	sid, _ := rt.CreateSession(ctx, SessionSpec{Key: "k"})
	if _, err := rt.Prompt(ctx, sid, PromptReq{}); err == nil {
		t.Fatal("空 prompt 应报错")
	}
	if err := rt.SetModel(ctx, "nope", "m"); err == nil {
		t.Fatal("未知会话 SetModel 应报错")
	}
	if err := rt.Interrupt(ctx, "nope"); err != nil {
		t.Fatalf("对未知会话 Interrupt 应为 no-op，得到 %v", err)
	}
}

func TestCLIRuntimeEnsureReady(t *testing.T) {
	if err := NewCLIRuntime(CLISpec{Name: "x", Bin: ""}).EnsureReady(context.Background()); err == nil {
		t.Fatal("空 bin 应报错")
	}
	rt := newFakeRuntime(t, filepath.Join(t.TempDir(), "definitely-missing"))
	if err := rt.EnsureReady(context.Background()); err == nil {
		t.Fatal("不存在的 bin 应报错")
	}
	good := newFakeRuntime(t, fakeBin(t, `true`))
	if err := good.EnsureReady(context.Background()); err != nil {
		t.Fatalf("存在的 bin 不应报错: %v", err)
	}
}

// Env 返回的变量应出现在子进程环境里（用 sh 打印回来验证）。
func TestCLIRuntimeInjectsEnv(t *testing.T) {
	bin := fakeBin(t, `printf '{"text":"%s"}\n' "$JANUS_TEST_VAR"`)
	rt := NewCLIRuntime(CLISpec{
		Name:    "fake",
		Bin:     bin,
		ArgsNew: func(cfg SessionConfig, prompt string) []string { return []string{prompt} },
		Env:     func(cfg SessionConfig) []string { return []string{"JANUS_TEST_VAR=injected"} },
		Parse:   fakeParse,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, _ := rt.CreateSession(ctx, SessionSpec{Key: "k"})
	ch, err := rt.Prompt(ctx, sid, PromptReq{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch, 5*time.Second)
	if len(evs) == 0 || evs[0].Text != "injected" {
		t.Fatalf("环境变量未注入: %+v", evs)
	}
}

// 任意环境变量不应影响逻辑键为空的默认会话 id 生成。
func TestCLIRuntimeGeneratesSessionKey(t *testing.T) {
	rt := newFakeRuntime(t, fakeBin(t, `true`))
	sid, err := rt.CreateSession(context.Background(), SessionSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sid, "fake-") {
		t.Fatalf("生成的 sid = %q，想要 fake- 前缀", sid)
	}
}

func TestLastUserText(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}
	if got := LastUserText(msgs); got != "c" {
		t.Fatalf("LastUserText = %q, want c", got)
	}
	if got := LastUserText([]Message{{Role: "assistant", Content: "only"}}); got != "only" {
		t.Fatalf("退到末条 = %q, want only", got)
	}
	if got := LastUserText(nil); got != "" {
		t.Fatalf("空历史 = %q, want 空", got)
	}
}
