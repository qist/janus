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
