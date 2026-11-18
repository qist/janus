// Package ui serves the built React console from internal/ui/dist. The assets
// are embedded so `go build` does not need Node; run `make ui` after changing
// web/ to refresh them.
//
// The console is a hash-router SPA: the server only needs to return index.html
// for /ui/ and the hashed assets under /ui/assets/. Authentication stays at the
// API layer: the page asks the operator for an API key and sends it as a header.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler returns the console routes. Mount it at /ui and /ui/ on the main mux.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("ui: embedded dist missing: " + err.Error())
	}

	files := http.StripPrefix("/ui/", http.FileServer(http.FS(sub)))

	mux := http.NewServeMux()
	mux.Handle("GET /ui/assets/", immutable(files))
	mux.HandleFunc("GET /ui/", serveIndex(sub))
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	return mux
}

func serveIndex(sub fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "前端资源缺失，请先构建 web/（make ui）", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 入口 HTML 必须随二进制更新立即生效，否则旧 index.html 会引用已删除的
		// hash 资源，浏览器表现为白屏。
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}

// immutable is safe for Vite's content-hashed filenames.
func immutable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		next.ServeHTTP(w, r)
	})
}
