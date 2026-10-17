package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"janus/internal/config"
	"janus/internal/logx"
)

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
	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		MetricsPublic: true, MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, logx.NewLogger("error"))

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
	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "*", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, logx.NewLogger("error"))

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
	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example, https://also.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, logx.NewLogger("error"))

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
