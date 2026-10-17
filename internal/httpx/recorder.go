// Package httpx 提供与业务无关的 HTTP 通用件：中间件、请求解析、
// 响应记录与写出。
//
// 这一层不认识 OpenAI / Anthropic 协议，也不认识 janus 的 Server，
// 只处理 http.Request / http.ResponseWriter 上的通用问题，
// 因此可以被任何端点层复用。
package httpx

import (
	"net/http"
	"strings"
)

// Recorder 在包装 ResponseWriter 的同时捕获状态码与写出的字节数，
// 供访问日志与指标使用。
type Recorder struct {
	http.ResponseWriter
	Status int
	Wrote  bool
	N      int64
}

func (r *Recorder) WriteHeader(code int) {
	if !r.Wrote {
		r.Status = code
		r.Wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *Recorder) Write(b []byte) (int, error) {
	if !r.Wrote {
		r.Status = http.StatusOK
		r.Wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.N += int64(n)
	return n, err
}

// Flush 必须透传，否则 SSE 无法工作。
func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LogPath 判断是否记录该路径。静态/探活端点（healthz、metrics、UI）噪音大，跳过。
func LogPath(p string) bool {
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
