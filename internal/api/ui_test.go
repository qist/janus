package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"janus/internal/config"
	"janus/internal/logx"
)

// ---------- Web 用量面板 ----------

func TestUIPage(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0) // APIKey=sk-test

	// 页面本身免鉴权：它只是静态 HTML，不含数据；/api/v1/* 仍要 key。
	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/ui 应 200，got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("入口页不该被缓存，Cache-Control = %q", cc)
	}
	// 入口只负责加载外壳：真正的页面在 /assets/ 下的 ESM 里。
	for _, want := range []string{
		"Janus 控制台",
		`type="importmap"`,
		"/assets/vendor/preact.module.js",
		"/assets/vendor/hooks.module.js",
		"/assets/vendor/htm.module.js",
		"/assets/app.js",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
}

// 静态资源必须真的能被加载：任一 404 都会让整站白屏。
func TestUIAssetsServed(t *testing.T) {
	srv := newRateLimitServer(t, 0, 0, 0)

	for _, path := range []string{
		"/assets/app.js",
		"/assets/api.js",
		"/assets/components.js",
		"/assets/pages/overview.js",
		"/assets/pages/settings.js",
		"/assets/pages/requests.js",
		"/assets/pages/sessions.js",
		"/assets/pages/keys.js",
		"/assets/pages/agents.js",
		"/assets/vendor/preact.module.js",
		"/assets/vendor/hooks.module.js",
		"/assets/vendor/htm.module.js",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 应 200，got %d", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("%s Content-Type = %q", path, ct)
		}
	}

	// 目录穿越不能读到 dist 之外的东西。
	req := httptest.NewRequest(http.MethodGet, "/assets/../ui.go", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("路径穿越应被拒绝，got 200")
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
	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, logx.NewLogger("error"))

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
	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, logx.NewLogger("error"))

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
	cfg := config.Config{Upstream: stub.URL, Username: "o", Password: "p",
		Directory: "/tmp", APIKey: "sk-test", ConsoleURL: stub.URL,
		CORSOrigin: "https://ok.example", MaxBodyBytes: 1 << 20}
	srv := NewServer(cfg, logx.NewLogger("error"))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://other.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("/v1/models 不该被放开 CORS，got %q", got)
	}
}
