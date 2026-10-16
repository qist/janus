package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// save 落盘：文件内容 = 原始字节，文件名含首 callID。
func TestForensicsSaveAndTrace(t *testing.T) {
	dir := t.TempDir()
	f := newForensics(dir, 5, NewLogger("error"))
	if f == nil {
		t.Fatal("应启用")
	}

	data := []byte(`data: {"chunk":1}

data: {"chunk":"call_abc123"}
`)
	p := f.save("s:test-conv", "ses_x", []string{"call_abc123", "call_def456"}, data)
	if p == "" {
		t.Fatal("save 返回空")
	}
	if !strings.Contains(filepath.Base(p), "call_abc123") {
		t.Fatalf("文件名应含首个 callID: %q", p)
	}
	onDisk, err := os.ReadFile(p)
	if err != nil || string(onDisk) != string(data) {
		t.Fatalf("落盘内容不符: %v %q", err, onDisk)
	}

	// trace 按 callID 内容找回
	f.trace("call_abc123") // 仅验证不 panic；匹配日志走 Logger（error 级不可见）
	f.trace("call_nope")   // 未命中走 no-match 分支
}

// prune：超出保留份数时按文件名时间戳淘汰最旧。
func TestForensicsPrune(t *testing.T) {
	dir := t.TempDir()
	f := newForensics(dir, 3, NewLogger("error"))
	for i := 1; i <= 5; i++ {
		if p := f.save("s:k", "ses", []string{"call_x"}, []byte{byte('0' + i)}); p == "" {
			t.Fatal("save 失败")
		}
		// 文件名毫秒时间戳可能重复，手动改名保证顺序递增
		ents, _ := os.ReadDir(filepath.Join(dir, "s-k"))
		if n := len(ents); n > 0 {
			last := ents[n-1].Name()
			os.Rename(filepath.Join(dir, "s-k", last), filepath.Join(dir, "s-k", fmt.Sprintf("%010d-call_x.sse", i)))
		}
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "s-k"))
	if len(ents) != 3 {
		t.Fatalf("应保留 3 份，实际 %d", len(ents))
	}
	// 最旧的两份应被淘汰：保留的是 3/4/5
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "0000000001") || strings.HasPrefix(e.Name(), "0000000002") {
			t.Fatalf("最旧文件未被淘汰: %s", e.Name())
		}
	}
}

// nil 取证器必须全程 no-op（未开启 BRIDGE_WEDGE_FORENSICS 的部署）。
func TestForensicsNilNoop(t *testing.T) {
	var f *Forensics
	f.save("s:k", "ses", []string{"call_x"}, []byte("data"))
	f.trace("call_x") // 不 panic 即可
}

// sseWriter 捕获：capOn 时 writeChunk/toolCalls/finish/done 全部留底。
func TestSSEWriterCapture(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	sw.capOn = true
	_ = sw.begin()
	_ = sw.toolCalls([]*pendingCall{{
		CallID:   "call_cap1",
		ToolName: "Read",
		MCPName:  "ob_s_x_Read",
		Args:     `{"p":1}`,
	}})
	_ = sw.finish("tool_calls", nil, false)

	cap := sw.captureBytes()
	if cap == nil {
		t.Fatal("capOn 时应有捕获")
	}
	for _, want := range []string{`"role":"assistant"`, "call_cap1", `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(string(cap), want) {
			t.Fatalf("捕获缺 %q:\n%s", want, cap)
		}
	}
	if !strings.HasPrefix(string(cap), "data: ") {
		t.Fatalf("捕获应以 data: 开头:\n%s", cap)
	}

	// 未开启时捕获为 nil
	rec2 := httptest.NewRecorder()
	sw2, _ := newSSE(rec2, "m")
	sw2.capOn = false
	_ = sw2.begin()
	if sw2.captureBytes() != nil {
		t.Fatal("capOn=false 不应有捕获")
	}
}
