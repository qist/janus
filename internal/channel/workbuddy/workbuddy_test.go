package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"janus/internal/config"
	"janus/internal/logx"
)

// 明文凭据两种形态都能解析；加密文件（无字符串 accessToken）明确报错。
func TestParseWBCredential(t *testing.T) {
	// 插件自留副本（扁平）
	flat := `{"accessToken":"at","refreshToken":"rt","domain":"workbuddy.ai","uid":"u1"}`
	c, err := parseCredential(flat)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "at" || c.RefreshToken != "rt" || c.region() != "global" || c.UID != "u1" {
		t.Fatalf("flat = %+v", c)
	}
	// 桌面明文（嵌套）
	nested := `{"auth":{"accessToken":"at2","refreshToken":"rt2","domain":"copilot.tencent.com"},"identity":{"uid":"u2","enterpriseId":"e1"}}`
	c2, err := parseCredential(nested)
	if err != nil {
		t.Fatal(err)
	}
	if c2.region() != "cn" || c2.UID != "u2" || c2.EnterpriseID != "e1" {
		t.Fatalf("nested = %+v", c2)
	}
	// 加密文件：accessToken 是对象 → 明确报错
	enc := `{"accessToken":{"v10":"..."},"auth":{"accessToken":{"v10":"..."}}}`
	if _, err := parseCredential(enc); err == nil || !strings.Contains(err.Error(), "accessToken") {
		t.Fatalf("加密文件应报 accessToken 缺失: %v", err)
	}
	// 空 domain 默认国内版
	c3, err := parseCredential(`{"accessToken":"x"}`)
	if err != nil || c3.region() != "cn" {
		t.Fatalf("empty domain = %+v %v", c3, err)
	}
}

// body 规整：强制 stream、developer→system、tool_choice 拍平、去 off 档。
func TestWBPrepareBody(t *testing.T) {
	src := `{"model":"m","stream":false,"reasoning_effort":"off","tool_choice":{"type":"function","function":{"name":"Read"}},"messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`
	out := string(prepareBody([]byte(src)))
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
	out2 := string(prepareBody([]byte(src2)))
	if strings.Contains(out2, "tool_choice") || strings.Contains(out2, "tools") {
		t.Fatalf("none 应连带删 tools: %s", out2)
	}
}

