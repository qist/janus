package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- Web 用量面板 ----------

func TestUIPage(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0) // APIKey=sk-test

	// 页面本身免鉴权：它只是静态 HTML，不含数据；/v1/usage 仍要 key。
	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/ui 应 200，got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"用量面板", "/v1/usage", "/v1/models", "cache_read_tokens", "缓存命中率", "chartBox", "支持的模型", "catalogBody", "catalogFree", "仅免费", "data-copy", "复制模型名", "copyText"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
}

func TestUIRootRedirects(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("根路径应 302，got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui" {
		t.Fatalf("Location = %q, want /ui", loc)
	}
}

// 公开静态路径不受 BRIDGE_CORS_ORIGIN 限制：任意来源都能加载面板。
func TestCORSPublicPathsBypassRestrictiveList(t *testing.T) {
	stub := stubUpstream(t)
	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, NewLogger("error"))

	for _, path := range []string{"/ui", "/healthz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://other.example")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://other.example" {
			t.Errorf("%s 应放行任意来源，got CORS=%q status=%d", path, got, rec.Code)
		}
	}

	// 根路径的 302 也要带 CORS 头，浏览器才拿得到 Location。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://other.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://other.example" {
		t.Errorf("/ 应放行任意来源，got CORS=%q", got)
	}
}

// 公开路径的预检即使来源不在白名单也放行（面板本身无敏感数据）。
func TestCORSPreflightPublicPathAllowed(t *testing.T) {
	stub := stubUpstream(t)
	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, NewLogger("error"))

	req := httptest.NewRequest(http.MethodOptions, "/ui", nil)
	req.Header.Set("Origin", "https://other.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("/ui 预检应 204，got %d", rec.Code)
	}
}

// 数据端点绝不因为公开路径规则而被放开 CORS。
func TestCORSPublicPathsDoNotOpenV1(t *testing.T) {
	stub := stubUpstream(t)
	cfg := Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, NewLogger("error"))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://other.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("/v1/models 不该被放开 CORS，got %q", got)
	}
}
