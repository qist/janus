// Package ui 提供内置的 Web 用量面板。
//
// 页面本身是一张静态 HTML（编译进二进制，离线可用、零外部依赖），
// 数据由浏览器带着 API key 调 /v1/usage 和 /metrics 获取。
// 因此页面可以免鉴权：里面不含任何敏感信息，真正的数据端点仍然要 key。
package ui

import (
	_ "embed"
	"net/http"
)

//go:embed ui.html
var html []byte

// HTML 返回内嵌的面板页面内容。
func HTML() []byte { return html }

// Serve 写出面板页面（静态、免鉴权、不缓存）。
func Serve(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(html)
}
