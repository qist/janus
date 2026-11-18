// Package ui serves the embedded human console. It is static only: the page
// calls /api/v1 with an API key typed by the operator, so the assets can stay
// public and the server keeps no session or cookie.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Handler returns the console routes. Mount it at /ui and /ui/ on the main mux.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("ui: embedded assets missing: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.Handle("GET /ui/static/", noCache(http.StripPrefix("/ui/static/", http.FileServer(http.FS(sub)))))
	mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		raw, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "ui unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	return mux
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
