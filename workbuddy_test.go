package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 明文凭据两种形态都能解析；加密文件（无字符串 accessToken）明确报错。
func TestParseWBCredential(t *testing.T) {
	// 插件自留副本（扁平）
	flat := `{"accessToken":"at","refreshToken":"rt","domain":"workbuddy.ai","uid":"u1"}`
	c, err := parseWBCredential(flat)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "at" || c.RefreshToken != "rt" || c.region() != "global" || c.UID != "u1" {
		t.Fatalf("flat = %+v", c)
	}
	// 桌面明文（嵌套）
	nested := `{"auth":{"accessToken":"at2","refreshToken":"rt2","domain":"copilot.tencent.com"},"identity":{"uid":"u2","enterpriseId":"e1"}}`
	c2, err := parseWBCredential(nested)
	if err != nil {
		t.Fatal(err)
	}
	if c2.region() != "cn" || c2.UID != "u2" || c2.EnterpriseID != "e1" {
		t.Fatalf("nested = %+v", c2)
	}
	// 加密文件：accessToken 是对象 → 明确报错
	enc := `{"accessToken":{"v10":"..."},"auth":{"accessToken":{"v10":"..."}}}`
	if _, err := parseWBCredential(enc); err == nil || !strings.Contains(err.Error(), "accessToken") {
		t.Fatalf("加密文件应报 accessToken 缺失: %v", err)
	}
	// 空 domain 默认国内版
	c3, err := parseWBCredential(`{"accessToken":"x"}`)
	if err != nil || c3.region() != "cn" {
		t.Fatalf("empty domain = %+v %v", c3, err)
	}
}

// body 规整：强制 stream、developer→system、tool_choice 拍平、去 off 档。
func TestWBPrepareBody(t *testing.T) {
	src := `{"model":"m","stream":false,"reasoning_effort":"off","tool_choice":{"type":"function","function":{"name":"Read"}},"messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`
	out := string(wbPrepareBody([]byte(src)))
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream 应强制 true")
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Error("off 档应被删除")
	}
	if obj["tool_choice"] != "Read" {
		t.Fatalf("tool_choice 应拍平为函数名: %v", obj["tool_choice"])
	}
	msgs := obj["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Error("developer 应改写为 system")
	}
	// tool_choice=none 应同时删掉 tools
	src2 := `{"tool_choice":"none","tools":[{"a":1}]}`
	out2 := string(wbPrepareBody([]byte(src2)))
	if strings.Contains(out2, "tool_choice") || strings.Contains(out2, "tools") {
		t.Fatalf("none 应连带删 tools: %s", out2)
	}
}

// 错误分类：状态码 + 中英文体标记。
func TestWBClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   wbErrKind
	}{
		{402, "", wbKindHardCredit},
		{401, "", wbKindSessionDead},
		{429, "", wbKindSoftRate},
		{404, "", wbKindNotFound},
		{500, "", wbKindServer},
		{400, "{}", wbKindClient},
		{200, "积分不足", wbKindHardCredit},
		{403, `{"msg":"12153"}`, wbKindSessionDead},
	}
	for _, c := range cases {
		if got := wbClassify(c.status, c.body); got != c.want {
			t.Fatalf("classify(%d,%q)=%s want %s", c.status, c.body, got, c.want)
		}
	}
}

