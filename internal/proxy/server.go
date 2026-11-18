// Package proxy is the OpenAI-compatible model endpoint: POST /v1/chat/completions
// and GET /v1/models. It is a transport: routing, keys, failover and usage all
// live in gateway/pool/usage, and the request body is forwarded as-is except for
// the model name.
package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"janus/internal/auth"
	"janus/internal/gateway"
	"janus/internal/pool"
	"janus/internal/usage"
)

// Server exposes models over the OpenAI wire format.
//
// The route table is swapped atomically so a configuration change takes effect
// without a restart. A request that already resolved a client keeps it for its
// whole lifetime; only new requests see the new table.
type Server struct {
	routes atomic.Pointer[map[string]*gateway.Client]
	Auth   auth.Authenticator
	Usage  usage.Ledger
	// MaxBodyBytes overrides the 2 MiB default.
	MaxBodyBytes int64
	Logf         func(format string, args ...any)
}

// New returns a server. A nil authenticator rejects every request.
func New(routes map[string]*gateway.Client, keys auth.Authenticator, ledger usage.Ledger) *Server {
	s := &Server{Auth: keys, Usage: ledger}
	s.SetRoutes(routes)
	return s
}

// SetRoutes replaces the whole table. The map is copied, so the caller may keep
// mutating its own copy afterwards.
func (s *Server) SetRoutes(routes map[string]*gateway.Client) {
	snapshot := make(map[string]*gateway.Client, len(routes))
	for name, client := range routes {
		snapshot[name] = client
	}
	s.routes.Store(&snapshot)
}

// Models returns the exposed names, sorted for stable output.
func (s *Server) Models() []string {
	table := s.table()
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) table() map[string]*gateway.Client {
	if snapshot := s.routes.Load(); snapshot != nil {
		return *snapshot
	}
	return nil
}

// Register mounts the proxy routes on mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
}

// Handler returns a standalone handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}

	names := s.Models()

	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{
			"id":       name,
			"object":   "model",
			"owned_by": "janus",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authorize(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody()))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model_required", "model is required")
		return
	}
	client, ok := s.table()[model]
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", "unknown model "+model)
		return
	}

	_, stream := payload["stream"].(bool)
	payload["model"] = client.Model
	rewritten, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if stream {
		s.stream(w, r, identity, client, rewritten)
		return
	}
	s.forward(w, r, identity, client, rewritten)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, id auth.Identity, client *gateway.Client, body []byte) {
	raw, used, err := client.Chat(r.Context(), client.Model, body)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	s.record(r, id, used, client.Model)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, id auth.Identity, client *gateway.Client, body []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	counter := &countingWriter{target: flushWriter{w: w}}
	used, err := client.ChatStream(r.Context(), client.Model, body, counter)
	s.record(r, id, used, client.Model)
	if err != nil {
		if counter.written == 0 {
			s.upstreamError(w, err)
			return
		}
		// The stream already started; all we can do is stop writing.
		s.logf("stream for %s failed mid-flight: %v", client.Model, err)
	}
}

func (s *Server) upstreamError(w http.ResponseWriter, err error) {
	var exhausted *pool.AllExhaustedError
	if errors.As(err, &exhausted) {
		if exhausted.ResumeAt > 0 {
			wait := exhausted.ResumeAt - time.Now().Unix()
			if wait < 1 {
				wait = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(wait, 10))
		}
		writeError(w, http.StatusServiceUnavailable, "all_accounts_exhausted", err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

func (s *Server) record(r *http.Request, id auth.Identity, used gateway.Usage, requested string) {
	if s.Usage == nil {
		return
	}
	model := used.Model
	if model == "" {
		model = requested
	}
	if err := s.Usage.Append(r.Context(), usage.Entry{
		KeyID:            id.AgentID,
		ProviderID:       used.ProviderID,
		CredentialID:     used.CredentialID,
		Model:            model,
		PromptTokens:     used.PromptTokens,
		CompletionTokens: used.CompletionTokens,
	}); err != nil {
		s.logf("record usage: %v", err)
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	if s.Auth == nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing API key")
		return auth.Identity{}, false
	}
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("Authorization"))
		key = strings.TrimSpace(strings.TrimPrefix(key, "Bearer "))
	}
	identity, err := s.Auth.Authenticate(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
		return auth.Identity{}, false
	}
	if !identity.Has(auth.ScopeChat) {
		writeError(w, http.StatusForbidden, "forbidden", "key lacks scope chat")
		return auth.Identity{}, false
	}
	return identity, true
}

func (s *Server) maxBody() int64 {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return 2 << 20
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

type flushWriter struct {
	w http.ResponseWriter
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

type countingWriter struct {
	target  io.Writer
	written int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.target.Write(p)
	c.written += n
	return n, err
}

// writeError uses the OpenAI error envelope so existing clients can parse it.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "janus_error",
			"code":    code,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
