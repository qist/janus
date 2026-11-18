// Package api exposes the work board over HTTP. It is a thin transport layer:
// every rule lives in worksvc/work, this package only decodes requests,
// authenticates the caller, calls one service method and maps errors to
// status codes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"janus/internal/goals"
	"janus/internal/policy"
	"janus/internal/provider"
	"janus/internal/scoring"
	"janus/internal/usage"
	"janus/internal/work"
	"janus/internal/worksvc"
)

const (
	// DefaultMaxBodyBytes caps request bodies. Evidence can be sizable but not
	// unbounded; larger artifacts belong in object storage, not in a submit.
	DefaultMaxBodyBytes int64 = 1 << 20

	headerKey = "X-API-Key"
)

// Server is the HTTP front end of the work board.
type Server struct {
	Svc  *worksvc.Service
	Auth Authenticator
	// RunnerFor returns the check runner for an item. When nil, /verify
	// answers 501 instead of silently doing nothing.
	RunnerFor func(ctx context.Context, item work.WorkItem) (work.CheckRunner, error)
	// MaxBodyBytes overrides DefaultMaxBodyBytes when positive.
	MaxBodyBytes int64
	// Logf receives server-side failures; nil means log.Printf.
	Logf func(format string, args ...any)
	// Usage is the spend ledger exposed at /api/v1/usage; nil answers 501.
	Usage usage.Ledger
	// Scores is exposed read-only at /api/v1/scores; nil answers 501.
	Scores *scoring.Engine
	// Goals is the goal service; nil answers 501 on goal routes.
	Goals *goals.Service
	// Providers manages upstream providers and credentials; nil answers 501.
	Providers *provider.Service
	// ReloadModels rebuilds the proxy route table after a provider change.
	// Failures are logged rather than returned: the write already succeeded and
	// the previous table keeps serving.
	ReloadModels func(ctx context.Context) error
}

// New returns a server. A nil authenticator rejects every request.
func New(svc *worksvc.Service, auth Authenticator) *Server {
	return &Server{Svc: svc, Auth: auth}
}

// Handler returns a standalone handler that answers unknown paths with a JSON
// 404. Use Register when mounting next to another transport.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.recover(mux)
}

// Register mounts the work-board routes on mux. It deliberately registers no
// catch-all so a parent mux can also serve other transports.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/healthz", s.handleHealth)
	// Any authenticated key may ask who it is: the console uses this to hide
	// actions the key cannot perform instead of letting them 403.
	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	mux.HandleFunc("GET /api/v1/work-items", s.handleList)
	mux.HandleFunc("GET /api/v1/usage", s.protect(ScopeRead, s.handleUsage))
	mux.HandleFunc("GET /api/v1/scores", s.protect(ScopeRead, s.handleScores))
	mux.HandleFunc("GET /api/v1/providers", s.protect(ScopeRead, s.handleListProviders))
	mux.HandleFunc("POST /api/v1/providers", s.protect(ScopeAdmin, s.handleCreateProvider))
	mux.HandleFunc("PATCH /api/v1/providers/{id}", s.protect(ScopeAdmin, s.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/v1/providers/{id}", s.protect(ScopeAdmin, s.handleDeleteProvider))
	mux.HandleFunc("GET /api/v1/providers/{id}/credentials", s.protect(ScopeAdmin, s.handleListCredentials))
	mux.HandleFunc("POST /api/v1/providers/{id}/credentials", s.protect(ScopeAdmin, s.handleCreateCredential))
	mux.HandleFunc("POST /api/v1/providers/{id}/probe", s.protect(ScopeAdmin, s.handleProbeProvider))
	mux.HandleFunc("DELETE /api/v1/credentials/{id}", s.protect(ScopeAdmin, s.handleDeleteCredential))
	mux.HandleFunc("GET /api/v1/model-routes", s.protect(ScopeRead, s.handleListRoutes))
	mux.HandleFunc("POST /api/v1/model-routes", s.protect(ScopeAdmin, s.handleCreateRoute))
	mux.HandleFunc("DELETE /api/v1/model-routes/{id}", s.protect(ScopeAdmin, s.handleDeleteRoute))
	mux.HandleFunc("POST /api/v1/goals", s.protect(ScopeWrite, s.handleCreateGoal))
	mux.HandleFunc("GET /api/v1/goals", s.protect(ScopeRead, s.handleListGoals))
	mux.HandleFunc("GET /api/v1/goals/{id}", s.protect(ScopeRead, s.handleGetGoal))
	mux.HandleFunc("GET /api/v1/goals/{id}/events", s.protect(ScopeRead, s.handleGoalEvents))
	mux.HandleFunc("POST /api/v1/goals/{id}/cancel", s.protect(ScopeWrite, s.handleCancelGoal))
	mux.HandleFunc("POST /api/v1/work-items", s.protect(ScopeWrite, s.handleCreate))
	mux.HandleFunc("GET /api/v1/work-items/{id}", s.protect(ScopeRead, s.handleGet))
	mux.HandleFunc("GET /api/v1/work-items/{id}/handoff", s.protect(ScopeRead, s.handleHandoff))
	mux.HandleFunc("POST /api/v1/work-items/{id}/claim", s.protect(ScopeClaim, s.handleClaim))
	mux.HandleFunc("POST /api/v1/work-items/{id}/assign", s.protect(ScopeAdmin, s.handleAssign))
	mux.HandleFunc("POST /api/v1/work-items/{id}/heartbeat", s.protect(ScopeClaim, s.handleHeartbeat))
	mux.HandleFunc("POST /api/v1/work-items/{id}/release", s.protect(ScopeClaim, s.handleRelease))
	mux.HandleFunc("POST /api/v1/work-items/{id}/submit", s.protect(ScopeSubmit, s.handleSubmit))
	mux.HandleFunc("POST /api/v1/work-items/{id}/verify", s.protect(ScopeVerify, s.handleVerify))
	mux.HandleFunc("POST /api/v1/work-items/{id}/verdict", s.protect(ScopeVerify, s.handleVerdict))
	mux.HandleFunc("POST /api/v1/work-items/{id}/complete", s.protect(ScopeIntegrate, s.handleComplete))
	mux.HandleFunc("POST /api/v1/work-items/{id}/authorize", s.protect(ScopeAuthorize, s.handleAuthorize))
	mux.HandleFunc("POST /api/v1/work-items/{id}/cancel", s.protect(ScopeWrite, s.handleCancelItem))
}

// protect authenticates the caller and checks the required scope before the
// handler runs. Identity comes from the key, never from the request body.
func (s *Server) protect(scope Scope, next func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := s.authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
			return
		}
		if !id.Has(scope) {
			writeError(w, http.StatusForbidden, "forbidden", "key lacks scope "+string(scope))
			return
		}
		next(w, r, id)
	}
}