// 错误分类：状态码 + 中英文体标记。
func TestWBClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errKind
	}{
		{402, "", kindHardCredit},
		{401, "", kindSessionDead},
		{429, "", kindSoftRate},
		{404, "", kindNotFound},
		{500, "", kindServer},
		{400, "{}", kindClient},
		{200, "积分不足", kindHardCredit},
		{403, `{"msg":"12153"}`, kindSessionDead},
	}
	for _, c := range cases {
		if got := classify(c.status, c.body); got != c.want {
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
	models, err := parseCatalog(doc)
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

	cred := &Credential{AccessToken: "at-1", RefreshToken: "rt-1", Domain: "workbuddy.ai", UID: "u9"}
	c := newWorkBuddyClient(cred, "5.6.2")
	c.baseOverride = up.URL
	rt := &Runtime{
		cfg:    &config.Config{MaxBodyBytes: 1 << 20},
		log:    logx.NewLogger("error"),
		token:  "tok-123",
		client: c,
		models: []Model{{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 200000}},
	}
	current = rt
	t.Cleanup(func() { current = nil })

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
	c2 := newWorkBuddyClient(&Credential{AccessToken: "stale", RefreshToken: "rt", Domain: "workbuddy.ai"}, "5.5.6")
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

// 倍率规范化：带单位词 / 纯单位词 / 大小写不敏感 / 空值。
func TestWBNormalizeCredits(t *testing.T) {
	cases := map[string]string{
		"x0.79 credits": "x0.79",
		"x0.79 Credits": "x0.79",
		"x0.79 credit":  "x0.79",
		"  x0.14  ":     "x0.14",
		"x0.00":         "x0.00",
		"credits":       "",
		"Credit":        "",
		"":              "",
		"   ":           "",
	}
	for in, want := range cases {
		if got := normalizeCredits(in); got != want {
			t.Fatalf("normalizeCredits(%q) = %q, want %q", in, got, want)
		}
	}
}

// 目录解析带出计费倍率（行顶层 credits）与思考元数据。
func TestWBParseCatalogCredits(t *testing.T) {
	doc := map[string]any{
		"code": 0,
		"data": map[string]any{
			"models": []any{
				map[string]any{"id": "m1", "credits": "x0.79 credits", "supportsReasoning": true,
					"reasoning": map[string]any{"supportedEfforts": []any{"low", "high", "max", "bogus"},
						"defaultEffort": "high", "canDisableThinking": true}},
				map[string]any{"id": "m2", "credits": "x0.00", "supportsReasoning": true,
					"reasoning": map[string]any{"effort": "medium", "canDisableThinking": false}},
				map[string]any{"id": "m3"},
				map[string]any{"id": "m4", "supportsReasoning": true, "onlyReasoning": true,
					"reasoning": map[string]any{"supportedEfforts": []any{"high"}}},
			},
			"agents": []any{
				map[string]any{"name": "cli", "models": []any{"m1", "m2", "m3", "m4"}},
			},
		},
	}
	models, err := parseCatalog(doc)
	if err != nil {
		t.Fatal(err)
	}
	if models[0].Credits != "x0.79" || models[1].Credits != "x0.00" || models[2].Credits != "" {
		t.Fatalf("倍率解析不符: %+v", models)
	}
	// m1：声明档位过滤掉未知拼写，default/canDisable 带出。
	if !models[0].Reasoning || len(models[0].SupportedEfforts) != 3 ||
		models[0].DefaultEffort != "high" || !models[0].CanDisableThinking {
		t.Fatalf("m1 思考元数据不符: %+v", models[0])
	}
	// m2：老形态 effort 字段兜底。
	if models[1].DefaultEffort != "medium" || models[1].CanDisableThinking {
		t.Fatalf("m2 思考元数据不符: %+v", models[1])
	}
	// m3：无思考元数据。
	if models[2].Reasoning || models[2].SupportedEfforts != nil {
		t.Fatalf("m3 应无思考元数据: %+v", models[2])
	}
	// m4：onlyReasoning 标记。
	if !models[3].OnlyReasoning || len(models[3].SupportedEfforts) != 1 {
		t.Fatalf("m4 思考元数据不符: %+v", models[3])
	}
}

// variants 注入语义：声明档位注入/off 按 canDisable、非推理模型空表压档。
func TestWBModelConfigsVariants(t *testing.T) {
	rt := &Runtime{
		client: newWorkBuddyClient(&Credential{AccessToken: "at", Domain: ""}, ""),
		models: []Model{
			{ID: "declared", Name: "D", Reasoning: true, SupportedEfforts: []string{"low", "high", "max"}, DefaultEffort: "high", CanDisableThinking: true},
			{ID: "nodeclare", Name: "N", Reasoning: true},
			{ID: "plain", Name: "P"},
			{ID: "onlyre", Name: "O", Reasoning: true, SupportedEfforts: []string{"high"}, CanDisableThinking: true, OnlyReasoning: true},
		},
	}
	cfg := rt.modelConfigs()
	d := cfg["declared"].(map[string]any)
	v := d["variants"].(map[string]any)
	if _, ok := v["low"]; !ok {
		t.Fatalf("declared 缺 low 档: %v", v)
	}
	if v["low"].(map[string]any)["reasoningEffort"] != "low" {
		t.Fatalf("low 档 settings 不符: %v", v["low"])
	}
	if _, ok := v["off"]; !ok {
		t.Fatalf("canDisableThinking 应有 off 档: %v", v)
	}
	if _, ok := cfg["nodeclare"].(map[string]any)["variants"]; ok {
		t.Fatalf("未声明档位不应注入 variants")
	}
	p := cfg["plain"].(map[string]any)
	if em, ok := p["variants"].(map[string]any); !ok || len(em) != 0 {
		t.Fatalf("非推理模型应注入空 variants 压档: %v", p["variants"])
	}
	// CN 端 onlyReasoning：off 线上必拒，不给。
	o := cfg["onlyre"].(map[string]any)["variants"].(map[string]any)
	if _, ok := o["off"]; ok {
		t.Fatalf("onlyReasoning 不应给 off 档: %v", o)
	}
}

// 积分查询（个人套餐路径）：信封逐层、remain 选取、到期分桶、身份头与请求体。
func TestWBFetchPersonalCredits(t *testing.T) {
	var gotAuth, gotUser, gotTenant, gotDomain, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/get-user-resource" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotUser = r.Header.Get("X-User-Id")
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotDomain = r.Header.Get("X-Domain")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		// 两个套餐：一个周期制（size>0 → 取 CycleCapacityRemain），一个总量制。
		// 到期时间取 now+6h，保证 24h/3d/7d 分桶在任意运行日期都稳定命中。
		endTime := time.Now().Add(6 * time.Hour).Format("2006-01-02 15:04:05")
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"Accounts": []any{
							map[string]any{
								"PackageName":         "月度套餐",
								"CycleCapacitySize":   1000,
								"CycleCapacityUsed":   300,
								"CycleCapacityRemain": 700,
								"CycleEndTime":        endTime,
							},
							map[string]any{
								"PackageName":    "永久包",
								"CapacitySize":   500,
								"CapacityRemain": 250,
							},
						},
					},
				},
			},
		})
	}))
	defer up.Close()

	// 全局版个人账户（不带企业号，走套餐列表端点）。
	cred := &Credential{AccessToken: "at", UID: "u1", Domain: "workbuddy.ai"}
	c := newWorkBuddyClient(cred, "")
	c.baseOverride = up.URL
	credits, err := c.fetchCredits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer at" || gotUser != "u1" || gotTenant != "" || gotDomain != "workbuddy.ai" {
		t.Fatalf("计费身份头不符: auth=%q user=%q tenant=%q domain=%q", gotAuth, gotUser, gotTenant, gotDomain)
	}
	if !strings.Contains(gotBody, `"ProductCode":"p_tcaca"`) {
		t.Fatalf("请求体缺 ProductCode: %s", gotBody)
	}
	if credits.Total != 950 {
		t.Fatalf("总额应 700+250=950: %+v", credits)
	}
	if len(credits.Accounts) != 2 || credits.Accounts[0].Remain != 700 || credits.Accounts[1].Remain != 250 {
		t.Fatalf("套餐明细不符: %+v", credits.Accounts)
	}
	if credits.Accounts[0].Size != 1000 || credits.Accounts[1].Size != 500 {
		t.Fatalf("容量不符: %+v", credits.Accounts)
	}
	// 到期分桶：唯一带到期时间的套餐落在所有窗口内，永久包不参与。
	if credits.Expiry == nil || credits.Expiry.Within24h.Packages != 1 || credits.Expiry.Within24h.Total != 700 {
		t.Fatalf("24h 到期分桶不符: %+v", credits.Expiry)
	}
	if credits.Expiry.Within7d.Total != 700 || credits.Expiry.Within3d.Total != 700 {
		t.Fatalf("3d/7d 到期分桶不符: %+v", credits.Expiry)
	}
}

