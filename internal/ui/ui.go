// Package ui 提供内置的 Web 控制台（静态资源，随二进制 embed）。
//
// 前端是零构建的 Preact + htm：dist/index.html 用 import map 指向
// dist/vendor/ 下的三个 ESM 文件，浏览器直接执行，不需要 Node 构建链，
// 也不依赖任何 CDN（离线可用）。
//
// 静态资源本身免鉴权（不含敏感数据）；真正的数据端点 /api/v1/* 仍然要
// admin key，由前端放在 Authorization 头里。
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist
var distFS embed.FS

var assets fs.FS

func init() {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("ui: embed dist: " + err.Error())
	}
	assets = sub
}

// HTML 返回控制台入口页面。
func HTML() []byte {
	raw, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return []byte("<!doctype html><meta charset=utf-8><p>ui assets missing")
	}
	return raw
}

// Serve 写出控制台入口页面（静态、免鉴权、不缓存）。
func Serve(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(HTML())
}

// Handler 返回静态资源处理器，挂在 /assets/ 下。
//
// 资源名里带不带前导 / 都能处理（调用方通常已 StripPrefix）。
func Handler() http.Handler {
	fileServer := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		// 资源没有内容指纹：每次都回源校验，避免升级后浏览器用旧 JS。
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fileServer.ServeHTTP(w, r)
	})
}
