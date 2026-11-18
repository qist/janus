// Package all imports every compiled plugin, so the binary registers them.
package all

import (
	_ "janus/plugins/antigravity"
	_ "janus/plugins/claude"
	_ "janus/plugins/codex"
	_ "janus/plugins/copilot"
	_ "janus/plugins/gemini"
	_ "janus/plugins/grok"
	_ "janus/plugins/workbuddy"
)
