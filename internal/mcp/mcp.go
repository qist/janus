// Package mcp exposes the work board as MCP tools over a JSON-RPC endpoint.
// It is a transport only: every tool call delegates to worksvc, so an MCP
// client and an HTTP client get identical rules, scopes and error codes.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"janus/internal/api"
	"janus/internal/auth"
	"janus/internal/policy"
	"janus/internal/work"
	"janus/internal/worksvc"
)

// ProtocolVersion is the MCP revision this endpoint speaks. Clients may ask
// for a different one; we always answer with ours.
const ProtocolVersion = "2025-06-18"

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeUnauthorized   = -32001
)

// Server is the MCP front end.
type Server struct {
	Svc  *worksvc.Service
	Auth auth.Authenticator
	// RunnerFor returns the check runner for an item; nil disables work_verify.
	RunnerFor    func(ctx context.Context, item work.WorkItem) (work.CheckRunner, error)
	Name         string
	Version      string
	MaxBodyBytes int64
	Logf         func(format string, args ...any)
}

// New returns a server with the default name and body limit.
func New(svc *worksvc.Service, keys auth.Authenticator) *Server {
	return &Server{Svc: svc, Auth: keys, Name: "janus", Version: "2.0.0-dev"}
}

// Register mounts the MCP endpoint on mux. The API key may come from
// /mcp/{token} or from the X-API-Key / Authorization header.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /mcp", s.handleRPC)
	mux.HandleFunc("POST /mcp/{token}", s.handleRPC)
}

// Handler returns a standalone handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return s.recover(mux)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, s.maxBody())
	var req rpcRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		s.write(w, rpcResponse{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: codeParseError, Message: "invalid JSON: " + err.Error()}})
		return
	}

	// A request without an id is a notification: act, but send no response.
	notification := len(req.ID) == 0 || string(req.ID) == "null"

	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		s.write(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "jsonrpc must be \"2.0\""}})
		return
	}

	if notification {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	id, err := s.authenticate(r)
	if err != nil {
		s.write(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeUnauthorized, Message: "unauthenticated"}})
		return
	}

	result, rpcErr := s.dispatch(r.Context(), id, req)
	if rpcErr != nil {
		s.write(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
		return
	}
	s.write(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(ctx context.Context, id auth.Identity, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		return s.callTool(ctx, id, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + req.Method}
	}
}

func (s *Server) initialize() map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{"name": s.Name, "version": s.Version},
	}
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolFailure struct {
	code    string
	message string
}

func (f *toolFailure) Error() string { return f.code + ": " + f.message }

func fail(code, message string) error { return &toolFailure{code: code, message: message} }

func (s *Server) callTool(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, *rpcError) {
	var params callParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
	}
	if params.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call requires name"}
	}

	value, err := s.runTool(ctx, id, params.Name, params.Arguments)
	if err != nil {
		// Tool-level failures are results, not protocol errors: the model needs
		// to see them and retry with different arguments.
		payload := map[string]any{"error": map[string]string{"code": toolCode(err), "message": err.Error()}}
		text, _ := json.Marshal(payload)
		return toolResult(string(text), true), nil
	}

	text, err := json.Marshal(value)
	if err != nil {
		return nil, &rpcError{Code: codeInternal, Message: "encode result: " + err.Error()}
	}
	return toolResult(string(text), false), nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// toolCode prefers our own codes for scope/runner failures, then falls back to
// the same classification the HTTP transport uses.
func toolCode(err error) string {
	var tf *toolFailure
	if errors.As(err, &tf) {
		return tf.code
	}
	_, code := api.Classify(err)
	return code
}

type idArgs struct {
	ID string `json:"id"`
}

type handlerFunc func(context.Context, auth.Identity, json.RawMessage) (any, error)

func (s *Server) runTool(ctx context.Context, id auth.Identity, name string, args json.RawMessage) (any, error) {
	switch name {
	case "work_list":
		return s.workList(ctx, id, args)
	case "work_get":
		return s.workGet(ctx, id, args)
	case "work_handoff":
		return s.workHandoff(ctx, id, args)
	case "work_create":
		return s.workCreate(ctx, id, args)
	case "work_claim":
		return s.workClaim(ctx, id, args)
	case "work_heartbeat":
		return s.workHeartbeat(ctx, id, args)
	case "work_release":
		return s.workRelease(ctx, id, args)
	case "work_submit":
		return s.workSubmit(ctx, id, args)
	case "work_verify":
		return s.workVerify(ctx, id, args)
	case "work_verdict":
		return s.workVerdict(ctx, id, args)
	case "work_complete":
		return s.workComplete(ctx, id, args)
	case "work_authorize":
		return s.workAuthorize(ctx, id, args)
	default:
		return nil, fail("unknown_tool", "no tool named "+name)
	}
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fail("invalid_arguments", err.Error())
	}
	return nil
}

func requireScope(id auth.Identity, scope auth.Scope) error {
	if !id.Has(scope) {
		return fail("forbidden", "key lacks scope "+string(scope))
	}
	return nil
}

