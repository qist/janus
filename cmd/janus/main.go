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
	"sync"
	"syscall"
	"time"

	"janus/internal/api"
	"janus/internal/auditor"
	"janus/internal/auth"
	"janus/internal/authz"
	"janus/internal/gateway"
	"janus/internal/goals"
	"janus/internal/mcp"
	"janus/internal/oauth"
	"janus/internal/pluginrt"
	"janus/internal/pool"
	"janus/internal/provider"
	"janus/internal/proxy"
	"janus/internal/scoring"
	"janus/internal/store"
	"janus/internal/ui"
	"janus/internal/usage"

	"janus/internal/verify"
	"janus/internal/work"
	"janus/internal/worksvc"
	_ "janus/plugins/all"
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
	tlsAddr := flag.String("tls-addr", os.Getenv("JANUS_TLS_ADDR"), "HTTPS listen address (optional)")
	tlsCert := flag.String("tls-cert", os.Getenv("JANUS_TLS_CERT"), "TLS certificate path")
	tlsKey := flag.String("tls-key", os.Getenv("JANUS_TLS_KEY"), "TLS private key path")
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
	goalSvc := goals.New(st, board)

	authority, err := capabilityAuthority(apiKey)
	if err != nil {
		return err
	}
	// Usage rows live in SQLite: restarting the process must not erase spend
	// history, and the same table backs the console's statistics page.
	var ledger usage.Ledger = st

	gate, err := buildGate(ledger)
	if err != nil {
		return err
	}
	svc.Authz = authz.New(gate, authority)
	scores := scoring.New(scoring.Config{})
	svc.Scores = scores

	keys := api.NewStaticKeys()
	keys.Add(apiKey, api.Identity{AgentID: *agentID, Scopes: []api.Scope{api.ScopeAdmin}})

	providers, err := buildProviders(st)
	if err != nil {
		return err
	}
	envRoutes, err := buildModelRoutes()
	if err != nil {
		return err
	}
	proxyServer := proxy.New(nil, keys, ledger)

	// reloadModels rebuilds the proxy table from env routes plus stored ones.
	// The mutex keeps two concurrent mutations from publishing an older
	// snapshot after a newer one.
	var reloadMu sync.Mutex
	reloadModels := func(ctx context.Context) error {
		reloadMu.Lock()
		defer reloadMu.Unlock()

		routes := make(map[string]*gateway.Client, len(envRoutes))
		for name, client := range envRoutes {
			routes[name] = client
		}
		if providers != nil {
			managed, err := providers.Clients(ctx)
			if err != nil {
				return err
			}
			for name, client := range managed {
				if _, exists := routes[name]; exists {
					log.Printf("model %q is defined by JANUS_MODELS and the provider store; keeping the env one", name)
					continue
				}
				routes[name] = client
			}
		}
		proxyServer.SetRoutes(routes)
		return nil
	}
	if err := reloadModels(context.Background()); err != nil {
		return fmt.Errorf("load model routes: %w", err)
	}

	server := api.New(svc, keys)
	server.Usage = ledger
	server.Scores = scores
	server.Goals = goalSvc
	server.Providers = providers
	server.ReloadModels = reloadModels
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
	proxyServer.Register(mux)

	console := ui.Handler()
	mux.Handle("/ui", console)
	mux.Handle("/ui/", console)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no such endpoint"}}`+"\n")
	})

	handler := securityHeaders(mux)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	// A second listener serves the same handler over TLS. Both ports stay open
	// during a migration, so an existing client keeps working until it is
	// pointed at https.
	var tlsServer *http.Server
	if strings.TrimSpace(*tlsAddr) != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return errors.New("JANUS_TLS_ADDR requires JANUS_TLS_CERT and JANUS_TLS_KEY")
		}
		tlsServer = &http.Server{
			Addr:              *tlsAddr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
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

	if tlsServer != nil {
		go func() {
			log.Printf("janus listening on %s (TLS)", *tlsAddr)
			if err := tlsServer.ListenAndServeTLS(*tlsCert, *tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if tlsServer != nil {
		_ = tlsServer.Shutdown(shutdownCtx)
	}
	return httpServer.Shutdown(shutdownCtx)
}

// securityHeaders adds the headers a browser-facing service should always send.
// HSTS is only set on TLS requests: sending it over plain HTTP is ignored by
// browsers and would be a lie on the migration port.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
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
		client, err := newModelClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("auditor %q: %w", cfg.Name, err)
		}
		client.OnUsage = usageRecorder(ledger)
		gate.Auditors = append(gate.Auditors, auditor.NewModelAuditor(client.Name, client.Model, modelCompleter{client}))
	}
	log.Printf("configured %d auditor model(s)", len(gate.Auditors))
	return gate, nil
}

