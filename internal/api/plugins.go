package api

import (
	"net/http"

	"janus/internal/pluginrt"
)

// handleListPlugins reports what the compiled plugins did at startup: active,
// off, or failed with the reason. It never returns their configuration values,
// which may hold an operator's client id.
func (s *Server) handleListPlugins(w http.ResponseWriter, _ *http.Request, _ Identity) {
	plugins := pluginrt.LastStatus()
	if plugins == nil {
		plugins = []pluginrt.Status{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": plugins})
}
