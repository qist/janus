// Package all imports every compiled plugin, so the binary registers them.
package all

import (
	_ "janus/plugins/claude"
	_ "janus/plugins/codex"
	_ "janus/plugins/copilot"
)
