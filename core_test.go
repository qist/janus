package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func m(role, text string) ChatMessage {
	return ChatMessage{Role: role, Content: MessageContent{Text: text}}
}

// ---------- Diff ----------

func TestDiffAppend(t *testing.T) {
	stored := []ChatMessage{m("system", "S"), m("user", "u1")}
	incoming := []ChatMessage{m("system", "S"), m("user", "u1"), m("assistant", "a1"), m("user", "u2")}

	mode, delta := Diff(stored, incoming)
	if mode != DiffAppend {
		t.Fatalf("mode = %v, want DiffAppend", mode)
	}
	if len(delta) != 2 || delta[0].Role != "assistant" || delta[1].Content.Text != "u2" {
		t.Fatalf("delta = %+v", delta)
	}
}

func TestDiffNone(t *testing.T) {
	msgs := []ChatMessage{m("system", "S"), m("user", "u1")}
	mode, delta := Diff(msgs, msgs)
	if mode != DiffNone {
		t.Fatalf("mode = %v, want DiffNone", mode)
	}
	if len(delta) != 0 {
		t.Fatalf("delta should be empty, got %d", len(delta))
	}
}

// 完全不同的首条：必须重开，不能把新话题接在旧上下文后面。
func TestDiffResetOnNewTopic(t *testing.T) {
	stored := []ChatMessage{m("system", "S"), m("user", "old topic about apples")}
	incoming := []ChatMessage{m("system", "S"), m("user", "brand new topic about goroutines")}

	mode, _ := Diff(stored, incoming)
	if mode != DiffReset {
		t.Fatalf("mode = %v, want DiffReset", mode)
	}
}

// 客户端删减了历史（incoming 是 stored 的前缀）：无法只发增量，必须重开。
func TestDiffResetOnTruncatedHistory(t *testing.T) {
	stored := []ChatMessage{m("system", "S"), m("user", "u1"), m("assistant", "a1")}
	incoming := []ChatMessage{m("system", "S")}

	mode, _ := Diff(stored, incoming)
	if mode != DiffReset {
		t.Fatalf("mode = %v, want DiffReset", mode)
	}
}

func TestDiffResetWhenStoredEmpty(t *testing.T) {
	incoming := []ChatMessage{m("user", "hello")}
	mode, delta := Diff(nil, incoming)
	if mode != DiffReset || len(delta) != 1 {
		t.Fatalf("mode=%v len=%d", mode, len(delta))
	}
}

func TestDiffEmptyIncoming(t *testing.T) {
	mode, _ := Diff([]ChatMessage{m("user", "x")}, nil)
	if mode != DiffNone {
		t.Fatalf("mode = %v, want DiffNone", mode)
	}
}

// system 变了就算历史其余一致也要重开：指纹 key 依赖 system。
func TestDiffResetOnSystemChange(t *testing.T) {
	stored := []ChatMessage{m("system", "A"), m("user", "u1")}
	incoming := []ChatMessage{m("system", "B"), m("user", "u1")}

	mode, _ := Diff(stored, incoming)
	if mode != DiffReset {
		t.Fatalf("mode = %v, want DiffReset", mode)
	}
}

func TestDiffIgnoresToolCallIDNoise(t *testing.T) {
	a := ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "call_1", Type: "function", Function: &FunctionCall{Name: "read", Arguments: `{"p":"/x"}`}},
	}}
	b := ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "call_9999", Type: "function", Function: &FunctionCall{Name: "read", Arguments: `{"p":"/x"}`}},
	}}
	stored := []ChatMessage{m("system", "S"), a}
	incoming := []ChatMessage{m("system", "S"), b, m("user", "u2")}

	mode, delta := Diff(stored, incoming)
	if mode != DiffAppend || len(delta) != 1 {
		t.Fatalf("mode=%v delta=%+v (ID 差异不该导致重开)", mode, delta)
	}
}

// ---------- Flatten ----------

