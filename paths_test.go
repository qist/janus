package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPaths(t *testing.T) {
	if p := defaultDBPath(); p == "" || !strings.HasSuffix(p, filepath.Join("janus", "janus.db")) {
		t.Fatalf("defaultDBPath = %q", p)
	}
	if p := defaultOpencodeDB(); p == "" || !strings.HasSuffix(p, filepath.Join("opencode", "opencode.db")) {
		t.Fatalf("defaultOpencodeDB = %q", p)
	}
	if p := defaultWorkspacesDir(); p == "" {
		t.Fatal("defaultWorkspacesDir 为空")
	}
}
