package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"janus/internal/gateway"
	"janus/internal/mcp"
	"janus/internal/pool"
	"janus/internal/store"
	"janus/internal/usage"
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
	ledger := usage.NewMemoryLedger(nil)

	gate, err := buildGate(ledger)
	if err != nil {
		return err
	}
	svc.Authz = authz.New(gate, authority)

	keys := api.NewStaticKeys()
	keys.Add(apiKey, api.Identity{AgentID: *agentID, Scopes: []api.Scope{api.ScopeAdmin}})

	server := api.New(svc, keys)
	server.Usage = ledger
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

// buildGate wires the auditor models from JANUS_AUDITORS. Without any model the
// gate stays empty and every sensitive action is denied, which is the intended
// fail-closed default.
func buildGate(ledger usage.Ledger) (*auditor.Gate, error) {
	gate := auditor.NewGate()
	gate.Cache = auditor.NewMemoryCache(600, nil)

	raw := strings.TrimSpace(os.Getenv("JANUS_AUDITORS"))
	if raw == "" {
		log.Print("JANUS_AUDITORS unset: sensitive actions will be denied (fail-closed)")
		return gate, nil
	}

	var configs []gateway.Config
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		return nil, fmt.Errorf("JANUS_AUDITORS must be a JSON array: %w", err)
	}
	if len(configs) == 0 {
		return nil, errors.New("JANUS_AUDITORS is empty")
	}

	for _, cfg := range configs {
		client, err := gateway.New(cfg)
		if err != nil {
			return nil, fmt.Errorf("auditor %q: %w", cfg.Name, err)
		}
		if ledger != nil {
			client.OnUsage = func(u gateway.Usage) {
				_ = ledger.Append(context.Background(), usage.Entry{
					ProviderID:       u.ProviderID,
					CredentialID:     u.CredentialID,
					Model:            u.Model,
					PromptTokens:     u.PromptTokens,
					CompletionTokens: u.CompletionTokens,
				})
			}
		}
		if len(cfg.APIKeyEnvs) > 0 {
			accounts := pool.New(nil)
			for i, envName := range cfg.APIKeyEnvs {
				key := strings.TrimSpace(os.Getenv(envName))
				if key == "" {
					return nil, fmt.Errorf("auditor %q: %s is empty", client.Name, envName)
				}
				account := pool.Account{
					ID:         fmt.Sprintf("%s#%d", client.Name, i+1),
					ProviderID: client.Name,
					APIKey:     key,
					Models:     []string{client.Model},
					Priority:   i,
				}
				if err := accounts.Add(account); err != nil {
					return nil, fmt.Errorf("auditor %q: %w", client.Name, err)
				}
			}
			client.Credentials = poolCredentials{accounts}
			log.Printf("auditor %s: %d account(s) in the pool", client.Name, accounts.Len())
		}
		gate.Auditors = append(gate.Auditors, auditor.NewModelAuditor(client.Name, client.Model, modelCompleter{client}))
	}
	log.Printf("configured %d auditor model(s)", len(gate.Auditors))
	return gate, nil
}

// modelCompleter adapts the gateway client to auditor.Completer, keeping the
// auditor package free of any provider or HTTP detail.
type modelCompleter struct {
	client *gateway.Client
}

func (m modelCompleter) Complete(ctx context.Context, req auditor.CompletionRequest) (string, error) {
	resp, err := m.client.Complete(ctx, gateway.Request{
		System: req.System,
		User:   req.User,
		Model:  req.Model,
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// poolCredentials adapts the account pool to gateway.CredentialSource. The
// pool decides selection order and account health; the client only reports what
// the upstream said.
type poolCredentials struct {
	accounts *pool.Pool
}

func (p poolCredentials) Next(_ context.Context, provider, model string) (gateway.Credential, error) {
	account, err := p.accounts.Next(provider, model)
	if err != nil {
		return gateway.Credential{}, err
	}
	return gateway.Credential{ID: account.ID, APIKey: account.APIKey}, nil
}

func (p poolCredentials) Report(_ context.Context, credentialID string, status int, body string, retryAfter time.Duration) {
	if status >= 200 && status <= 299 {
		p.accounts.ReportSuccess(credentialID)
		return
	}
	p.accounts.ReportFailure(credentialID, pool.ClassifyHTTP(status, body, retryAfter))
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