func TestFlattenRoles(t *testing.T) {
	got := Flatten([]ChatMessage{
		m("system", "be brief"),
		m("user", "hi"),
		m("assistant", "hello"),
		m("tool", "file content"),
	})
	for _, want := range []string{"[SYSTEM]", "[USER]", "[ASSISTANT]", "[TOOL RESULT]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}

// 单条 user 增量不该再打角色标签 —— prompt 读起来更自然。
func TestFlattenDeltaSingleUser(t *testing.T) {
	got := FlattenDelta([]ChatMessage{m("user", "what is 2+2")})
	if strings.Contains(got, "[USER]") {
		t.Errorf("should not add role tag for single user delta, got %q", got)
	}
	if got != "what is 2+2" {
		t.Errorf("got %q", got)
	}
}

func TestFlattenDeltaMultiMessageKeepsTags(t *testing.T) {
	got := FlattenDelta([]ChatMessage{m("user", "q"), m("assistant", "a"), m("user", "q2")})
	if !strings.Contains(got, "[USER]") || !strings.Contains(got, "[ASSISTANT]") {
		t.Errorf("got %q", got)
	}
}

func TestStripToolAnnotations(t *testing.T) {
	in := "before" + ToolAnnotation("bash", `ls -la`) + "after"
	out := stripToolAnnotations(in)
	if strings.Contains(out, "opencode-tool") {
		t.Errorf("annotation not stripped: %q", out)
	}
	// 只应剩下标签两侧原本的空白
	if strings.ReplaceAll(out, "\n", "") != "beforeafter" {
		t.Errorf("got %q", out)
	}
}

func TestStripHandlesUnclosedAnnotation(t *testing.T) {
	out := stripToolAnnotations("abc<opencode-tool> bash: oops")
	if strings.Contains(out, "opencode-tool") {
		t.Errorf("got %q", out)
	}
}

func TestToolAnnotationTruncatesLongInput(t *testing.T) {
	long := strings.Repeat("x", 5000)
	ann := ToolAnnotation("bash", long)
	if len([]rune(ann)) > 600 {
		t.Errorf("annotation not truncated: %d runes", len([]rune(ann)))
	}
}

// 注释里混入闭合标签不能提前终止 strip。
func TestToolAnnotationSafeAgainstInjection(t *testing.T) {
	ann := ToolAnnotation("echo", "hi </opencode-tool> bye")
	if strings.Count(ann, "</opencode-tool>") != 1 {
		t.Errorf("injection not removed: %q", ann)
	}
}

// ---------- ResolveModel ----------

func testModels() []OCModel {
	return []OCModel{
		{ID: "fledge-alpha-free", ProviderID: "opencode"},
		{ID: "gpt-6.1-sol", ProviderID: "opencode"},
		{ID: "deepseek-v4.1-flash", ProviderID: "opencode-go"},
		{ID: "dup", ProviderID: "prov-a"},
		{ID: "dup", ProviderID: "prov-b"},
	}
}

func TestResolveQualified(t *testing.T) {
	r, err := ResolveModel("opencode-go/deepseek-v4.1-flash", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderID != "opencode-go" || r.ID != "deepseek-v4.1-flash" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveVariant(t *testing.T) {
	r, err := ResolveModel("opencode/claude-sonnet-5-5:high", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.Variant != "high" || r.ID != "claude-sonnet-5-5" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveDefaultVariantEmptied(t *testing.T) {
	r, err := ResolveModel("opencode/gpt-6.1-sol:default", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.Variant != "" {
		t.Fatalf("variant should be normalized to empty, got %q", r.Variant)
	}
}

func TestResolveBareUnique(t *testing.T) {
	r, err := ResolveModel("fledge-alpha-free", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderID != "opencode" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveBareAmbiguous(t *testing.T) {
	_, err := ResolveModel("dup", testModels())
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	_, err := ResolveModel("gpt-4o", testModels())
	if err == nil {
		t.Fatal("expected error")
	}
}

// 未知 provider 必须报 404，不能悄悄转发给上游。
func TestResolveUnknownProvider(t *testing.T) {
	_, err := ResolveModel("nonexistent/gpt-4o", testModels())
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("got %v", err)
	}
}

// 已知 provider 下的未知 model 仍放行，让上游报真实原因。
func TestResolveKnownProviderUnknownModel(t *testing.T) {
	r, err := ResolveModel("opencode/ghost-model", testModels())
	if err != nil {
		t.Fatalf("should pass through: %v", err)
	}
	if r.ID != "ghost-model" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveEmptyModel(t *testing.T) {
	if _, err := ResolveModel("", testModels()); err == nil {
		t.Fatal("expected error")
	}
}

// ---------- ConversationKey ----------

func TestConversationKeyExplicitWins(t *testing.T) {
	k := ConversationKey("my-id", "system", "u", "/dir")
	if k != "x:my-id" {
		t.Fatalf("got %q", k)
	}
}

func TestConversationKeyStable(t *testing.T) {
	a := ConversationKey("", "system prompt", "", "/tmp/proj")
	b := ConversationKey("", "system prompt", "", "/tmp/proj")
	if a != b {
		t.Fatal("same inputs must yield same key")
	}
}

func TestConversationKeyDiffersBySystem(t *testing.T) {
	a := ConversationKey("", "sys A", "", "/tmp/proj")
	b := ConversationKey("", "sys B", "", "/tmp/proj")
	if a == b {
		t.Fatal("different system prompts must not collide")
	}
}

func TestConversationKeyDiffersByDir(t *testing.T) {
	a := ConversationKey("", "sys", "", "/tmp/proj")
	b := ConversationKey("", "sys", "", "/root")
	if a == b {
		t.Fatal("different directories must not collide")
	}
}

// ---------- MessageContent 多形态 ----------

func TestContentStringForm(t *testing.T) {
	var c MessageContent
	if err := c.UnmarshalJSON([]byte(`"hello"`)); err != nil {
		t.Fatal(err)
	}
	if c.IsArray || c.Text != "hello" {
		t.Fatalf("%+v", c)
	}
}

func TestContentArrayForm(t *testing.T) {
	var c MessageContent
	raw := `[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]`
	if err := c.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if !c.IsArray || c.Text != "part1part2" {
		t.Fatalf("%+v", c)
	}
}

func TestContentNullForm(t *testing.T) {
	var c MessageContent
	if err := c.UnmarshalJSON([]byte(`null`)); err != nil {
		t.Fatal(err)
	}
	if c.Text != "" {
		t.Fatalf("%+v", c)
	}
}

func TestContentImages(t *testing.T) {
	var c MessageContent
	raw := `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]`
	if err := c.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	imgs := c.Images()
	if len(imgs) != 1 || !strings.HasPrefix(imgs[0], "data:image/png") {
		t.Fatalf("%v", imgs)
	}
}

// ---------- finish / usage 映射 ----------

func TestSetFinishMapping(t *testing.T) {
	cases := map[string]string{
		"stop":           "stop",
		"length":         "length",
		"tool-calls":     "tool_calls",
		"content-filter": "content_filter",
		"error":          "stop",
		"unknown":        "stop",
	}
	for in, want := range cases {
		e := newExecutor(nil, "sid", "m", false)
		e.setFinish(in)
		if got := e.snapshot().finish; got != want {
			t.Errorf("setFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetFinishEmptyStaysStop(t *testing.T) {
	e := newExecutor(nil, "sid", "m", false)
	if got := e.snapshot().finish; got != "stop" {
		t.Fatalf("got %q", got)
	}
}

func TestAddTokensAccumulates(t *testing.T) {
	e := newExecutor(nil, "sid", "m", false)
	tk := &OCTokens{Input: 100, Output: 50, Reason: 20}
	tk.Cache.Read = 30
	e.addTokens(tk)
	e.addTokens(tk)

	u := e.snapshot().usage
	// OpenAI 语义：completion_tokens 已包含 reasoning_tokens（后者是明细子项），
	// 所以这里只把 output 累进 completion，reasoning 单独记在 details 里。
	if u.PromptTokens != 200 || u.CompletionTokens != 100 {
		t.Fatalf("got %+v", u)
	}
	if u.TotalTokens != 300 {
		t.Fatalf("total = %d", u.TotalTokens)
	}
	if u.PromptTokensDetails.CachedTokens != 60 {
		t.Fatalf("cached = %d", u.PromptTokensDetails.CachedTokens)
	}
	if u.CompletionTokensDetails.ReasoningTokens != 40 {
		t.Fatalf("reasoning = %d", u.CompletionTokensDetails.ReasoningTokens)
	}
}

// ---------- reconcile 不越界 ----------

// 历史回归：工具注释让最终文本比上游纯文本长时，对账不能 panic。
func TestReconcileNoPanicWhenAnnotationsMakeTextLonger(t *testing.T) {
	e := newExecutor(&Server{log: NewLogger("error")}, "ses_x", "m", true)
	_ = e.addText("1+1=2")                          // 上游纯文本 5B
	_ = e.addToolText(ToolAnnotation("bash", "ls")) // 注释把 text 撑到 100B+

	// 不应 panic
	e.mu.Lock()
	got := e.text.String()
	server := e.serverText.String()
	e.mu.Unlock()

	if server != "1+1=2" {
		t.Fatalf("serverText = %q, want 1+1=2", server)
	}
	if len(got) <= len(server) {
		t.Fatalf("text should be longer than serverText due to annotations, got %d vs %d", len(got), len(server))
	}
}

// ---------- 附件 ----------

const redPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestFromDataURI(t *testing.T) {
	fa, err := fromDataURI("data:image/png;base64," + redPNG)
	if err != nil {
		t.Fatal(err)
	}
	if fa.Name != "image.png" {
		t.Errorf("name = %q", fa.Name)
	}
	if !strings.HasPrefix(fa.URI, "data:image/png;base64,") {
		t.Errorf("uri = %q", fa.URI)
	}
}

func TestFromDataURIMissingComma(t *testing.T) {
	if _, err := fromDataURI("data:image/png;base64"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFromDataURINotBase64(t *testing.T) {
	if _, err := fromDataURI("data:image/png,rawdata"); err == nil {
		t.Fatal("expected error for non-base64 payload")
	}
}

func TestFromDataURIBadBase64(t *testing.T) {
	if _, err := fromDataURI("data:image/png;base64,!!!not-base64!!!"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestExtractDataURIImages(t *testing.T) {
	msgs := []ChatMessage{{
		Role: "user",
		Content: MessageContent{
			IsArray: true,
			Text:    "what color",
			Parts: []ContentPart{
				{Type: "text", Text: "what color"},
				{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64," + redPNG}},
			},
		},
	}}
	files, failed := ExtractAttachments(context.Background(), msgs, nil)
	if len(files) != 1 || len(failed) != 0 {
		t.Fatalf("files=%d failed=%d", len(files), len(failed))
	}
}

// 上游实测只认 data:，其余 scheme 要进 failed 让调用方降级。
func TestExtractUnsupportedScheme(t *testing.T) {
	msgs := []ChatMessage{{
		Role: "user",
		Content: MessageContent{
			IsArray: true,
			Parts:   []ContentPart{{Type: "image_url", ImageURL: &ImageURL{URL: "file:///etc/passwd"}}},
		},
	}}
	files, failed := ExtractAttachments(context.Background(), msgs, nil)
	if len(files) != 0 || len(failed) != 1 {
		t.Fatalf("files=%d failed=%d", len(files), len(failed))
	}
}

func TestExtractIgnoresNonUserRoles(t *testing.T) {
	msgs := []ChatMessage{{
		Role: "assistant",
		Content: MessageContent{IsArray: true, Parts: []ContentPart{
			{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64," + redPNG}},
		}},
	}}
	files, _ := ExtractAttachments(context.Background(), msgs, nil)
	if len(files) != 0 {
		t.Fatalf("should only attach images from user messages, got %d", len(files))
	}
}

func TestExtractCapsCount(t *testing.T) {
	var msgs []ChatMessage
	for i := 0; i < maxAttachCount+5; i++ {
		msgs = append(msgs, ChatMessage{
			Role: "user",
			Content: MessageContent{IsArray: true, Parts: []ContentPart{
				{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64," + redPNG}},
			}},
		})
	}
	files, failed := ExtractAttachments(context.Background(), msgs, nil)
	if len(files) != maxAttachCount {
		t.Fatalf("files = %d, want %d", len(files), maxAttachCount)
	}
	if len(failed) != 5 {
		t.Fatalf("failed = %d, want 5", len(failed))
	}
}

func TestAttachFailureNote(t *testing.T) {
	if attachFailureNote(nil) != "" {
		t.Error("empty failed list should produce no note")
	}
	n := attachFailureNote([]string{"https://example.com/a.png"})
	if !strings.Contains(n, "未能附带") || !strings.Contains(n, "example.com") {
		t.Errorf("got %q", n)
	}
}

func TestMimeFromExt(t *testing.T) {
	cases := map[string]string{
		"https://x/y/a.jpg":  "image/jpeg",
		"https://x/y/a.jpeg": "image/jpeg",
		"https://x/y/a.gif":  "image/gif",
		"https://x/y/a.webp": "image/webp",
		"https://x/y/a.png":  "image/png",
	}
	for in, want := range cases {
		if got := mimeFromExt(in); got != want {
			t.Errorf("mimeFromExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtFor(t *testing.T) {
	if extFor("image/png") != ".png" || extFor("image/jpeg") != ".jpg" {
		t.Error("extFor mismatch")
	}
	if extFor("application/octet-stream") != "" {
		t.Error("unknown mime should give empty ext")
	}
}

// ---------- 分桶 + 前缀匹配 ----------

func newTestStore() *Store { return NewStore(NewLogger("error"), time.Minute, 64) }

// 指纹 key 相同的两个不相关话题必须拿到不同会话，不能互相挤掉。
func TestStoreSeparatesUnrelatedTopicsSameKey(t *testing.T) {
	s := newTestStore()
	key := "f:same"

	a := s.Acquire(key, []ChatMessage{m("user", "话题甲：苹果")})
	a.setLast([]ChatMessage{m("user", "话题甲：苹果")})
	a.setSessionID("ses_A")
	s.Release(a)

	b := s.Acquire(key, []ChatMessage{m("user", "话题乙：香蕉")})
	s.Release(b)

	if a.snapshotSessionID() != "ses_A" {
		t.Fatalf("话题甲的会话被改动了: %q", a.snapshotSessionID())
	}
	if b == a {
		t.Fatal("不相关话题不应共用会话")
	}
	if b.snapshotSessionID() != "" {
		t.Fatalf("新会话不该有 sessionID, got %q", b.snapshotSessionID())
	}
}

// 历史是前缀 → 复用同一会话（多轮的关键路径）。
func TestStoreReusesOnPrefixMatch(t *testing.T) {
	s := newTestStore()
	key := "f:same"

	h1 := []ChatMessage{m("user", "u1")}
	a := s.Acquire(key, h1)
	a.setLast(h1)
	a.setSessionID("ses_A")
	s.Release(a)

	h2 := []ChatMessage{m("user", "u1"), m("assistant", "a1"), m("user", "u2")}
	b := s.Acquire(key, h2)
	s.Release(b)

	if b != a {
		t.Fatal("前缀匹配应复用同一会话")
	}
}

// 多个候选时取最长前缀匹配。
func TestStorePicksLongestPrefix(t *testing.T) {
	s := newTestStore()
	key := "f:same"

	short := []ChatMessage{m("user", "hi")}
	a := s.Acquire(key, short)
	a.setLast(short)
	a.setSessionID("ses_short")
	s.Release(a)

	// 第二个话题在短历史之后分叉，且自身更长
	long := []ChatMessage{m("user", "hi"), m("assistant", "a"), m("user", "more")}
	b := s.Acquire(key, long)
	b.setLast(long)
	b.setSessionID("ses_long")
	s.Release(b)

	// 继续 long 的历史，应命中 ses_long 而非 ses_short
	next := append(append([]ChatMessage{}, long...), m("user", "even more"))
	c := s.Acquire(key, next)
	s.Release(c)

	if c.snapshotSessionID() != "ses_long" {
		t.Fatalf("应选最长前缀匹配, got %q", c.snapshotSessionID())
	}
}

// 客户端改写了历史（lcp < len(stored)）必须另起会话，不能污染旧上下文。
func TestStoreNewSessionOnRewrittenHistory(t *testing.T) {
	s := newTestStore()
	key := "f:same"

	h1 := []ChatMessage{m("user", "原始问题"), m("assistant", "原始回答")}
	a := s.Acquire(key, h1)
	a.setLast(h1)
	a.setSessionID("ses_A")
	s.Release(a)

	// 同样开头但 assistant 那条被改写了
	rewritten := []ChatMessage{m("user", "原始问题"), m("assistant", "被篡改的回答"), m("user", "继续")}
	b := s.Acquire(key, rewritten)
	s.Release(b)

	if b == a {
		t.Fatal("改写历史后不应复用旧会话")
	}
}

// incoming 比 stored 短（客户端截断了历史）也不该复用。
func TestStoreNewSessionOnTruncatedHistory(t *testing.T) {
	s := newTestStore()
	key := "f:same"

	h1 := []ChatMessage{m("system", "S"), m("user", "u1"), m("assistant", "a1")}
	a := s.Acquire(key, h1)
	a.setLast(h1)
	a.setSessionID("ses_A")
	s.Release(a)

	b := s.Acquire(key, []ChatMessage{m("system", "S")})
	s.Release(b)

	if b == a {
		t.Fatal("截断历史后不应复用旧会话")
	}
}

// 空 lastMessages 的会话（首 prompt 失败）可被复用。
func TestStoreReusesEmptyConversation(t *testing.T) {
	s := newTestStore()
	a := s.Acquire("f:k", []ChatMessage{m("user", "x")})
	s.Release(a)

	b := s.Acquire("f:k", []ChatMessage{m("user", "y")})
	s.Release(b)

	if a != b {
		t.Fatal("两个空会话应合并")
	}
}

// 单 key 候选数有上限，滑动窗口客户端不能无限新建。
func TestStoreCapsPerKey(t *testing.T) {
	s := newTestStore()
	key := "f:k"
	var last *Conversation
	for i := 0; i < 40; i++ {
		last = s.Acquire(key, []ChatMessage{m("user", "turn "+string(rune('a'+i%26))+" "+string(rune('0'+i/26)))})
		last.setLast([]ChatMessage{m("user", "never matches")}) // 制造永不匹配 → 持续新建
		s.Release(last)
	}
	s.mu.Lock()
	n := len(s.dir[key])
	s.mu.Unlock()
	if n > s.maxPerKey {
		t.Fatalf("bucket size = %d, cap = %d", n, s.maxPerKey)
	}
}

// 全局上限也要守住。
func TestStoreCapsGlobal(t *testing.T) {
	s := NewStore(NewLogger("error"), time.Minute, 4)
	for i := 0; i < 30; i++ {
		c := s.Acquire("k"+string(rune('a'+i)), []ChatMessage{m("user", "x")})
		c.setLast([]ChatMessage{m("user", "never")})
		s.Release(c)
	}
	s.mu.Lock()
	n := 0
	for _, b := range s.dir {
		n += len(b)
	}
	s.mu.Unlock()
	if n > 8 {
		t.Fatalf("total conversations = %d, want <= 8", n)
	}
}

// 被挤出的会话要进 orphans 通道，让 janitor 删上游 session。
func TestStoreEvictedSessionsGoToOrphans(t *testing.T) {
	s := NewStore(NewLogger("error"), time.Minute, 2)
	for i := 0; i < 10; i++ {
		c := s.Acquire("k"+string(rune('a'+i)), []ChatMessage{m("user", "x")})
		c.setSessionID("ses_" + string(rune('a'+i)))
		c.setLast([]ChatMessage{m("user", "never")})
		s.Release(c)
	}
	if len(s.orphans) == 0 {
		t.Fatal("evicted sessions should be queued for upstream deletion")
	}
}

func TestGCReturnsIdleSessions(t *testing.T) {
	s := NewStore(NewLogger("error"), 0, 8) // ttl=0 → 立即过期
	c := s.Acquire("f:k", []ChatMessage{m("user", "x")})
	c.setSessionID("ses_old")
	c.setLast([]ChatMessage{m("user", "x")})
	s.Release(c)

	dead := s.GC()
	if len(dead) != 1 || dead[0] != "ses_old" {
		t.Fatalf("dead = %v, want [ses_old]", dead)
	}
	s.mu.Lock()
	n := len(s.dir["f:k"])
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("bucket should be empty after GC, got %d", n)
	}
}

// 正在服务（mu 被持）的会话不能被 GC 掉。
func TestGCSkipsBusyConversation(t *testing.T) {
	s := NewStore(NewLogger("error"), 0, 8)
	c := s.Acquire("f:k", []ChatMessage{m("user", "x")})
	c.setSessionID("ses_busy")
	c.setLast([]ChatMessage{m("user", "x")})
	// 不 Release → 模拟请求进行中

	if dead := s.GC(); len(dead) != 0 {
		t.Fatalf("GC should skip busy conversations, got %v", dead)
	}
	s.Release(c)
}

// ---------- 上游自动发现 ----------

func TestIsOpenCodeServe(t *testing.T) {
	yes := [][]string{
		{"/root/.opencode/bin/opencode", "--log-level", "warn", "serve", "--hostname", "0.0.0.0", "--port", "2809"},
		{"opencode", "serve"},
	}
	no := [][]string{
		{"/usr/bin/bash", "-c", "opencode serve"},        // 不是进程本体
		{"/root/.opencode/bin/opencode", "run", "hello"}, // 没有 serve
		{},
	}
	for _, a := range yes {
		if !isOpenCodeServe(a) {
			t.Errorf("should detect serve: %v", a)
		}
	}
	for _, a := range no {
		if isOpenCodeServe(a) {
			t.Errorf("should NOT detect serve: %v", a)
		}
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "2809"}
	if got := flagValue(args, "--port"); got != "2809" {
		t.Errorf("port = %q", got)
	}
	if got := flagValue(args, "--hostname"); got != "0.0.0.0" {
		t.Errorf("hostname = %q", got)
	}
	if got := flagValue(args, "--missing"); got != "" {
		t.Errorf("missing flag = %q", got)
	}
	// --key=value 形式
	if got := flagValue([]string{"opencode", "serve", "--port=1234"}, "--port"); got != "1234" {
		t.Errorf("inline form = %q", got)
	}
}

// 真实环境里至少应能看到当前跑着的 opencode serve（本机自测）。
func TestScanProcFindsRunningOpenCode(t *testing.T) {
	cands, err := scanProc()
	if err != nil {
		t.Skipf("cannot scan /proc: %v", err)
	}
	if len(cands) == 0 {
		t.Skip("no opencode serve process running; skipping")
	}
	for _, c := range cands {
		if c.Base == "" {
			t.Errorf("bad candidate: %+v", c)
		}
	}
}

func TestDiscoverAgainstLiveServer(t *testing.T) {
	cands, err := scanProc()
	if err != nil || len(cands) == 0 {
		t.Skip("no opencode serve process running; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ep, err := Discover(ctx, probeEndpoint)
	if err != nil {
		t.Fatalf("discover failed: %v", err)
	}
	if ep.Pass == "" {
		t.Error("discovered endpoint has empty password")
	}
	t.Logf("discovered %s (pid %d)", ep.Base, ep.PID)
}

// 并发首请求（历史都为空）不能都挤到同一个会话上。
func TestStoreConcurrentFirstRequestsGetDistinctSessions(t *testing.T) {
	s := newTestStore()
	key := "f:race"

	const n = 8
	var wg sync.WaitGroup
	convs := make([]*Conversation, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := []ChatMessage{m("user", fmt.Sprintf("topic-%d", i))}
			c := s.Acquire(key, in)
			c.setLast(in) // 模拟发完 prompt
			convs[i] = c
			s.Release(c)
		}(i)
	}
	wg.Wait()

	seen := map[*Conversation]int{}
	for _, c := range convs {
		seen[c]++
	}
	if len(seen) != n {
		t.Fatalf("并发首请求应各自独立，实际只有 %d 个会话承载了 %d 个请求", len(seen), n)
	}
}

// 并发多轮：同一话题的请求应连续命中同一会话。
func TestStoreConcurrentSameConversation(t *testing.T) {
	s := newTestStore()
	key := "f:seq"

	base := []ChatMessage{m("user", "q")}
	c0 := s.Acquire(key, base)
	c0.setLast(base)
	c0.setSessionID("ses_seq")
	s.Release(c0)

	var wg sync.WaitGroup
	got := make([]*Conversation, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = s.Acquire(key, base) // 前缀相同，都应命中 ses_seq
			time.Sleep(time.Millisecond)
			s.Release(got[i])
		}(i)
	}
	wg.Wait()

	for i, c := range got {
		if c.snapshotSessionID() != "ses_seq" {
			t.Fatalf("第 %d 个并发的同话题请求跑到了别的会话: %q", i, c.snapshotSessionID())
		}
	}
}

// ---------- default 别名 ----------

func TestIsDefaultAlias(t *testing.T) {
	for _, in := range []string{"", "  ", "default", "DEFAULT", "Default", "auto", "AUTO"} {
		if !isDefaultAlias(in) {
			t.Errorf("%q 应被识别为默认别名", in)
		}
	}
	for _, in := range []string{"opencode/fledge-alpha-free", "fledge-alpha-free", "defaults", "mydefault", "gpt-4o"} {
		if isDefaultAlias(in) {
			t.Errorf("%q 不应被识别为默认别名", in)
		}
	}
}

// ---------- 用量/余额 ----------

func TestMicroCentsToUSD(t *testing.T) {
	cases := map[microCents]float64{
		0:         0,
		100000000: 1,          // 1 USD
		856750577: 8.56750577, // 与实测 summary 对齐
		1:         0.00000001,
	}
	for in, want := range cases {
		if got := in.USD(); math.Abs(got-want) > 1e-12 {
			t.Errorf("microCents(%d).USD() = %v, want %v", in, got, want)
		}
	}
}

// console API 把数字序列化成字符串，flexInt 必须两种都认。
func TestFlexIntAcceptsStringAndNumber(t *testing.T) {
	var v struct {
		A flexInt `json:"a"`
		B flexInt `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":"342653290","b":1294}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 342653290 || v.B != 1294 {
		t.Fatalf("A=%d B=%d", v.A, v.B)
	}
}

func TestFlexIntHandlesMissingAndNull(t *testing.T) {
	var v struct {
		A flexInt `json:"a"`
		B flexInt `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 0 || v.B != 0 {
		t.Fatalf("A=%d B=%d", v.A, v.B)
	}
}

func TestFlexIntRejectsGarbage(t *testing.T) {
	var v struct {
		A flexInt `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":"not-a-number"}`), &v); err == nil {
		t.Fatal("expected error")
	}
}

// 实测响应体必须能完整解析成报告结构。
func TestParseRealConsolePayloads(t *testing.T) {
	billing := `{"billingMode":"prepaid","mode":"pay-as-you-go","balanceMicroCents":"0",
	  "creditLimitMicroCents":null,"availableMicroCents":"0",
	  "canPurchaseCredits":true,"canEnableAutoRecharge":true}`
	var b billingRaw
	if err := json.Unmarshal([]byte(billing), &b); err != nil {
		t.Fatal(err)
	}
	if b.Mode != "pay-as-you-go" || b.BillingMode != "prepaid" {
		t.Fatalf("%+v", b)
	}
	if b.BalanceMicroCents.USD() != 0 || b.CreditLimitMicroCents != nil {
		t.Fatalf("balance/limit 解析错误: %+v", b)
	}

	summary := `{"totalRequests":"4084","totalInputTokens":"16570229",
	  "totalOutputTokens":"3005418","totalCacheReadTokens":"1550468473",
	  "totalCacheWrite5mTokens":"75186","totalCacheWrite1hTokens":"0",
	  "totalCostMicroCents":"856750577","services":[]}`
	var sm summaryRaw
	if err := json.Unmarshal([]byte(summary), &sm); err != nil {
		t.Fatal(err)
	}
	if int64(sm.TotalRequests) != 4084 {
		t.Fatalf("requests=%d", sm.TotalRequests)
	}
	if math.Abs(sm.TotalCostMicroCents.USD()-8.56750577) > 1e-9 {
		t.Fatalf("cost=%v", sm.TotalCostMicroCents.USD())
	}

	models := `{"items":[{"model":"fledge-alpha-free","provider":"opencode",
	  "totalRequests":"82","totalInputTokens":"352171","totalOutputTokens":"7776",
	  "totalCacheReadTokens":"122624","totalCostMicroCents":"0"}]}`
	var m modelsRaw
	if err := json.Unmarshal([]byte(models), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Items) != 1 || m.Items[0].Model != "fledge-alpha-free" {
		t.Fatalf("%+v", m)
	}

	byDay := `[{"date":"2026-10-02","totalCostMicroCents":"342653290",
	  "totalTokens":"560025171","totalRequests":"1294"}]`
	var d []dayRaw
	if err := json.Unmarshal([]byte(byDay), &d); err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || d[0].Date != "2026-10-02" {
		t.Fatalf("%+v", d)
	}
}

func TestDefaultOpencodeDB(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	if got := defaultOpencodeDB(); got != "/custom/data/opencode/opencode.db" {
		t.Fatalf("got %q", got)
	}
}

// ---------- 思考强度映射 ----------

func modelWithVariants(ids ...string) OCModel {
	m := OCModel{ID: "m", ProviderID: "p"}
	for _, id := range ids {
		var v struct {
			ID       string          `json:"id"`
			Settings json.RawMessage `json:"settings,omitempty"`
		}
		v.ID = id
		m.Variants = append(m.Variants, v)
	}
	return m
}

func TestPickVariantExact(t *testing.T) {
	m := modelWithVariants("low", "high", "max")
	for _, want := range []string{"low", "high", "max"} {
		got, ok := PickVariant(m, want)
		if !ok || got != want {
			t.Errorf("PickVariant(%q) = (%q,%v)", want, got, ok)
		}
	}
}

func TestPickVariantEmptyMeansDefault(t *testing.T) {
	m := modelWithVariants("low", "high")
	for _, in := range []string{"", "  ", "default", "DEFAULT", "auto"} {
		got, ok := PickVariant(m, in)
		if !ok || got != "" {
			t.Errorf("PickVariant(%q) = (%q,%v), want (\"\",true)", in, got, ok)
		}
	}
}

func TestPickVariantAliases(t *testing.T) {
	m := modelWithVariants("minimal", "medium", "xhigh", "max")
	cases := map[string]string{
		"min": "minimal", "minimum": "minimal",
		"mid": "medium", "med": "medium", "normal": "medium",
		"x-high": "xhigh", "extra-high": "xhigh", "extra_high": "xhigh", "very-high": "xhigh",
		"maximum": "max", "highest": "max",
	}
	for in, want := range cases {
		got, ok := PickVariant(m, in)
		if !ok || got != want {
			t.Errorf("PickVariant(%q) = (%q,%v), want %q", in, got, ok, want)
		}
	}
}

// 标准档位在模型上不存在时，取最接近的。
func TestPickVariantNearest(t *testing.T) {
	m := modelWithVariants("low", "high") // 没有 medium
	if got, _ := PickVariant(m, "medium"); got != "low" {
		t.Errorf("medium 应就近取 low（并列取更弱），got %q", got)
	}
	m2 := modelWithVariants("medium", "max") // 没有 low
	if got, _ := PickVariant(m2, "low"); got != "medium" {
		t.Errorf("low 应就近取 medium，got %q", got)
	}
	m3 := modelWithVariants("low", "high") // 没有 none
	if got, _ := PickVariant(m3, "none"); got != "low" {
		t.Errorf("none 应退到最低档 low，got %q", got)
	}
}

// 布尔档位模型：非 none 一律 thinking。
func TestPickVariantBooleanThinking(t *testing.T) {
	m := modelWithVariants("none", "thinking")
	if got, _ := PickVariant(m, "none"); got != "none" {
		t.Errorf("none → %q", got)
	}
	for _, in := range []string{"low", "medium", "high", "max"} {
		if got, _ := PickVariant(m, in); got != "thinking" {
			t.Errorf("%q → %q, want thinking", in, got)
		}
	}
}

func TestPickVariantNoVariants(t *testing.T) {
	m := modelWithVariants()
	if _, ok := PickVariant(m, "high"); ok {
		t.Error("无 variants 的模型应返回 false，让调用方忽略该参数")
	}
}

func TestPickVariantUnknownEffort(t *testing.T) {
	m := modelWithVariants("low", "high")
	if _, ok := PickVariant(m, "supersonic"); ok {
		t.Error("未知档位名应返回 false")
	}
}

func TestVariantIDsSortedByStrength(t *testing.T) {
	m := modelWithVariants("max", "none", "high", "minimal", "low")
	got := VariantIDs(m)
	want := []string{"none", "minimal", "low", "high", "max"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestToOpenAIExposesLimits(t *testing.T) {
	m := OCModel{
		ID: "claude-sonnet-5-5", ProviderID: "opencode",
		Limit: struct {
			Context int `json:"context"`
			Input   int `json:"input"`
			Output  int `json:"output"`
		}{Context: 1000000, Output: 131072},
	}
	m.Capabilities.Input = []string{"text", "image", "pdf"}
	m.Variants = modelWithVariants("low", "high", "max").Variants

	o := ToOpenAI(m)
	if o.ID != "opencode/claude-sonnet-5-5" || o.OwnedBy != "opencode" {
		t.Fatalf("%+v", o)
	}
	if o.ContextLength != 1000000 || o.MaxOutputTokens != 131072 {
		t.Fatalf("limits: %+v", o)
	}
	if strings.Join(o.SupportedReasoningEfforts, ",") != "low,high,max" {
		t.Fatalf("efforts: %v", o.SupportedReasoningEfforts)
	}
	if strings.Join(o.InputModalities, ",") != "text,image,pdf" {
		t.Fatalf("modalities: %v", o.InputModalities)
	}
}

// ---------- 配置文件 ----------

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

// ---------- 空回复不入缓存 ----------

func respWith(content, reasoning string) *ChatResponse {
	return &ChatResponse{Choices: []Choice{{Message: AssistantMsg{
		Role: "assistant", Content: content, ReasoningContent: reasoning,
	}}}}
}

func TestResponseHasContent(t *testing.T) {
	if responseHasContent(nil) {
		t.Error("nil 不算有内容")
	}
	if responseHasContent(&ChatResponse{}) {
		t.Error("无 choices 不算有内容")
	}
	if responseHasContent(respWith("", "")) {
		t.Error("空正文+空推理不算有内容")
	}
	if responseHasContent(respWith("   ", "\n")) {
		t.Error("纯空白不算有内容")
	}
	if !responseHasContent(respWith("hi", "")) {
		t.Error("有正文就算有内容")
	}
	if !responseHasContent(respWith("", "thinking...")) {
		t.Error("只有推理也算有内容")
	}
}

func TestLastUserTurn(t *testing.T) {
	msgs := []ChatMessage{
		m("system", "S"),
		m("user", "first"),
		m("assistant", "a1"),
		m("user", "second"),
	}
	got := lastUserTurn(msgs)
	if len(got) != 1 || got[0].Content.Text != "second" {
		t.Fatalf("got %+v", got)
	}
	if len(lastUserTurn([]ChatMessage{m("system", "S")})) != 0 {
		t.Error("没有 user 消息时应返回空")
	}
}

// 关键回归：空回复不能被 DiffNone 回放。
func TestEmptyResponseNotReplayed(t *testing.T) {
	s := newTestStore()
	key := "f:k"
	msgs := []ChatMessage{m("user", "问题")}

	c := s.Acquire(key, msgs)
	c.setLast(msgs)
	c.setSessionID("ses_1")
	// 模拟一次"上游返回空"的结果被写入缓存
	c.setResponse(respWith("", ""))
	s.Release(c)

	// 同一个请求再来一次 → DiffNone
	c2 := s.Acquire(key, msgs)
	defer s.Release(c2)
	mode, _ := Diff(c2.snapshotLast(), msgs)
	if mode != DiffNone {
		t.Fatalf("mode = %v, want DiffNone", mode)
	}
	if responseHasContent(c2.snapshotResponse()) {
		t.Fatal("空回复不应被视为可用缓存，否则会被原样回放")
	}
	// 调用方据此会退回重跑最后一条 user 消息
	if retry := lastUserTurn(msgs); len(retry) != 1 {
		t.Fatal("应能取到最后一条 user 消息用于重跑")
	}
}

// ---------- 模型详情与过滤 ----------

func richModel() OCModel {
	m := OCModel{
		ID: "claude-sonnet-5-5", ProviderID: "opencode",
		Name: "Claude Sonnet 5.5", Family: "claude-sonnet",
		Status: "active", Enabled: true,
	}
	m.Capabilities.Tools = true
	m.Capabilities.Input = []string{"text", "image", "pdf"}
	m.Capabilities.Output = []string{"text"}
	m.Limit.Context = 1000000
	m.Limit.Output = 128000
	m.Time.Released = 1790553600000
	m.Variants = modelWithVariants("low", "high", "max").Variants
	m.Cost = append(m.Cost, struct {
		Tier *struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tier,omitempty"`
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
		Cache  struct {
			Read  float64 `json:"read"`
			Write float64 `json:"write"`
		} `json:"cache"`
	}{Input: 2, Output: 10})
	m.Cost[0].Cache.Read = 0.2
	m.Cost[0].Cache.Write = 2.5
	return m
}

func TestToDetail(t *testing.T) {
	d := ToDetail(richModel())
	if d.ID != "opencode/claude-sonnet-5-5" || d.Name != "Claude Sonnet 5.5" {
		t.Fatalf("%+v", d)
	}
	if d.ContextLength != 1000000 || d.MaxOutputTokens != 128000 {
		t.Fatalf("limits: %d/%d", d.ContextLength, d.MaxOutputTokens)
	}
	if !d.ToolCalling || d.Capabilities == nil || !d.Capabilities.Tools {
		t.Fatalf("capabilities: %+v", d.Capabilities)
	}
	if strings.Join(d.SupportedReasoningEfforts, ",") != "low,high,max" {
		t.Fatalf("efforts: %v", d.SupportedReasoningEfforts)
	}
	if len(d.Variants) != 3 || d.Variants[0].ID != "low" {
		t.Fatalf("variants: %+v", d.Variants)
	}
	if len(d.Pricing) != 1 || d.Pricing[0].InputPerMillion != 2 ||
		d.Pricing[0].OutputPerMillion != 10 ||
		d.Pricing[0].CacheReadPerMillion != 0.2 {
		t.Fatalf("pricing: %+v", d.Pricing)
	}
}

func TestModelFilterEmpty(t *testing.T) {
	if !(ModelFilter{}).Empty() {
		t.Error("零值应为空过滤")
	}
	if (ModelFilter{Provider: "opencode"}).Empty() {
		t.Error("有 provider 不算空")
	}
}

func TestModelFilterMatch(t *testing.T) {
	mk := func(id string, tools bool, ctx int, in ...string) OCModel {
		m := OCModel{ID: id, ProviderID: "p", Enabled: true}
		m.Capabilities.Tools = tools
		m.Capabilities.Input = in
		m.Limit.Context = ctx
		return m
	}
	vision := mk("vision", true, 200000, "text", "image")
	textOnly := mk("text", false, 8000, "text")
	bigText := mk("big", true, 1000000, "text")

	if !(ModelFilter{Provider: "p"}).Match(vision) {
		t.Error("provider 匹配失败")
	}
	if (ModelFilter{Provider: "other"}).Match(vision) {
		t.Error("provider 不该匹配")
	}
	if !(ModelFilter{Tools: true}).Match(vision) || (ModelFilter{Tools: true}).Match(textOnly) {
		t.Error("tools 过滤错误")
	}
	if !(ModelFilter{Modality: "image"}).Match(vision) ||
		(ModelFilter{Modality: "image"}).Match(bigText) {
		t.Error("modality 过滤错误")
	}
	if !(ModelFilter{MinContext: 100000}).Match(vision) ||
		(ModelFilter{MinContext: 100000}).Match(textOnly) {
		t.Error("min_context 过滤错误")
	}
	// 组合条件
	if !(ModelFilter{Tools: true, Modality: "image", MinContext: 100000}).Match(vision) {
		t.Error("组合条件应命中 vision")
	}
	if (ModelFilter{Tools: true, Modality: "image"}).Match(bigText) {
		t.Error("big 不支持 image，不该命中")
	}
}

func TestModelFilterSkipsDisabled(t *testing.T) {
	m := OCModel{ID: "x", ProviderID: "p", Enabled: false}
	if (ModelFilter{}).Match(m) {
		t.Error("禁用的模型不应出现在列表里")
	}
}

func TestAtoiOrZero(t *testing.T) {
	cases := map[string]int{"0": 0, "1000": 1000, "": 0, "abc": 0, "-5": 0, " 42 ": 42}
	for in, want := range cases {
		if got := atoiOrZero(in); got != want {
			t.Errorf("atoiOrZero(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---------- SSE 错误事件 ----------

func TestSSEErrorEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "opencode/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.begin(); err != nil {
		t.Fatal(err)
	}
	if err := sw.errorEvent("insufficient_quota", "upstream returned an empty completion", "empty_completion"); err != nil {
		t.Fatal(err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Errorf("缺少 error 对象:\n%s", body)
	}
	if !strings.Contains(body, "empty_completion") {
		t.Errorf("缺少 code:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺少 [DONE]:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}

	// 收尾之后再写必须被丢弃，不能污染流
	before := rec.Body.Len()
	_ = sw.deltaText("should-not-appear")
	if rec.Body.Len() != before {
		t.Error("errorEvent 之后仍写入了内容")
	}
	if strings.Contains(rec.Body.String(), "should-not-appear") {
		t.Error("closed 流里出现了内容")
	}
}

func TestSSENormalFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "opencode/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = sw.begin()
	if err := sw.deltaText("hi"); err != nil {
		t.Fatal(err)
	}
	u := Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	if err := sw.finish("stop", &u, true); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("缺少正文:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason:\n%s", body)
	}
	if !strings.Contains(body, `"total_tokens":3`) {
		t.Errorf("include_usage 时应输出 usage:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺少 [DONE]:\n%s", body)
	}
}

// ---------- 路由 ----------

// stubUpstream 起一个最小的假上游，避免测试依赖真实 OpenCode（也避免连不可达
// 端口时挂到内核 connect 超时）。
func stubUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/model"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","modelID":"m1","providerID":"p","name":"M1","enabled":true,"limit":{"context":1000,"output":100},"capabilities":{"tools":true,"input":["text"],"output":["text"]}}]}`))
		case strings.HasSuffix(r.URL.Path, "/api/model/default"):
			_, _ = w.Write([]byte(`{"data":{"id":"m1","modelID":"m1","providerID":"p","name":"M1","enabled":true,"limit":{"context":1000,"output":100}}}`))
		case strings.HasSuffix(r.URL.Path, "/api/info"):
			_, _ = w.Write([]byte(`{"version":"test"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"nope"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestServerForRoutes(t *testing.T) *Server {
	t.Helper()
	stub := stubUpstream(t)
	cfg := Config{
		Upstream: stub.URL, Username: "opencode", Password: "x",
		Directory: "/tmp", Agent: "build",
		APIKey:       "sk-test",
		UsageEnabled: false,
		ConsoleURL:   stub.URL,
	}
	return NewServer(cfg, NewLogger("error"))
}

// 回归：模型名含斜杠（provider/model），必须能路由到 handleGetModel，
// 而不是掉进 /v1/ 的兜底 404。
func TestModelDetailRouteMatchesMultiSegmentID(t *testing.T) {
	h := newTestServerForRoutes(t).Handler()

	for _, path := range []string{
		"/v1/models/p/m1",
		"/v1/models/default",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		body := rec.Body.String()
		if strings.Contains(body, "unknown_endpoint") {
			t.Errorf("%s 掉进了兜底 404（路由没匹配上）:\n%s", path, body)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, body=%s", path, rec.Code, body)
		}
		if !strings.Contains(body, `"context_length"`) {
			t.Errorf("%s 详情缺少 context_length:\n%s", path, body)
		}
	}

	// 兜底仍然要生效
	req := httptest.NewRequest(http.MethodGet, "/v1/embeddings", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "unknown_endpoint") {
		t.Errorf("未知端点应返回 unknown_endpoint:\n%s", rec.Body.String())
	}
}

func TestModelDetailRouteRequiresAuth(t *testing.T) {
	srv := newTestServerForRoutes(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models/p/m1", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// 上游不可达时，短操作必须快速失败而不是挂到内核 connect 超时。
func TestListModelsFailsFastWhenUpstreamUnreachable(t *testing.T) {
	cfg := Config{
		Upstream: "http://127.0.0.1:9", // discard 端口：连接被拒/丢弃
		Username: "o", Password: "p", Directory: "/tmp", APIKey: "sk-test",
		ConsoleURL: "http://127.0.0.1:9",
	}
	srv := NewServer(cfg, NewLogger("error"))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { srv.Handler().ServeHTTP(rec, req); close(done) }()

	select {
	case <-done:
		// 必须在 shortTimeout（30s）内返回，这里给足余量
	case <-time.After(35 * time.Second):
		t.Fatal("上游不可达时 /v1/models 挂住了（缺少短操作超时）")
	}
	if rec.Code < 400 {
		t.Fatalf("status = %d, want >= 400", rec.Code)
	}
}

// ---------- 上游失败原因透传 ----------

func TestMapUpstreamFailureQuota(t *testing.T) {
	f := &upstreamFailure{Type: "provider.quota",
		Message: "Upstream request failed: Insufficient account funds", Status: 402}
	status, typ, code, msg := mapUpstreamFailure(f)
	if status != 429 || typ != "insufficient_quota" || code != "insufficient_quota" {
		t.Fatalf("got %d %s %s", status, typ, code)
	}
	if !strings.Contains(msg, "Insufficient account funds") {
		t.Fatalf("msg = %q", msg)
	}
}

func TestMapUpstreamFailureRateLimit(t *testing.T) {
	f := &upstreamFailure{Type: "provider.rate_limit", Message: "too many requests", Status: 429}
	status, typ, code, _ := mapUpstreamFailure(f)
	if status != 429 || typ != "rate_limit_error" || code != "rate_limit_exceeded" {
		t.Fatalf("got %d %s %s", status, typ, code)
	}
}

func TestMapUpstreamFailureUnknown(t *testing.T) {
	status, typ, code, _ := mapUpstreamFailure(&upstreamFailure{Message: "boom"})
	if status != 502 || typ != "api_error" || code != "upstream_error" {
		t.Fatalf("got %d %s %s", status, typ, code)
	}
	if _, _, code, _ := mapUpstreamFailure(nil); code != "upstream_error" {
		t.Fatalf("nil case code = %q", code)
	}
}

func TestUpstreamFailureString(t *testing.T) {
	f := &upstreamFailure{Message: "m", Status: 402}
	if !strings.Contains(f.String(), "402") || !strings.Contains(f.String(), "m") {
		t.Fatalf("%q", f.String())
	}
	if (*upstreamFailure)(nil).String() != "" {
		t.Error("nil String() 应为空")
	}
}

// assistant 消息的 error 字段必须被 reconcile 捕获（余额不足的实测形状）。
func TestReconcileCapturesAssistantError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"msg_1","type":"assistant","finish":"error",
		  "error":{"type":"provider.quota","message":"Upstream request failed: Insufficient account funds","status":402},
		  "time":{"created":99999999999999},"content":[]}],"cursor":{"previous":"","next":""}}`))
	}))
	defer stub.Close()

	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p", Directory: "/tmp",
		APIKey: "sk", ConsoleURL: stub.URL}
	srv := NewServer(cfg, NewLogger("error"))
	ex := newExecutor(srv, "ses_x", "m", false)

	ex.reconcile(context.Background(), 1)

	f := ex.failure()
	if f == nil {
		t.Fatal("未捕获上游失败原因")
	}
	if f.Type != "provider.quota" || f.Status != 402 {
		t.Fatalf("%+v", f)
	}
	if status, _, code, _ := mapUpstreamFailure(f); status != 429 || code != "insufficient_quota" {
		t.Fatalf("映射错误: %d %s", status, code)
	}
}

// 上游 outcome=failed 时不能报 succeeded。
func TestTerminalSignalFromOutcome(t *testing.T) {
	ex := newExecutor(&Server{log: NewLogger("error")}, "s", "m", false)
	cases := map[string]string{
		"failed":      "failed",
		"FAILED":      "failed",
		" failed ":    "failed",
		"succeeded":   "succeeded",
		"":            "succeeded",
		"interrupted": "succeeded",
	}
	for in, want := range cases {
		if got := ex.terminalSignal(in); got != want {
			t.Errorf("terminalSignal(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------- 响应 ID ----------

func TestNewIDFormat(t *testing.T) {
	id := newID()
	if len(id) != 32 {
		t.Fatalf("长度 = %d, want 32（128 位十六进制）: %q", len(id), id)
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("非十六进制字符 %q in %q", c, id)
		}
	}
}

// 高并发下不能碰撞，也不能退化成可预测序列。
func TestNewIDUniqueAndUnpredictable(t *testing.T) {
	const n = 20000
	seen := make(map[string]struct{}, n)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n/8; i++ {
				id := newID()
				mu.Lock()
				seen[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("碰撞：%d 次生成只有 %d 个唯一值", n, len(seen))
	}

	// 相邻两次不应有可见规律（自制 LCG 时它们会高度相关）
	a, b := newID(), newID()
	if a == b {
		t.Fatal("相邻 ID 相同")
	}
	common := 0
	for i := 0; i < len(a); i++ {
		if a[i] == b[i] {
			common++
		}
	}
	if common > 12 {
		t.Errorf("相邻 ID 有 %d/%d 位相同，随机性可疑\n a=%s\n b=%s", common, len(a), a, b)
	}
}

func TestFallbackIDFormat(t *testing.T) {
	id := fallbackID()
	if len(id) != 32 {
		t.Fatalf("fallback 长度 = %d, want 32: %q", len(id), id)
	}
	if fallbackID() == id {
		t.Fatal("fallback 连续两次相同")
	}
}

// 端到端：响应里的 id 形如 chatcmpl-<32 hex>。
func TestCompletionIDPrefix(t *testing.T) {
	resp := buildResponse("chatcmpl-"+newID(), time.Now().Unix(), "m", runResult{text: "x"})
	if !strings.HasPrefix(resp.ID, "chatcmpl-") {
		t.Fatalf("id = %q", resp.ID)
	}
	if len(resp.ID) != len("chatcmpl-")+32 {
		t.Fatalf("id 长度异常: %q", resp.ID)
	}
}

// user 必须参与分桶：不同终端用户不应共用同一个桶。
func TestConversationKeyDiffersByUser(t *testing.T) {
	a := ConversationKey("", "sys", "user-a", "/tmp/proj")
	b := ConversationKey("", "sys", "user-b", "/tmp/proj")
	if a == b {
		t.Fatal("不同 user 不应共用分桶 key")
	}
	// 不传 user 时仍应稳定（老客户端行为不变）
	if ConversationKey("", "sys", "", "/d") != ConversationKey("", "sys", "", "/d") {
		t.Fatal("key 必须稳定")
	}
}

// ---------- 模型列表缓存健壮性 ----------

// 上游 200 + 空数组时不能把已有缓存清空。
func TestModelCacheKeepsOldOnEmptyList(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/model") {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer empty.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/model") {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","providerID":"p","enabled":true}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"m1","providerID":"p"}}`))
	}))
	defer good.Close()

	cache := NewModelCache(time.Minute)

	upGood := NewUpstream(Config{Upstream: good.URL, Username: "o", Password: "p"}, NewLogger("error"))
	list, err := cache.Get(context.Background(), upGood, "/d", true)
	if err != nil || len(list) != 1 {
		t.Fatalf("首拉失败: %v %d", err, len(list))
	}

	// 现在上游返回空列表（超时刷新）
	upEmpty := NewUpstream(Config{Upstream: empty.URL, Username: "o", Password: "p"}, NewLogger("error"))
	got, err := cache.Get(context.Background(), upEmpty, "/d", true)
	if err != nil {
		t.Fatalf("空列表不该让整体失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("空列表覆盖了旧缓存: %+v", got)
	}
}

// 没有任何旧缓存 + 空列表 → 必须报错，不能返回空列表让模型"消失"。
func TestModelCacheEmptyListWithNoCacheIsError(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer empty.Close()

	cache := NewModelCache(time.Minute)
	up := NewUpstream(Config{Upstream: empty.URL, Username: "o", Password: "p"}, NewLogger("error"))
	if _, err := cache.Get(context.Background(), up, "/d", true); err == nil {
		t.Fatal("空列表且无缓存时应报错")
	}
}

// out 非 nil 但响应体为空 → 视为错误。
func TestUpstreamEmptyBodyIsError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // 200 + 空 body
	}))
	defer stub.Close()

	up := NewUpstream(Config{Upstream: stub.URL, Username: "o", Password: "p"}, NewLogger("error"))
	var out struct {
		Data []OCModel `json:"data"`
	}
	if err := up.do(context.Background(), http.MethodGet, "/api/model", nil, nil, &out); err == nil {
		t.Fatal("空响应体应报错，而不是静默留下零值")
	}
	// out 为 nil 的调用（如 interrupt）不受影响
	if err := up.do(context.Background(), http.MethodPost, "/api/x", nil, nil, nil); err != nil {
		t.Fatalf("out=nil 不该报错: %v", err)
	}
}

// ---------- ResponseWriter 使用后写 ----------

func TestMarkClosedStopsWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = sw.begin()
	_ = sw.deltaText("before")
	sw.markClosed()

	before := rec.Body.Len()
	_ = sw.deltaText("after")
	_ = sw.deltaReasoning("after")
	_ = sw.finish("stop", nil, false)
	sw.ping()

	if rec.Body.Len() != before {
		t.Errorf("markClosed 之后仍写入了内容:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "after") {
		t.Error("closed 流里出现了内容")
	}
}

// 并发 write + markClosed 不能碰到 ResponseWriter（-race 下会报）。
// 这正是客户端断开后 handler 已返回、执行协程仍在写的场景。
func TestConcurrentWriteAndMarkClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "m")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = sw.deltaText("x")
					sw.ping()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sw.markClosed()
		close(stop)
	}()
	wg.Wait()
}

func TestSSEPing(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "m")
	if err != nil {
		t.Fatal(err)
	}
	sw.ping()
	body := rec.Body.String()
	if !strings.Contains(body, ": ping\n\n") {
		t.Fatalf("心跳未写出: %q", body)
	}
	// 心跳是 SSE 注释行，不能污染 data 通道
	if strings.Contains(body, "data:") {
		t.Errorf("心跳不该产生 data 行: %q", body)
	}
}
