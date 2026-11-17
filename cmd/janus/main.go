package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"janus/internal/api"
	"janus/internal/auditor"
	"janus/internal/auth"
	"janus/internal/authz"
	"janus/internal/mcp"
	"janus/internal/store"
	"janus/internal/verify"
	"janus/internal/work"
	"janus/internal/worksvc"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "janus:", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version")
	addr := flag.String("addr", envOr("JANUS_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	dbPath := flag.String("db", envOr("JANUS_DB", "janus.db"), "SQLite database path")
	workspace := flag.String("workspace", os.Getenv("JANUS_WORKSPACE"), "workspace root for contract checks (optional)")
	agentID := flag.String("agent-id", envOr("JANUS_AGENT_ID", "admin"), "agent id bound to JANUS_API_KEY")
	flag.Parse()

	if *showVersion {
		fmt.Printf("janus %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	apiKey := strings.TrimSpace(os.Getenv("JANUS_API_KEY"))
	if apiKey == "" {
		return errors.New("JANUS_API_KEY is required (server refuses to start unauthenticated)")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	board := work.NewBoard(st, nil)
	svc := worksvc.New(board, st)

	authority, err := capabilityAuthority(apiKey)
	if err != nil {
		return err
	}
	// No auditor models are wired yet, so every sensitive action is denied
	// until the gateway exists. That is the intended fail-closed default.
	gate := auditor.NewGate()
	gate.Cache = auditor.NewMemoryCache(600, nil)
	svc.Authz = authz.New(gate, authority)

	keys := api.NewStaticKeys()
	keys.Add(apiKey, api.Identity{AgentID: *agentID, Scopes: []api.Scope{api.ScopeAdmin}})

	server := api.New(svc, keys)
	mcpServer := mcp.New(svc, keys)
	mcpServer.Version = version

	if strings.TrimSpace(*workspace) != "" {
		// One workspace per process for now. Per-item execution sites (server
		// vs client) need a runner factory that resolves the item's checkout.
		runner := verify.NewRunner(*workspace)
		runFor := func(context.Context, work.WorkItem) (work.CheckRunner, error) {
			return runner, nil
		}
		server.RunnerFor = runFor
		mcpServer.RunnerFor = runFor
	}

	// One mux for both transports; neither registers a catch-all.
	mux := http.NewServeMux()
	server.Register(mux)
	mcpServer.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no such endpoint"}}`+"\n")
	})

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("janus listening on %s (db=%s)", *addr, *dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// capabilityAuthority signs the single-use tokens executors verify. Production
// sets JANUS_CAPABILITY_KEY; otherwise a per-process key is derived from the
// API key, which keeps dev working but invalidates tokens on restart.
func capabilityAuthority(apiKey string) (*auth.Authority, error) {
	if key := strings.TrimSpace(os.Getenv("JANUS_CAPABILITY_KEY")); key != "" {
		authority, err := auth.NewAuthority([]byte(key), nil)
		if err != nil {
			return nil, fmt.Errorf("JANUS_CAPABILITY_KEY: %w", err)
		}
		return authority, nil
	}
	sum := sha256.Sum256([]byte("janus-capability:" + apiKey))
	log.Print("JANUS_CAPABILITY_KEY unset; deriving a per-process capability key from JANUS_API_KEY")
	return auth.NewAuthority(sum[:], nil)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
