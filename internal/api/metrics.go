package api

import (
	"net/http"
)

// ---------- HTTP ----------

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.MetricsPublic {
		if err := s.checkAuth(r); err != nil {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error(), "invalid_api_key")
			return
		}
	}
	if s.metrics == nil {
		http.Error(w, "metrics disabled", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.metrics.Render()))
}
