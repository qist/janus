package gemini

import (
	"fmt"

	"janus/internal/pluginrt"
)

// errMissingSecret is surfaced as needs_config in /api/v1/plugins instead of a
// 400 at the token endpoint.
func errMissingSecret() error {
	return fmt.Errorf("%w: client_secret for gemini", pluginrt.ErrNeedsConfig)
}
