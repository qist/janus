package main

import (
	"net/http"
	"strings"
	"time"
)

// accessRecorder 在 statusRecorder 基础上统计写出的字节数，供访问日志使用。
type accessRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
	n      int64
}

func (r *accessRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *accessRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.n += int64(n)
	return n, err
}

// Flush 必须透传，否则 SSE 无法工作。
func (r *accessRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withAccessLog 记录每个进入 mux 的请求。耗时对流式端点是"直到流结束"，
// 正好用来定位客户端提前断流（表现为 llm response empty stream）。
// 静态/探活端点（healthz、metrics、UI）噪音大，跳过。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accessLogPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &accessRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		ua := r.Header.Get("User-Agent")
		if len(ua) > 60 {
			ua = ua[:60]
		}
		s.log.Infof("http: %s %s status=%d dur=%s bytes=%d ip=%q ua=%q",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond), rec.n, clientIP(r), ua)
	})
}

// accessLogPath 判断是否记录该路径。
func accessLogPath(p string) bool {
	switch p {
	case "/healthz", "/metrics", "/ui", "/", "/favicon.ico":
		return false
	}
	// UI 静态资源。
	if strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/static/") {
		return false
	}
	return true
}
