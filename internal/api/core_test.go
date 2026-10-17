package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/logx"
	"janus/internal/prompt"
	"janus/internal/upstream"
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

// 工具结果要作为具名注释写进正文；且不再产生空的 "name:" 噪声注释。
func TestToolResultAnnotatedOnSuccess(t *testing.T) {
	ex := newExecutor(&Server{log: logx.NewLogger("error")}, "ses_ok", "m", true)
	ctx := context.Background()

	_ = ex.handleEvent(ctx, upstream.OCEvent{Type: "session.tool.input.started",
		Data: json.RawMessage(`{"sessionID":"ses_ok","id":"call_1","name":"subagent","text":"{}"}`)}, 0)
	_ = ex.handleEvent(ctx, upstream.OCEvent{Type: "session.tool.success",
		Data: json.RawMessage(`{"sessionID":"ses_ok","id":"call_1","content":[{"type":"text","text":"审计结论：无高危漏洞"}]}`)}, 0)

	got := ex.text.String()
	if !strings.Contains(got, "subagent: 审计结论") {
		t.Fatalf("结果注释缺失: %q", got)
	}
	if strings.Contains(got, "subagent: </opencode-tool>") {
		t.Fatalf("空名噪声注释应已移除: %q", got)
	}
}

// 被中止的会话要能被标记，且标记只消费一次（下一轮重开会话）。
func TestConversationTerminatedFlag(t *testing.T) {
	c := &Conversation{}
	if c.takeTerminated() {
		t.Fatal("初始不应为 true")
	}
	c.markTerminated()
	if !c.takeTerminated() {
		t.Fatal("mark 后应为 true")
	}
	if c.takeTerminated() {
		t.Fatal("take 后应被清除")
	}
}