// 目录解析：兼容裸文档与信封，按 cli agent 的 models 列表取权威顺序。
func TestWBParseCatalog(t *testing.T) {
	doc := map[string]any{
		"code": 0,
		"data": map[string]any{
			"models": []any{
				map[string]any{"id": "glm-5.3", "name": "GLM-5.3", "maxInputTokens": 200000, "maxOutputTokens": 32000, "supportsImages": true},
				map[string]any{"id": "glm-5.1", "maxInputTokens": 100000, "maxOutputTokens": 8000, "disabled": true},
				map[string]any{"id": "glm-5.3-flash", "maxInputTokens": 130000, "maxOutputTokens": 16000},
			},
			"agents": []any{
				map[string]any{"name": "build", "models": []any{"glm-5.3"}},
				map[string]any{"name": "cli", "models": []any{"glm-5.3-flash", "glm-5.3"}},
			},
		},
	}
	models, err := wbParseCatalog(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("应取 cli 列表顺序的 2 个（跳过 disabled）: %+v", models)
	}
	if models[0].ID != "glm-5.3-flash" || models[1].ID != "glm-5.3" {
		t.Fatalf("顺序应按 cli 列表: %+v", models)
	}
	if !models[1].SupportsImages || models[1].ContextWindow != 200000 {
		t.Fatalf("字段解析不符: %+v", models[1])
	}
}

// 端到端：httptest 假上游 + 真路由。models 走缓存目录，chat 透传 SSE 并校验身份头。
func TestWorkBuddyProviderRoutes(t *testing.T) {
	// 假上游：记录请求头，回一段 SSE
	var gotAuth, gotUA, gotConv, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotConv = r.Header.Get("X-Conversation-ID")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	cred := &wbCredential{AccessToken: "at-1", RefreshToken: "rt-1", Domain: "workbuddy.ai", UID: "u9"}
	c := newWorkBuddyClient(cred, "5.6.2")
	c.baseOverride = up.URL
	rt := &workbuddyRuntime{
		cfg:    &Config{MaxBodyBytes: 1 << 20},
		log:    NewLogger("error"),
		token:  "tok-123",
		client: c,
		models: []wbModel{{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 200000}},
	}
	wbRuntime = rt
	t.Cleanup(func() { wbRuntime = nil })

	mux := http.NewServeMux()
	rt.registerRoutes(mux)

	// 错 token → 401
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/workbuddy/wrong/v1/chat/completions", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401, got %d", rec.Code)
	}

	// models
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/workbuddy/tok-123/v1/models", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "glm-5.3") {
		t.Fatalf("models: %d %s", rec.Code, rec.Body.String())
	}

	// chat：SSE 原样透传 + 身份头/body 规整生效
	body := `{"model":"glm-5.3","stream":false,"messages":[{"role":"developer","content":"s"},{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/workbuddy/tok-123/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("SSE 未透传: %s", rec.Body.String())
	}
	if gotAuth != "Bearer at-1" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotUA, "WorkBuddy/5.6.2 WorkBuddy AI/5.6.2") {
		t.Fatalf("UA = %q", gotUA)
	}
	if gotConv == "" {
		t.Fatal("X-Conversation-ID 缺失")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true || sent["tool_choice"] != "auto" {
		t.Fatalf("body 规整不符: %s", gotBody)
	}
	if msgs := sent["messages"].([]any); msgs[0].(map[string]any)["role"] != "system" {
		t.Fatal("developer 应已改写")
	}

	// 上游 401 → 自动续期后重试
	up2Hits := 0
	refreshHits := 0
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			refreshHits++
			w.Write([]byte(`{"code":0,"data":{"accessToken":"at-2"}}`))
			return
		}
		up2Hits++
		if up2Hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"msg":"expired"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"after-refresh\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up2.Close()
	c2 := newWorkBuddyClient(&wbCredential{AccessToken: "stale", RefreshToken: "rt", Domain: "workbuddy.ai"}, "5.5.6")
	c2.baseOverride = up2.URL
	rt.client = c2

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/workbuddy/tok-123/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "after-refresh") {
		t.Fatalf("401 续期重试失败: %d %s", rec.Code, rec.Body.String())
	}
	if refreshHits != 1 || up2Hits != 2 {
		t.Fatalf("refresh=%d chat=%d", refreshHits, up2Hits)
	}
	if c2.credential().AccessToken != "at-2" {
		t.Fatalf("新 token 未生效: %q", c2.credential().AccessToken)
	}
	_ = context.Background()
}