// 积分查询（CN 企业路径）：limit/used 双拼写、-1 无上限、缺用量字段硬报错。
func TestWBFetchEnterpriseCredits(t *testing.T) {
	cred := &Credential{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	c := newWorkBuddyClient(cred, "")

	// ① 正常：data.data 读位 + limit_num/used_num 拼写。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/get-enterprise-user-usage" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"data":           map[string]any{"limit_num": 1000, "used_num": 400, "cycleResetTime": "2026-11-01 00:00:00"},
				"cycleResetTime": "",
			},
		})
	}))
	c.baseOverride = up.URL
	credits, err := c.fetchCredits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	up.Close()
	if credits.Total != 600 || len(credits.Accounts) != 1 || credits.Accounts[0].Package != "enterprise" {
		t.Fatalf("企业余量不符: %+v", credits)
	}
	if credits.CycleResetTime != "2026-11-01 00:00:00" {
		t.Fatalf("重置时间不符: %+v", credits)
	}

	// ② -1 = 无上限。
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"limitNum": -1}})
	}))
	c.baseOverride = up2.URL
	credits, err = c.fetchCredits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	up2.Close()
	if !credits.Unlimited || credits.Total != 0 {
		t.Fatalf("无上限形态不符: %+v", credits)
	}

	// ③ 有上限无用量 → 硬报错（宁可报错也不渲染 0）。
	up3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"limitNum": 1000}})
	}))
	c.baseOverride = up3.URL
	if _, err = c.fetchCredits(context.Background()); err == nil {
		t.Fatal("缺用量字段应报错")
	}
	up3.Close()
}