// TestIsJanusAliasRoleForms：§4.2 角色形式 janus/<role>[@auto|:variant] 必须被识别为
// janus 虚拟模型——否则落到 ResolveModel 按 provider="janus" 解析直接 404
// "unknown provider janus"，客户端表现为"LLM 空流"（实测）。
func TestIsJanusAliasRoleForms(t *testing.T) {
	cases := map[string]bool{
		"janus":                   true,
		"JANUS":                   true,
		" janus ":                 true,
		"janus/build":             true,
		"janus/build@auto":        true,
		"janus/orchestrator@auto": true,
		"janus/build:high":        true,
		"janus/":                  false,
		"janus/a/b":               false,
		"janusis":                 false,
		"default":                 false,
	}
	for raw, want := range cases {
		if got := isJanusAlias(raw); got != want {
			t.Errorf("isJanusAlias(%q) = %v, want %v", raw, got, want)
		}
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
	tk := &upstream.OCTokens{Input: 100, Output: 50, Reason: 20}
	tk.Cache.Read = 30
	e.addTokens(tk)
	e.addTokens(tk)

	u := e.snapshot().usage
	// prompt_tokens = 未命中输入 + 缓存命中；completion_tokens 含 reasoning（明细子项）。
	if u.PromptTokens != 260 || u.CompletionTokens != 140 {
		t.Fatalf("got %+v", u)
	}
	if u.TotalTokens != 400 {
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
	e := newExecutor(&Server{log: logx.NewLogger("error")}, "ses_x", "m", true)
	_ = e.addText("1+1=2")                                 // 上游纯文本 5B
	_ = e.addToolText(prompt.ToolAnnotation("bash", "ls")) // 注释把 text 撑到 100B+

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

const redPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// ---------- 分桶 + 前缀匹配 ----------

func newTestStore() *Store { return NewStore(logx.NewLogger("error"), time.Minute, time.Minute, 64) }

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
	s := NewStore(logx.NewLogger("error"), time.Minute, time.Minute, 4)
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
	s := NewStore(logx.NewLogger("error"), time.Minute, time.Minute, 2)
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
	s := NewStore(logx.NewLogger("error"), 0, 0, 8) // ttl=0 → 立即过期
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
	s := NewStore(logx.NewLogger("error"), 0, 0, 8)
	c := s.Acquire("f:k", []ChatMessage{m("user", "x")})
	c.setSessionID("ses_busy")
	c.setLast([]ChatMessage{m("user", "x")})
	// 不 Release → 模拟请求进行中

	if dead := s.GC(); len(dead) != 0 {
		t.Fatalf("GC should skip busy conversations, got %v", dead)
	}
	s.Release(c)
}

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

// 同一分桶 key、内容不同的两个 client，即使不是并发，也必须落到不同会话。
// （分桶只负责召回候选，最终由"历史前缀严格匹配"决定归属。）
func TestStoreSameBucketDifferentContentIsolated(t *testing.T) {
	s := newTestStore()
	key := "f:same-bucket"

	aIn := []ChatMessage{m("user", "client-A：帮我改 A 项目")}
	a := s.Acquire(key, aIn)
	a.setLast(aIn)
	a.setSessionID("ses_A")
	s.Release(a)

	bIn := []ChatMessage{m("user", "client-B：另一个完全不同的请求")}
	b := s.Acquire(key, bIn)
	b.setLast(bIn)
	b.setSessionID("ses_B")
	s.Release(b)

	if a == b {
		t.Fatal("同一分桶里内容不同的请求不应共用会话")
	}
	if b.snapshotSessionID() != "ses_B" {
		t.Fatalf("B 跑到了别的会话: %q", b.snapshotSessionID())
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

func TestModelFilterEmpty(t *testing.T) {
	if !(ModelFilter{}).Empty() {
		t.Error("零值应为空过滤")
	}
	if (ModelFilter{Provider: "opencode"}).Empty() {
		t.Error("有 provider 不算空")
	}
}

func TestModelFilterMatch(t *testing.T) {
	mk := func(id string, tools bool, ctx int, in ...string) upstream.OCModel {
		m := upstream.OCModel{ID: id, ProviderID: "p", Enabled: true}
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
	m := upstream.OCModel{ID: "x", ProviderID: "p", Enabled: false}
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

// 未显式请求 include_usage 时也要发 usage（CodeBuddy 等客户端依赖它统计输出与缓存）。
func TestSSEFinishAlwaysUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSE(rec, "opencode/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = sw.begin()
	u := Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11,
		PromptTokensDetails: &TokenDetails{CachedTokens: 4}}
	if err := sw.finish("stop", &u, false); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"total_tokens":11`) || !strings.Contains(body, `"cached_tokens":4`) {
		t.Errorf("未请求 include_usage 也应输出 usage:\n%s", body)
	}
}

// prompt_tokens 应为「未命中 + 命中」总量，cached_tokens 为命中的子集。
func TestAddTokensCacheIntoPrompt(t *testing.T) {
	e := &executor{}
	tk := &upstream.OCTokens{Input: 10, Output: 2}
	tk.Cache.Read = 90
	tk.Cache.Write = 5
	e.addTokens(tk)
	if e.usage.PromptTokens != 100 {
		t.Fatalf("PromptTokens = %d, want 100", e.usage.PromptTokens)
	}
	if e.usage.PromptTokensDetails == nil || e.usage.PromptTokensDetails.CachedTokens != 90 {
		t.Fatalf("CachedTokens = %+v, want 90", e.usage.PromptTokensDetails)
	}
	if e.usage.PromptTokensDetails.CacheCreationTokens != 5 {
		t.Fatalf("CacheCreationTokens = %d, want 5", e.usage.PromptTokensDetails.CacheCreationTokens)
	}
	if e.usage.CompletionTokens != 2 {
		t.Fatalf("CompletionTokens = %d, want 2", e.usage.CompletionTokens)
	}
	// DeepSeek 风格明细：hit + miss = prompt_tokens
	snap := e.snapshot().usage
	if snap.PromptCacheHitTokens != 90 || snap.PromptCacheMissTokens != 10 {
		t.Fatalf("cache hit/miss = %d/%d, want 90/10", snap.PromptCacheHitTokens, snap.PromptCacheMissTokens)
	}
}

// 客户端重排 assistant/tool 消息（只影响尾部）时，应容忍并追加最后一条 user，
// 而不是重开会话。改了早期 user 或尾部不是 user 时不 tolerate。
func TestTolerateReset(t *testing.T) {
	stored := []ChatMessage{
		{Role: "system", Content: MessageContent{Text: "s"}},
		{Role: "user", Content: MessageContent{Text: "u1"}},
		{Role: "assistant", Content: MessageContent{Text: "a1"}},
		{Role: "user", Content: MessageContent{Text: "u2"}},
	}
	incoming := []ChatMessage{
		{Role: "system", Content: MessageContent{Text: "s"}},
		{Role: "user", Content: MessageContent{Text: "u1"}},
		{Role: "assistant", Content: MessageContent{Text: "a1-CHANGED"}}, // 被重格式化
		{Role: "user", Content: MessageContent{Text: "u2"}},
		{Role: "assistant", Content: MessageContent{Text: "a2"}},
		{Role: "user", Content: MessageContent{Text: "u3"}},
	}
	d, ok := TolerateReset(stored, incoming)
	if !ok || len(d) != 1 || d[0].Content.Text != "u3" {
		t.Fatalf("ok=%v d=%+v", ok, d)
	}

	bad := []ChatMessage{
		{Role: "system", Content: MessageContent{Text: "s"}},
		{Role: "user", Content: MessageContent{Text: "u1-DIFFERENT"}},
		{Role: "user", Content: MessageContent{Text: "u2"}},
	}
	if _, ok := TolerateReset(stored, bad); ok {
		t.Fatal("早期 user 被改写不应 tolerate")
	}
	if _, ok := TolerateReset(stored, incoming[:len(incoming)-1]); ok {
		t.Fatal("尾部不是 user 不应 tolerate（工具结果走专门路径）")
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
	cfg := config.Config{
		Upstream: stub.URL, Username: "opencode", Password: "x",
		Directory: "/tmp", Agent: "build",
		APIKey:       "sk-test",
		UsageEnabled: false,
		ConsoleURL:   stub.URL,
	}
	return NewServer(cfg, logx.NewLogger("error"))
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
	cfg := config.Config{
		Upstream: "http://127.0.0.1:9", // discard 端口：连接被拒/丢弃
		Username: "o", Password: "p", Directory: "/tmp", APIKey: "sk-test",
		ConsoleURL: "http://127.0.0.1:9",
	}
	srv := NewServer(cfg, logx.NewLogger("error"))
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

	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p", Directory: "/tmp",
		APIKey: "sk", ConsoleURL: stub.URL}
	srv := NewServer(cfg, logx.NewLogger("error"))
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
	ex := newExecutor(&Server{log: logx.NewLogger("error")}, "s", "m", false)
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
