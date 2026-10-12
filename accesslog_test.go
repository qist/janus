package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 访问日志必须拿到真实状态码与字节数，否则排查时会误导（尤其 SSE 提前断流）。
func TestAccessLogRecordsStatusAndBytes(t *testing.T) {
	s := &Server{log: NewLogger("error")}
	h := s.withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("hello world"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应透传，got %d", rec.Code)
	}
	if rec.Body.String() != "hello world" {
		t.Fatalf("响应体应透传，got %q", rec.Body.String())
	}
}

// 静态/探活端点不记录，避免刷屏。
func TestAccessLogPathFilter(t *testing.T) {
	for _, p := range []string{"/healthz", "/metrics", "/ui", "/", "/favicon.ico", "/assets/app.js"} {
		if accessLogPath(p) {
			t.Errorf("%s 应被跳过", p)
		}
	}
	for _, p := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages", "/mcp/abc"} {
		if !accessLogPath(p) {
			t.Errorf("%s 应记录", p)
		}
	}
}