// buildModelRoutes exposes JANUS_MODELS over /v1. Each entry is one upstream
// model plus optional aliases.
func buildModelRoutes() (map[string]*gateway.Client, error) {
	raw := strings.TrimSpace(os.Getenv("JANUS_MODELS"))
	if raw == "" {
		return map[string]*gateway.Client{}, nil
	}

	var configs []gateway.Config
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		return nil, fmt.Errorf("JANUS_MODELS must be a JSON array: %w", err)
	}

	routes := make(map[string]*gateway.Client, len(configs))
	for _, cfg := range configs {
		client, err := newModelClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", cfg.Model, err)
		}
		if _, exists := routes[client.Model]; exists {
			return nil, fmt.Errorf("model %q is defined twice", client.Model)
		}
		routes[client.Model] = client
		for _, alias := range cfg.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			if _, exists := routes[alias]; exists {
				return nil, fmt.Errorf("model alias %q is defined twice", alias)
			}
			routes[alias] = client
		}
	}
	log.Printf("configured %d model route(s)", len(routes))
	return routes, nil
}

// buildProviders wires DB-backed providers, credentials and routes when
// JANUS_MASTER_KEY is set. Without a key the endpoints answer 501 rather than
// storing secrets in plaintext.
func buildProviders(st *store.Store) (*provider.Service, error) {
	raw := strings.TrimSpace(os.Getenv("JANUS_MASTER_KEY"))
	if raw == "" {
		log.Print("JANUS_MASTER_KEY unset: provider management disabled (501)")
		return nil, nil
	}
	key, err := provider.ParseMasterKey(raw)
	if err != nil {
		return nil, fmt.Errorf("JANUS_MASTER_KEY: %w", err)
	}
	box, err := provider.NewSecretBox(key)
	if err != nil {
		return nil, fmt.Errorf("JANUS_MASTER_KEY: %w", err)
	}
	svc := provider.New(st, box)
	loadProviderPlugins(svc)
	return svc, nil
}

// loadProviderPlugins applies the compiled login plugins. JANUS_PLUGINS points
// at plugins.json; without it every plugin keeps its defaults.
func loadProviderPlugins(svc *provider.Service) {
	file, err := pluginrt.Load(strings.TrimSpace(os.Getenv("JANUS_PLUGINS")))
	if err != nil {
		log.Printf("plugins: %v (using defaults)", err)
		file = pluginrt.File{}
	}
	registry := oauth.NewRegistry()
	devices := oauth.NewDeviceRegistry()
	polls := oauth.NewPollRegistry()
	pluginrt.ApplyManifests(pluginrt.Registered(), file, pluginrt.Targets{
		OAuth:   registry,
		Devices: devices,
		Polls:   polls,
	})
	svc.OAuth = registry
	svc.Devices = devices
	svc.Polls = polls
	for _, status := range pluginrt.LastStatus() {
		if status.State == pluginrt.StateFailed {
			log.Printf("plugin %s failed: %s", status.ID, status.Error)
		}
	}
}

// usageRecorder returns the ledger callback shared by every gateway client.
// Failed calls do not reach it: the gateway only reports successful usage.
func usageRecorder(ledger usage.Ledger) func(gateway.Usage) {
	if ledger == nil {
		return nil
	}
	return func(u gateway.Usage) {
		_ = ledger.Append(context.Background(), usage.Entry{
			ProviderID:       u.ProviderID,
			CredentialID:     u.CredentialID,
			Model:            u.Model,
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			CachedTokens:     u.CachedTokens,
		})
	}
}

// newModelClient builds one upstream client, including its account pool.
func newModelClient(cfg gateway.Config) (*gateway.Client, error) {
	client, err := gateway.New(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.APIKeyEnvs) == 0 {
		return client, nil
	}

	accounts := pool.New(nil)
	for i, envName := range cfg.APIKeyEnvs {
		key := strings.TrimSpace(os.Getenv(envName))
		if key == "" {
			return nil, fmt.Errorf("%s is empty", envName)
		}
		account := pool.Account{
			ID:         fmt.Sprintf("%s#%d", client.Name, i+1),
			ProviderID: client.Name,
			APIKey:     key,
			Models:     []string{client.Model},
			Priority:   i,
		}
		if err := accounts.Add(account); err != nil {
			return nil, err
		}
	}
	client.Credentials = poolCredentials{accounts}
	log.Printf("model %s: %d account(s) in the pool", client.Name, accounts.Len())
	return client, nil
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
