package work

import (
	"path/filepath"
	"strings"
)

// PathsOverlap reports whether any prefix in a overlaps any prefix in b.
// A prefix covers itself and everything below it, so "internal/work" overlaps
// "internal/work/board.go" but not "internal/worker".
func PathsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if pathOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

func pathOverlap(a, b string) bool {
	a = normalizePath(a)
	b = normalizePath(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.ToSlash(filepath.Clean(p))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	if p == "." {
		return ""
	}
	return p
}
