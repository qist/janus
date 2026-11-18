package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"janus/internal/usage"
)

// usageRange resolves ?period=24h|7d|30d or explicit since/until Unix seconds.
// An explicit since/until pair wins so the console can ask for an arbitrary
// window; period only fills in what the caller left empty.
func usageRange(r *http.Request) (usage.Filter, bool) {
	query := r.URL.Query()
	filter := usage.Filter{
		KeyID:      strings.TrimSpace(query.Get("key")),
		GroupID:    strings.TrimSpace(query.Get("group")),
		WorkItemID: strings.TrimSpace(query.Get("work_item")),
		ProviderID: strings.TrimSpace(query.Get("provider")),
		Model:      strings.TrimSpace(query.Get("model")),
	}

	since, ok := parseUnix(query.Get("since"))
	if !ok {
		return usage.Filter{}, false
	}
	until, ok := parseUnix(query.Get("until"))
	if !ok {
		return usage.Filter{}, false
	}

	if since == 0 && until == 0 {
		now := time.Now()
		until = now.Unix()
		switch strings.TrimSpace(query.Get("period")) {
		case "24h":
			since = now.Add(-24 * time.Hour).Unix()
		case "30d":
			since = now.AddDate(0, 0, -30).Unix()
		case "7d", "":
			since = now.AddDate(0, 0, -7).Unix()
		default:
			since = now.AddDate(0, 0, -7).Unix()
		}
	}
	filter.Since, filter.Until = since, until
	return filter, true
}

// handleUsageStats returns totals plus the three breakdowns and a trend series
// in one round trip; the console needs all of them together.
func (s *Server) handleUsageStats(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Usage == nil {
		writeError(w, http.StatusNotImplemented, "usage_unavailable", "usage ledger is not configured")
		return
	}

	filter, ok := usageRange(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_time", "since/until must be Unix seconds")
		return
	}

	top := 10
	if raw := strings.TrimSpace(r.URL.Query().Get("top")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < -1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "invalid_top", "top must be between -1 and 100")
			return
		}
		top = parsed
	}

	stats, err := s.Usage.Stats(r.Context(), usage.StatsQuery{
		Filter: filter,
		Bucket: strings.TrimSpace(r.URL.Query().Get("bucket")),
		TopN:   top,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleUsageLogs returns the newest calls first. It exists so an operator can
// see which model/key produced a spike without a separate log stack.
func (s *Server) handleUsageLogs(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Usage == nil {
		writeError(w, http.StatusNotImplemented, "usage_unavailable", "usage ledger is not configured")
		return
	}
	filter, ok := usageRange(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_time", "since/until must be Unix seconds")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}

	entries, err := s.Usage.Recent(r.Context(), filter, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if entries == nil {
		entries = []usage.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