func (s *Server) workList(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeRead); err != nil {
		return nil, err
	}
	var args struct {
		Status    string `json:"status"`
		Workspace string `json:"workspace"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	status := work.Status(strings.TrimSpace(args.Status))
	if status != "" && !status.Valid() {
		return nil, fail("invalid_status", fmt.Sprintf("unknown status %q", status))
	}
	items, err := s.Svc.List(ctx, status, strings.TrimSpace(args.Workspace))
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []work.WorkItem{}
	}
	return map[string]any{"items": items}, nil
}

func (s *Server) workGet(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeRead); err != nil {
		return nil, err
	}
	var args idArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return s.Svc.Get(ctx, args.ID)
}

func (s *Server) workHandoff(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeRead); err != nil {
		return nil, err
	}
	var args idArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	pack, warnings, err := s.Svc.Handoff(ctx, args.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"pack": pack, "warnings": warnings}, nil
}

func (s *Server) workCreate(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeWrite); err != nil {
		return nil, err
	}
	var args struct {
		Item work.WorkItem `json:"item"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Item.ID) == "" {
		return nil, fail("invalid_arguments", "item.id required")
	}
	if args.Item.CreatedBy == "" {
		args.Item.CreatedBy = id.AgentID
	}
	if err := s.Svc.Add(ctx, args.Item); err != nil {
		return nil, err
	}
	return map[string]any{"id": args.Item.ID}, nil
}

func (s *Server) workClaim(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeClaim); err != nil {
		return nil, err
	}
	var args struct {
		ID           string    `json:"id"`
		Role         work.Role `json:"role"`
		Plan         string    `json:"plan"`
		LeaseSeconds int64     `json:"lease_seconds"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if !args.Role.Valid() {
		return nil, fail("invalid_role", fmt.Sprintf("unknown role %q", args.Role))
	}
	return s.Svc.Claim(ctx, args.ID, id.AgentID, args.Role, args.Plan, args.LeaseSeconds)
}

func (s *Server) workHeartbeat(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeClaim); err != nil {
		return nil, err
	}
	var args struct {
		ID            string `json:"id"`
		Epoch         int64  `json:"epoch"`
		ExtendSeconds int64  `json:"extend_seconds"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.Svc.Heartbeat(ctx, args.ID, id.AgentID, args.Epoch, args.ExtendSeconds); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) workRelease(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeClaim); err != nil {
		return nil, err
	}
	var args struct {
		ID    string `json:"id"`
		Epoch int64  `json:"epoch"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.Svc.Release(ctx, args.ID, id.AgentID, args.Epoch); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) workSubmit(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeSubmit); err != nil {
		return nil, err
	}
	var args struct {
		ID             string        `json:"id"`
		Epoch          int64         `json:"epoch"`
		IdempotencyKey string        `json:"idempotency_key"`
		Evidence       work.Evidence `json:"evidence"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.Svc.Submit(ctx, args.ID, id.AgentID, args.Epoch, args.IdempotencyKey, args.Evidence); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) workVerify(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeVerify); err != nil {
		return nil, err
	}
	var args struct {
		ID    string `json:"id"`
		Epoch int64  `json:"epoch"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	item, ok := s.Svc.Board.Get(ctx, args.ID)
	if !ok {
		return nil, work.ErrNotFound
	}
	if s.RunnerFor == nil {
		return nil, fail("runner_unavailable", "no check runner configured for this deployment")
	}
	runner, err := s.RunnerFor(ctx, item)
	if err != nil {
		return nil, err
	}
	checks, err := s.Svc.Verify(ctx, args.ID, id.AgentID, args.Epoch, runner)
	if err != nil {
		return nil, err
	}
	if checks == nil {
		checks = []work.CheckResult{}
	}
	return map[string]any{"checks": checks}, nil
}

func (s *Server) workVerdict(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeVerify); err != nil {
		return nil, err
	}
	var args struct {
		ID      string             `json:"id"`
		Epoch   int64              `json:"epoch"`
		Verdict work.Verdict       `json:"verdict"`
		Reasons string             `json:"reasons"`
		Checks  []work.CheckResult `json:"checks"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	switch args.Verdict {
	case work.VerdictApprove, work.VerdictReject, work.VerdictNeedsHuman:
	default:
		return nil, fail("invalid_verdict", fmt.Sprintf("unknown verdict %q", args.Verdict))
	}
	if err := s.Svc.Verdict(ctx, args.ID, id.AgentID, args.Epoch, args.Verdict, args.Reasons, args.Checks); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) workComplete(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeIntegrate); err != nil {
		return nil, err
	}
	var args struct {
		ID             string `json:"id"`
		Epoch          int64  `json:"epoch"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.Svc.Complete(ctx, args.ID, id.AgentID, args.Epoch, args.IdempotencyKey); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) workAuthorize(ctx context.Context, id auth.Identity, raw json.RawMessage) (any, error) {
	if err := requireScope(id, auth.ScopeAuthorize); err != nil {
		return nil, err
	}
	var args struct {
		ID          string        `json:"id"`
		Epoch       int64         `json:"epoch"`
		Action      policy.Action `json:"action"`
		Reason      string        `json:"reason"`
		Evidence    []string      `json:"evidence"`
		WorkerModel string        `json:"worker_model"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Action.Kind) == "" && strings.TrimSpace(args.Action.Command) == "" {
		return nil, fail("invalid_action", "action.kind or action.command required")
	}
	return s.Svc.Authorize(ctx, args.ID, id.AgentID, args.Epoch, args.Action, args.Reason, args.Evidence, args.WorkerModel)
}

func (s *Server) authenticate(r *http.Request) (auth.Identity, error) {
	if s.Auth == nil {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	key := strings.TrimSpace(r.PathValue("token"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if key == "" {
		if after, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "); ok {
			key = strings.TrimSpace(after)
		}
	}
	return s.Auth.Authenticate(r.Context(), key)
}

func (s *Server) maxBody() int64 {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return 1 << 20
}

func (s *Server) write(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		s.logf("write response: %v", err)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				s.write(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeInternal, Message: "internal error"}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
