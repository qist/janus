package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mkModel(provider, id string, input, output float64, tools bool, enabled bool) OCModel {
	m := OCModel{ID: id, ModelID: id, ProviderID: provider, Enabled: enabled}
	m.Capabilities.Tools = tools
	m.Limit.Context = 100000
	m.Cost = append(m.Cost, OCCost{Input: input, Output: output})
	return m
}

func TestAnthropicTierDetection(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-1":   "opus",
		"claude-3-opus":     "opus",
		"claude-sonnet-4-5": "sonnet",
		"claude-3-5-haiku":  "haiku",
		"claude-haiku-4":    "haiku",
		"opencode-go/foo":   "",
		"gpt-4o":            "",
	}
	for in, want := range cases {
		if got := anthropicTier(in); got != want {
			t.Errorf("anthropicTier(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPickTierModel(t *testing.T) {
	list := []OCModel{
		mkModel("opencode", "fledge-alpha-free", 0, 0, true, true), // 免费额度，API 会 403，必须排除
		mkModel("opencode-go", "cheap-flash", 0.1, 0.2, true, true),
		mkModel("opencode-go", "mid-model", 1, 2, true, true),
		mkModel("opencode-go", "big-pro", 5, 10, true, true),
		mkModel("opencode-go", "no-tools", 0.01, 0.01, false, true), // 不支持工具，排除
		mkModel("opencode-go", "disabled", 9, 9, true, false),       // 禁用，排除
	}
	if ref, ok := pickTierModel(list, "haiku"); !ok || ref.String() != "opencode-go/cheap-flash" {
		t.Errorf("haiku → %v ok=%v", ref, ok)
	}
	if ref, ok := pickTierModel(list, "opus"); !ok || ref.String() != "opencode-go/big-pro" {
		t.Errorf("opus → %v ok=%v", ref, ok)
	}
	// sonnet 取中间（按排名+价格排序后的中间）
	if ref, ok := pickTierModel(list, "sonnet"); !ok || ref.ProviderID != "opencode-go" {
		t.Errorf("sonnet → %v ok=%v", ref, ok)
	}
	// 全被排除时返回 false
	if _, ok := pickTierModel([]OCModel{mkModel("opencode", "free", 0, 0, true, true)}, "opus"); ok {
		t.Error("只剩免费额度时应该选不出来")
	}
}

func TestResolveAnthropicModelDefaultKnob(t *testing.T) {
	list := []OCModel{
		mkModel("opencode", "free", 0, 0, true, true),
		mkModel("opencode-go", "cheap-flash", 0.1, 0.2, true, true),
		mkModel("opencode-go", "big-pro", 5, 10, true, true),
	}

	// 配了 BRIDGE_DEFAULT_MODEL：所有档位都走它（一个开关搞定）
	s := &Server{cfg: Config{DefaultModel: "opencode-go/big-pro"}}
	ref, err := s.resolveAnthropicModel(context.Background(), "claude-3-5-haiku", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/big-pro" {
		t.Errorf("配了默认模型时 haiku 应走默认，got %s", ref)
	}

	// 没配默认：按档位自动挑
	s2 := &Server{cfg: Config{}}
	ref, err = s2.resolveAnthropicModel(context.Background(), "claude-3-5-haiku", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/cheap-flash" {
		t.Errorf("未配默认时 haiku 应挑最便宜，got %s", ref)
	}

	// 显式 BRIDGE_MODEL_MAP 优先于一切
	s3 := &Server{cfg: Config{
		DefaultModel: "opencode-go/big-pro",
		ModelMap:     map[string]string{"claude-*": "opencode-go/cheap-flash"},
	}}
	ref, err = s3.resolveAnthropicModel(context.Background(), "claude-3-5-haiku", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/cheap-flash" {
		t.Errorf("显式映射应优先，got %s", ref)
	}
}

func TestResolveAnthropicModelPrefersRuntimeDefault(t *testing.T) {
	list := []OCModel{
		mkModel("opencode", "free", 0, 0, true, true),
		mkModel("opencode-go", "cheap-flash", 0.1, 0.2, true, true),
		mkModel("opencode-go", "big-pro", 5, 10, true, true),
	}
	// 面板选择（存 DB，含思考档位）优先于 BRIDGE_DEFAULT_MODEL
	s := &Server{cfg: Config{DefaultModel: "opencode-go/big-pro"}, log: NewLogger("error")}
	s.setRuntimeDefaultModel("opencode-go/cheap-flash:high")
	ref, err := s.resolveAnthropicModel(context.Background(), "claude-sonnet-4-5", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/cheap-flash:high" {
		t.Errorf("面板选择应优先（含思考档位），got %s", ref)
	}
	// 未设置运行时默认：仍走 BRIDGE_DEFAULT_MODEL
	s2 := &Server{cfg: Config{DefaultModel: "opencode-go/big-pro"}, log: NewLogger("error")}
	ref, err = s2.resolveAnthropicModel(context.Background(), "claude-sonnet-4-5", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/big-pro" {
		t.Errorf("未设 runtime 时 default 应优先，got %s", ref)
	}
	// 运行时默认选了未知 provider 的模型（解析失败）→ 回落到 BRIDGE_DEFAULT_MODEL
	s3 := &Server{cfg: Config{DefaultModel: "opencode-go/big-pro"}, log: NewLogger("error")}
	s3.setRuntimeDefaultModel("nosuch-provider/gone:max")
	ref, err = s3.resolveAnthropicModel(context.Background(), "claude-sonnet-4-5", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/big-pro" {
		t.Errorf("runtime 不可解析应回落 default，got %s", ref)
	}
}

// Anthropic 客户端回填工具结果时可能用自己生成的 tool_use_id（toolu_…），
// 与 janus 下发的 call_xxx 对不上 —— resumeAnthropic 改走与 OpenAI 一致的三级
// 对齐（精确 id → 工具名 → 顺序）。这里验证转换后的消息能支撑按名/序兜底。
func TestAnthropicToolResultAlignment(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"查天气"}]`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"tool_use","id":"toolu_01A","name":"get_weather","input":{"city":"北京"}}]`)},
		{Role: "user", Content: json.RawMessage(`[
			{"type":"tool_result","tool_use_id":"toolu_01A","content":"晴，26℃"}]`)},
	}
	converted, results := anthropicInputToMessages("", msgs)
	// 转换结果可按客户端的 toolu_ id 索引到结果…
	if _, ok := results["toolu_01A"]; !ok {
		t.Fatalf("results=%+v", results)
	}
	// …但 janus 的 pending id 是 call_xxx，精确匹配必然落空
	if _, ok := results["call_1"]; ok {
		t.Fatal("精确 id 不应命中")
	}
	// orderedToolResults 从转换后的消息里能拿到工具名 + 结果，供 ② 工具名 对齐兜底
	items := orderedToolResults(converted)
	if len(items) != 1 {
		t.Fatalf("items=%+v", items)
	}
	if items[0].Name != "get_weather" {
		t.Errorf("按名兜底需要工具名，got %q", items[0].Name)
	}
	if items[0].Result.Content != "晴，26℃" {
		t.Errorf("结果内容丢失：%+v", items[0].Result)
	}
}

