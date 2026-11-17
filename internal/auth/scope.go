package auth

import (
	"path/filepath"
	"strings"
)

// PathAllowed reports whether target is inside one of the allowed prefixes.
// Paths are cleaned first, so ".." cannot escape a prefix.
func PathAllowed(prefixes []string, target string) bool {
	target = filepath.Clean(target)
	for _, prefix := range prefixes {
		prefix = filepath.Clean(prefix)
		if prefix == "." || prefix == "" {
			continue
		}
		if target == prefix {
			return true
		}
		if strings.HasPrefix(target, prefix+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