func (s *Server) authenticate(r *http.Request) (Identity, error) {
	if s.Auth == nil {
		return Identity{}, ErrUnauthenticated
	}
	key := strings.TrimSpace(r.Header.Get(headerKey))
	if key == "" {
		value := strings.TrimSpace(r.Header.Get("Authorization"))
		if after, ok := strings.CutPrefix(value, "Bearer "); ok {
			key = strings.TrimSpace(after)
		}
	}
	return s.Auth.Authenticate(r.Context(), key)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return
	}
	if id.Scopes == nil {
		id.Scopes = []Scope{}
	}
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return
	}
	if !id.Has(ScopeRead) {
		writeError(w, http.StatusForbidden, "forbidden", "key lacks scope "+string(ScopeRead))
		return
	}

	status := work.Status(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && !status.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_status", fmt.Sprintf("unknown status %q", status))
		return
	}

	items, err := s.Svc.List(r.Context(), status, strings.TrimSpace(r.URL.Query().Get("workspace")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if items == nil {
		items = []work.WorkItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleUsage returns aggregated spend. Filters: key, group, work_item,
// provider, model, credential, since, until (Unix seconds, inclusive).
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Usage == nil {
		writeError(w, http.StatusNotImplemented, "usage_unavailable", "usage ledger is not configured")
		return
	}

	query := r.URL.Query()
	filter := usage.Filter{
		KeyID:        strings.TrimSpace(query.Get("key")),
		GroupID:      strings.TrimSpace(query.Get("group")),
		WorkItemID:   strings.TrimSpace(query.Get("work_item")),
		ProviderID:   strings.TrimSpace(query.Get("provider")),
		Model:        strings.TrimSpace(query.Get("model")),
		CredentialID: strings.TrimSpace(query.Get("credential")),
	}

	since, ok := parseUnix(query.Get("since"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_time", "since must be Unix seconds")
		return
	}
	until, ok := parseUnix(query.Get("until"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_time", "until must be Unix seconds")
		return
	}
	filter.Since, filter.Until = since, until

	totals, err := s.Usage.Totals(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, totals)
}

func parseUnix(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (s *Server) handleCreateGoal(w http.ResponseWriter, r *http.Request, id Identity) {
	if s.Goals == nil {
		writeError(w, http.StatusNotImplemented, "goals_unavailable", "goal service is not configured")
		return
	}

	var req goals.CreateRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.WorkspaceID) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "title and workspace_id are required")
		return
	}
	req.CreatedBy = id.AgentID

	goal, err := s.Goals.Submit(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"goal_id": goal.ID,
		"status":  goal.Status,
	})
}

func (s *Server) handleListGoals(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Goals == nil {
		writeError(w, http.StatusNotImplemented, "goals_unavailable", "goal service is not configured")
		return
	}

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}

	list, err := s.Goals.List(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []goals.Goal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"goals": list})
}