func TestStripContextSuffix(t *testing.T) {
	cases := map[string]string{
		"mimo-v2.5-pro[1m]":                   "mimo-v2.5-pro",
		"mimo-v2.5-pro[1M]":                   "mimo-v2.5-pro",
		"opencode-go/deepseek-v4.1-flash[1m]": "opencode-go/deepseek-v4.1-flash",
		"claude-sonnet-4-5":                   "claude-sonnet-4-5",
		"weird[abc]":                          "weird[abc]",
		"[1m]":                                "[1m]",
	}
	for in, want := range cases {
		if got := stripContextSuffix(in); got != want {
			t.Errorf("stripContextSuffix(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMapModelNameStripsContextSuffix(t *testing.T) {
	// 无映射：[1m] 兜底去掉
	s := &Server{cfg: Config{}}
	if got := s.mapModelName("mimo-v2.5-pro[1m]"); got != "mimo-v2.5-pro" {
		t.Errorf("无映射 got %q", got)
	}
	// 有映射：先去掉后缀，再用前缀通配命中
	s2 := &Server{cfg: Config{ModelMap: map[string]string{"claude-*": "opencode-go/x"}}}
	if got := s2.mapModelName("claude-sonnet-4-5[1m]"); got != "opencode-go/x" {
		t.Errorf("有映射 got %q", got)
	}
}

func TestResolveAnthropicFullNameWithContextSuffix(t *testing.T) {
	list := []OCModel{mkModel("opencode-go", "mimo-v2.5-pro", 1, 2, true, true)}
	s := &Server{cfg: Config{}}
	// DS 式：CC 配完整模型名（含 [1m]），透传过来也要能解析
	ref, err := s.resolveAnthropicModel(context.Background(), "mimo-v2.5-pro[1m]", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/mimo-v2.5-pro" {
		t.Errorf("got %s", ref)
	}
}

func TestTierFallbackUsesLastUsedModel(t *testing.T) {
	list := []OCModel{
		mkModel("opencode", "grok-build-0.1", 1, 2, true, true),
		mkModel("opencode-go", "mimo-v2.5-pro", 1, 2, true, true),
	}
	s := &Server{cfg: Config{}}
	// 客户端先显式用了 mimo（CC 的主模型）
	if _, err := s.resolveModel(context.Background(), "mimo-v2.5-pro", list); err != nil {
		t.Fatalf("resolve explicit: %v", err)
	}
	// 之后 claude-* 别名应跟着上次用过的模型，而不是启发式乱挑
	ref, err := s.resolveAnthropicModel(context.Background(), "claude-sonnet-4-5", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref.String() != "opencode-go/mimo-v2.5-pro" {
		t.Errorf("got %s want opencode-go/mimo-v2.5-pro", ref)
	}

	// 显式 BRIDGE_DEFAULT_MODEL 仍然优先
	s2 := &Server{cfg: Config{DefaultModel: "opencode/grok-build-0.1"}}
	s2.rememberModel(OCModelRef{ProviderID: "opencode-go", ID: "mimo-v2.5-pro"})
	ref2, err := s2.resolveAnthropicModel(context.Background(), "claude-sonnet-4-5", list)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ref2.String() != "opencode/grok-build-0.1" {
		t.Errorf("default 应优先，got %s", ref2)
	}

	// 记忆的模型已不在列表 → 退回启发式
	s3 := &Server{cfg: Config{}}
	s3.rememberModel(OCModelRef{ProviderID: "opencode-go", ID: "gone"})
	if _, ok := s3.rememberedModel(list); ok {
		t.Error("已下线的模型不该被记住")
	}
}

func TestAnthropicModelsEndpoint(t *testing.T) {
	srv := newTestServerForRoutes(t)
	h := srv.Handler()

	get := func(path string, anthropic bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-api-key", "sk-test")
		if anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// /anthropic/v1/models 必须是 Anthropic 格式
	rec := get("/anthropic/v1/models", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v body=%s", err, rec.Body.String())
	}
	if len(got.Data) == 0 {
		t.Fatalf("没有返回任何模型: %s", rec.Body.String())
	}
	found := false
	for _, m := range got.Data {
		if m.Type != "model" || !strings.Contains(m.DisplayName, "→") {
			t.Errorf("条目格式不对: %+v", m)
		}
		if m.ID == "claude-sonnet-4-5" {
			found = true
		}
	}
	if !found {
		t.Errorf("缺少档位别名 claude-sonnet-4-5: %s", rec.Body.String())
	}
	if got.FirstID == "" || got.LastID == "" {
		t.Errorf("缺少 first_id/last_id: %s", rec.Body.String())
	}

	// 根路径 /v1/models + anthropic-version 头 → 也走 Anthropic 格式
	rec = get("/v1/models", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"display_name"`) {
		t.Errorf("anthropic 客户端访问 /v1/models 未返回 Anthropic 格式: %d %s", rec.Code, rec.Body.String())
	}

	// 普通 OpenAI 客户端（无 anthropic-version）→ 仍是 OpenAI 格式
	rec = get("/v1/models", false)
	if strings.Contains(rec.Body.String(), `"display_name"`) {
		t.Errorf("OpenAI 客户端不该拿到 Anthropic 格式: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Errorf("OpenAI 格式缺失: %s", rec.Body.String())
	}
}

func TestAnthropicWebSearchToolSpecs(t *testing.T) {
	specs := anthropicToolsToSpecs([]AnthropicTool{
		{Type: "web_search_20250305", Name: "web_search", MaxUses: 5},
		{Name: "get_weather"},
	})
	if len(specs) != 2 {
		t.Fatalf("specs=%+v", specs)
	}
	var ws *ToolSpec
	for i := range specs {
		if specs[i].Function.Name == webSearchToolName {
			ws = &specs[i]
		}
	}
	if ws == nil {
		t.Fatal("web_search 未暴露给 agent")
	}
	if !strings.Contains(string(ws.Function.Parameters), `"query"`) {
		t.Errorf("web_search schema 缺少 query: %s", ws.Function.Parameters)
	}
	if !anthropicHasWebSearch([]AnthropicTool{{Type: "web_search_20250305", Name: "web_search"}}) {
		t.Error("anthropicHasWebSearch 漏判")
	}
	if anthropicHasWebSearch([]AnthropicTool{{Name: "get_weather"}}) {
		t.Error("anthropicHasWebSearch 误判")
	}
}

// 桥自己执行的"服务端工具"（Claude Code 的 web_search）必须在 handleMCP 里内联执行，
// 既不能甩回客户端，也不能被"防串会话"守卫拒掉。
func TestHandleMCPExecutesServerToolInline(t *testing.T) {
	s := NewServer(Config{APIKey: "x"}, NewLogger("error"))
	sess := s.tools.Register("f:ws", []ToolSpec{
		{Type: "function", Function: ToolFunction{Name: webSearchToolName}},
	})
	sess.setServerTools(map[string]serverToolFunc{
		webSearchToolName: func(ctx context.Context, args string) (string, bool) {
			if !strings.Contains(args, "hello") {
				return "bad args", true
			}
			return "SEARCH_RESULT_OK", false
		},
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
		sess.mcpToolName(webSearchToolName) + `","arguments":{"query":"hello"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/x", strings.NewReader(body))
	req.SetPathValue("token", sess.token)
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SEARCH_RESULT_OK") {
		t.Fatalf("server tool 未内联执行: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no active request") {
		t.Fatalf("server tool 不该走防串会话拒绝: %s", rec.Body.String())
	}
}

func stubWebSearchUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/websearch") {
			_, _ = w.Write([]byte(`{"data":{"providerID":"parallel","results":[` +
				`{"url":"https://example.com/a","title":"Title A","content":"Body A"},` +
				`{"url":"https://example.com/b","title":"Title B","content":"Body B"}]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"_tag":"NotFoundError","message":"nope"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebSearchServerTool(t *testing.T) {
	stub := stubWebSearchUpstream(t)
	srv := NewServer(Config{
		Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", MaxBodyBytes: 1 << 20,
	}, NewLogger("error"))

	ctx := context.Background()
	results, provider, err := srv.up.WebSearch(ctx, "/tmp", "hello")
	if err != nil {
		t.Fatalf("WebSearch: %v", err)
	}
	if provider != "parallel" || len(results) != 2 || results[0].URL != "https://example.com/a" {
		t.Fatalf("results=%+v provider=%q", results, provider)
	}

	fn := srv.webSearchServerTool("/tmp")
	out, isErr := fn(ctx, `{"query":"hello"}`)
	if isErr {
		t.Fatalf("server tool 报错: %s", out)
	}
	if !strings.Contains(out, "https://example.com/a") || !strings.Contains(out, "Title B") {
		t.Errorf("server tool 输出缺少结果: %s", out)
	}

	// 缺 query → isError
	if _, isErr := fn(ctx, `{}`); !isErr {
		t.Error("缺少 query 应返回 isError")
	}
}
