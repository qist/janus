package main

import (
	_ "embed"
	"net/http"
)

// ui.go —— 内置 Web 用量面板。
//
// 页面本身是一张静态 HTML（编译进二进制，离线可用、零外部依赖），
// 数据由浏览器带着 API key 调 /v1/usage 和 /metrics 获取。
// 因此页面可以免鉴权：里面不含任何敏感信息，真正的数据端点仍然要 key。

//go:embed web/ui.html
var uiHTML []byte

// handleUI 返回用量面板页面。
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(uiHTML)
}