func (s *Server) handleGetGoal(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Goals == nil {
		writeError(w, http.StatusNotImplemented, "goals_unavailable", "goal service is not configured")
		return
	}

	goalID := r.PathValue("id")
	goal, err := s.Goals.Get(r.Context(), goalID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	progress, err := s.Goals.Progress(r.Context(), goalID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"goal": goal, "progress": progress})
}

func (s *Server) handleCancelGoal(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Goals == nil {
		writeError(w, http.StatusNotImplemented, "goals_unavailable", "goal service is not configured")
		return
	}
	goal, err := s.Goals.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, goal)
}

// handleGoalEvents streams progress as SSE until the goal reaches a terminal
// state, the client disconnects, or the 10-minute cap is hit.
func (s *Server) handleGoalEvents(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Goals == nil {
		writeError(w, http.StatusNotImplemented, "goals_unavailable", "goal service is not configured")
		return
	}

	goalID := r.PathValue("id")
	if _, err := s.Goals.Get(r.Context(), goalID); err != nil {
		s.fail(w, r, err)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(ctx context.Context) bool {
		progress, err := s.Goals.Progress(ctx, goalID)
		if err != nil {
			return true
		}
		payload, err := json.Marshal(progress)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: progress\ndata: %s\n\n", payload); err != nil {
			return true
		}
		if flusher != nil {
			flusher.Flush()
		}
		switch progress.Status {
		case goals.StatusDone, goals.StatusCancelled, goals.StatusFailed:
			return true
		}
		return false
	}

	if send(r.Context()) {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Minute)
	defer deadline.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			if send(r.Context()) {
				return
			}
		}
	}
}

