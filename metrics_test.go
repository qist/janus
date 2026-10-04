package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 指标 ----------

func TestMetricsRender(t *testing.T) {
	m := newMetrics(func() int { return 7 })
	m.observeRequest("/v1/models", 200, 12*time.Millisecond)
	m.observeRequest("/v1/models", 500, 3*time.Second)
	m.observeRequest("/v1/chat/completions", 200, 100*time.Millisecond)
	m.incRateLimited()
	m.incToolReg()
	m.incToolCalls(2)
	m.addTokens(10, 20)
	m.streamStart()

	out := m.render()
	for _, want := range []string{
		"opencode_bridge_up 1",
		`opencode_bridge_requests_total{endpoint="/v1/models",status="200"} 1`,
		`opencode_bridge_requests_total{endpoint="/v1/models",status="500"} 1`,
		"opencode_bridge_rate_limited_total 1",
		"opencode_bridge_tool_registrations_total 1",
		"opencode_bridge_tool_calls_total 2",
		"opencode_bridge_active_streams 1",
		"opencode_bridge_conversations 7",
		`opencode_bridge_tokens_total{direction="input"} 10`,
		`opencode_bridge_tokens_total{direction="output"} 20`,
		`opencode_bridge_request_duration_seconds_count{endpoint="/v1/models"} 2`,
		"opencode_bridge_request_duration_seconds_bucket",
		`le="+Inf"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("指标输出缺少 %q\n%s", want, out)
		}
	}
	// 直方图桶必须单调不减
	if !strings.Contains(out, `le="0.05"} 1`) {
		t.Errorf("12ms 应落在 0.05 桶:\n%s", out)
	}
}

func TestMetricsNilSafe(t *testing.T) {
	var m *metrics
	m.observeRequest("/x", 200, time.Millisecond)
	m.incRateLimited()
	m.incUpstreamErr()
	m.incToolReg()
	m.incToolCalls(1)
	m.addTokens(1, 1)
	m.streamStart()
	m.streamEnd()
	if m.render() != "" {
		t.Fatal("nil metrics 应渲染为空")
	}
}

func TestMetricEndpointNormalization(t *testing.T) {
	cases := map[string]string{
		"/healthz":               "/healthz",
		"/metrics":               "/metrics",
		"/ui":                    "/ui",
		"/":                      "/",
		"/v1/models":             "/v1/models",
		"/v1/models/opencode/x":  "/v1/models/{id}",
		"/v1/chat/completions":   "/v1/chat/completions",
		"/v1/responses":          "/v1/responses",
		"/v1/responses/resp_abc": "/v1/responses/{id}",
		"/mcp/deadbeef":          "/mcp/{token}",
		"/v1/unknown":            "other",
	}
	for in, want := range cases {
		if got := metricEndpoint(in); got != want {
			t.Errorf("metricEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetricsEndpointAuth(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0) // 复用：APIKey=sk-test

	// 默认需要鉴权
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("默认应 401，got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "opencode_bridge_up") {
		t.Fatalf("带 key 应 200，got %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestMetricsPublic(t *testing.T) {
	stub := stubUpstream(t)
	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		MetricsPublic: true, MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, NewLogger("error"))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public 时应免鉴权，got %d", rec.Code)
	}
}

func TestMetricsCountsRequests(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0)
	doModels(srv, "sk-test")
	doModels(srv, "sk-test")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `opencode_bridge_requests_total{endpoint="/v1/models",status="200"} 2`) {
		t.Fatalf("请求计数不对:\n%s", rec.Body.String())
	}
}

// ---------- CORS 默认收紧 ----------

func TestCORSDefaultDenies(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0) // CORSOrigin 为空
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("默认不该回 CORS 头，got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("默认不该回 credentials，got %q", got)
	}
}

func TestCORSWildcardNoCredentials(t *testing.T) {
	stub := stubUpstream(t)
	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "*", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, NewLogger("error"))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://any.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("通配应回 *，got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatal("通配模式下不该带 credentials（浏览器会拒绝）")
	}
}

func TestCORSExplicitAllowList(t *testing.T) {
	stub := stubUpstream(t)
	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example, https://also.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, NewLogger("error"))

	// 允许的 origin
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://ok.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ok.example" {
		t.Fatalf("应回显允许的 origin，got %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("显式允许时应带 credentials")
	}

	// 未允许的 origin
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("未允许的 origin 不该回 CORS 头，got %q", got)
	}
}

func TestCORSPreflightDenied(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0) // 默认拒绝
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("被拒的预检应 403，got %d", rec.Code)
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0)
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("无 Origin 的预检应 204，got %d", rec.Code)
	}
}
