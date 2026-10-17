package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPaths(t *testing.T) {
	if p := DefaultDBPath(); p == "" || !strings.HasSuffix(p, filepath.Join("janus", "janus.db")) {
		t.Fatalf("DefaultDBPath = %q", p)
	}
	if p := DefaultOpencodeDB(); p == "" || !strings.HasSuffix(p, filepath.Join("opencode", "opencode.db")) {
		t.Fatalf("DefaultOpencodeDB = %q", p)
	}
	if p := DefaultWorkspacesDir(); p == "" {
		t.Fatal("DefaultWorkspacesDir 为空")
	}
}