// handleScores returns the current model tiers, sorted for stable output.
func (s *Server) handleScores(w http.ResponseWriter, r *http.Request, _ Identity) {
	if s.Scores == nil {
		writeError(w, http.StatusNotImplemented, "scores_unavailable", "scoring is not configured")
		return
	}
	scores := s.Scores.Scores()
	if scores == nil {
		scores = []scoring.Score{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scores": scores})
}

type createRequest struct {
	WorkItem work.WorkItem `json:"item"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, id Identity) {
	var req createRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	item := req.WorkItem
	if strings.TrimSpace(item.ID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "item.id required")
		return
	}
	if item.CreatedBy == "" {
		item.CreatedBy = id.AgentID
	}

	if err := s.Svc.Add(r.Context(), item); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": item.ID})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, _ Identity) {
	view, err := s.Svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleHandoff(w http.ResponseWriter, r *http.Request, _ Identity) {
	pack, warnings, err := s.Svc.Handoff(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pack":     pack,
		"warnings": warnings,
	})
}

type claimRequest struct {
	Role         work.Role `json:"role"`
	Plan         string    `json:"plan"`
	LeaseSeconds int64     `json:"lease_seconds"`
	ModelID      string    `json:"model_id"`
	ModelVersion string    `json:"model_version"`
	ToolMode     string    `json:"tool_mode"`
	TaskClass    string    `json:"task_class"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, id Identity) {
	var req claimRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if !req.Role.Valid() {
		writeError(w, http.StatusUnprocessableEntity, "invalid_role", fmt.Sprintf("unknown role %q", req.Role))
		return
	}

	who := work.Claimant{
		AgentID:      id.AgentID,
		ModelID:      req.ModelID,
		ModelVersion: req.ModelVersion,
		ToolMode:     req.ToolMode,
		TaskClass:    req.TaskClass,
	}
	result, err := s.Svc.Claim(r.Context(), r.PathValue("id"), who, req.Role, req.Plan, req.LeaseSeconds)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type assignRequest struct {
	AgentID      string    `json:"agent_id"`
	Role         work.Role `json:"role"`
	Plan         string    `json:"plan"`
	LeaseSeconds int64     `json:"lease_seconds"`
	ModelID      string    `json:"model_id"`
	ModelVersion string    `json:"model_version"`
	ToolMode     string    `json:"tool_mode"`
	TaskClass    string    `json:"task_class"`
}

// handleAssign is forced dispatch: admin only, bypasses tier and path locks,
// still refuses a live lease and terminal items.
func (s *Server) handleAssign(w http.ResponseWriter, r *http.Request, id Identity) {
	var req assignRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "agent_id required")
		return
	}
	if !req.Role.Valid() {
		writeError(w, http.StatusUnprocessableEntity, "invalid_role", fmt.Sprintf("unknown role %q", req.Role))
		return
	}

	result, err := s.Svc.Assign(r.Context(), r.PathValue("id"), id.AgentID, work.Claimant{
		AgentID:      req.AgentID,
		ModelID:      req.ModelID,
		ModelVersion: req.ModelVersion,
		ToolMode:     req.ToolMode,
		TaskClass:    req.TaskClass,
	}, req.Role, req.Plan, req.LeaseSeconds)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type heartbeatRequest struct {
	Epoch         int64 `json:"epoch"`
	ExtendSeconds int64 `json:"extend_seconds"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, id Identity) {
	var req heartbeatRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if err := s.Svc.Heartbeat(r.Context(), r.PathValue("id"), id.AgentID, req.Epoch, req.ExtendSeconds); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type epochRequest struct {
	Epoch int64 `json:"epoch"`
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request, id Identity) {
	var req epochRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if err := s.Svc.Release(r.Context(), r.PathValue("id"), id.AgentID, req.Epoch); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type submitRequest struct {
	Epoch          int64         `json:"epoch"`
	IdempotencyKey string        `json:"idempotency_key"`
	Evidence       work.Evidence `json:"evidence"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request, id Identity) {
	var req submitRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if err := s.Svc.Submit(r.Context(), r.PathValue("id"), id.AgentID, req.Epoch, req.IdempotencyKey, req.Evidence); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request, id Identity) {
	var req epochRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	item, ok := s.Svc.Board.Get(r.Context(), r.PathValue("id"))
	if !ok {
		s.fail(w, r, work.ErrNotFound)
		return
	}
	if s.RunnerFor == nil {
		writeError(w, http.StatusNotImplemented, "runner_unavailable", "no check runner configured for this deployment")
		return
	}
	runner, err := s.RunnerFor(r.Context(), item)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	checks, err := s.Svc.Verify(r.Context(), item.ID, id.AgentID, req.Epoch, runner)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if checks == nil {
		checks = []work.CheckResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

type verdictRequest struct {
	Epoch        int64                `json:"epoch"`
	Verdict      work.Verdict         `json:"verdict"`
	Reasons      string               `json:"reasons"`
	Checks       []work.CheckResult   `json:"checks"`
	FailureOwner scoring.FailureOwner `json:"failure_owner"`
}

func (s *Server) handleVerdict(w http.ResponseWriter, r *http.Request, id Identity) {
	var req verdictRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	switch req.Verdict {
	case work.VerdictApprove, work.VerdictReject, work.VerdictNeedsHuman:
	default:
		writeError(w, http.StatusUnprocessableEntity, "invalid_verdict", fmt.Sprintf("unknown verdict %q", req.Verdict))
		return
	}

	if req.FailureOwner != "" && !req.FailureOwner.Valid() {
		writeError(w, http.StatusUnprocessableEntity, "invalid_owner",
			fmt.Sprintf("unknown failure_owner %q", req.FailureOwner))
		return
	}

	if err := s.Svc.Verdict(r.Context(), r.PathValue("id"), id.AgentID, req.Epoch, req.Verdict, req.Reasons, req.Checks, req.FailureOwner); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type completeRequest struct {
	Epoch          int64  `json:"epoch"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request, id Identity) {
	var req completeRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if err := s.Svc.Complete(r.Context(), r.PathValue("id"), id.AgentID, req.Epoch, req.IdempotencyKey); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type authorizeRequest struct {
	Epoch       int64         `json:"epoch"`
	Action      policy.Action `json:"action"`
	Reason      string        `json:"reason"`
	Evidence    []string      `json:"evidence"`
	WorkerModel string        `json:"worker_model"`
}

func (s *Server) handleCancelItem(w http.ResponseWriter, r *http.Request, id Identity) {
	if err := s.Svc.Cancel(r.Context(), r.PathValue("id"), id.AgentID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request, id Identity) {
	var req authorizeRequest
	if !decode(w, r, s.maxBody(), &req) {
		return
	}
	if strings.TrimSpace(req.Action.Kind) == "" && strings.TrimSpace(req.Action.Command) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_action", "action.kind or action.command required")
		return
	}

	out, err := s.Svc.Authorize(r.Context(), r.PathValue("id"), id.AgentID, req.Epoch, req.Action, req.Reason, req.Evidence, req.WorkerModel)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) maxBody() int64 {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// fail maps a domain error to a status code. The JSON body always carries a
// stable code so clients can branch without matching message text.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	if status >= 500 {
		s.logf("%s %s: %v", r.Method, r.URL.Path, err)
	}
	writeError(w, status, code, err.Error())
}

// Classify maps a domain error to an HTTP status and a stable code. MCP reuses
// it so both transports report the same code for the same failure.
func Classify(err error) (int, string) { return classify(err) }

func classify(err error) (int, string) {
	switch {
	case errors.Is(err, work.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, goals.ErrNotFound):
		return http.StatusNotFound, "goal_not_found"
	case errors.Is(err, provider.ErrNotFound):
		return http.StatusNotFound, "provider_not_found"
	case errors.Is(err, provider.ErrConflict):
		return http.StatusConflict, "already_exists"
	case errors.Is(err, provider.ErrInUse):
		return http.StatusConflict, "provider_in_use"
	case errors.Is(err, provider.ErrInvalidInput):
		return http.StatusUnprocessableEntity, "invalid_request"
	case errors.Is(err, provider.ErrSecretBox):
		return http.StatusNotImplemented, "secrets_unavailable"
	case errors.Is(err, work.ErrAlreadyExists):
		return http.StatusConflict, "already_exists"
	case errors.Is(err, work.ErrAlreadyClaimed):
		return http.StatusConflict, "already_claimed"
	case errors.Is(err, work.ErrAlreadySubmitted):
		return http.StatusConflict, "already_submitted"
	case errors.Is(err, work.ErrVersionConflict):
		return http.StatusConflict, "version_conflict"
	case errors.Is(err, work.ErrPathLocked):
		return http.StatusConflict, "path_locked"
	case errors.Is(err, work.ErrStaleEpoch):
		return http.StatusConflict, "stale_epoch"
	case errors.Is(err, work.ErrLeaseExpired):
		return http.StatusConflict, "lease_expired"
	case errors.Is(err, work.ErrInvalidTransition):
		return http.StatusConflict, "invalid_transition"
	case errors.Is(err, work.ErrDependencyNotDone):
		return http.StatusConflict, "dependency_not_done"
	case errors.Is(err, work.ErrAttemptsExhausted):
		return http.StatusConflict, "attempts_exhausted"
	case errors.Is(err, work.ErrPlanRequired):
		return http.StatusUnprocessableEntity, "plan_required"
	case errors.Is(err, work.ErrPlanTooLong):
		return http.StatusUnprocessableEntity, "plan_too_long"
	case errors.Is(err, work.ErrIdempotencyRequired):
		return http.StatusUnprocessableEntity, "idempotency_required"
	case errors.Is(err, work.ErrEvidenceMissing):
		return http.StatusUnprocessableEntity, "evidence_missing"
	case errors.Is(err, work.ErrCheckNotRun):
		return http.StatusUnprocessableEntity, "check_not_run"
	case errors.Is(err, work.ErrCheckFailed):
		return http.StatusUnprocessableEntity, "check_failed"
	case errors.Is(err, work.ErrSelfReview):
		return http.StatusForbidden, "self_review"
	case errors.Is(err, work.ErrNotHolder):
		return http.StatusForbidden, "not_lease_holder"
	case errors.Is(err, worksvc.ErrAuthzUnavailable):
		return http.StatusNotImplemented, "authz_unavailable"
	case errors.Is(err, worksvc.ErrTierDenied):
		return http.StatusForbidden, "tier_denied"
	case errors.Is(err, worksvc.ErrModelRequired):
		return http.StatusUnprocessableEntity, "model_required"
	case errors.Is(err, worksvc.ErrFailureOwnerRequired):
		return http.StatusUnprocessableEntity, "failure_owner_required"
	case errors.Is(err, work.ErrVerifyRequired):
		return http.StatusUnprocessableEntity, "verify_required"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// decode reads one JSON object with unknown fields rejected. It writes the
// error response itself and returns false so handlers stay flat.
func decode(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "invalid_json", err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "trailing data after JSON object")
		return false
	}
	return true
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Headers are already sent; nothing sensible left to do.
		_ = err
	}
}

// recover keeps one panicking handler from taking down the process.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
