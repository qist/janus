// Package provider owns the upstream side of the model plane: which providers
// exist, which credentials pay for them, and which exposed model (or alias)
// maps to which upstream model.
//
// Secrets are sealed with AES-GCM before they reach the store and are never
// serialized back to clients: list responses only carry a masked hint. The
// service also builds gateway clients from this configuration, so the console
// and the proxy read the same source of truth.
package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"janus/internal/gateway"
	"janus/internal/pool"
)

// Kind is the upstream protocol family.
type Kind string

const (
	KindOpenAI    Kind = "openai"
	KindAnthropic Kind = "anthropic"
	KindCustom    Kind = "custom"
)

// Valid reports whether k is a supported provider kind.
func (k Kind) Valid() bool {
	switch k {
	case KindOpenAI, KindAnthropic, KindCustom:
		return true
	}
	return false
}

// CredentialKind separates plain API keys from OAuth grants. OAuth secrets are
// refresh tokens and must never leave the server either.
type CredentialKind string

const (
	CredentialAPIKey CredentialKind = "api_key"
	CredentialOAuth  CredentialKind = "oauth"
)

// Valid reports whether k is a supported credential kind.
func (k CredentialKind) Valid() bool {
	switch k {
	case CredentialAPIKey, CredentialOAuth:
		return true
	}
	return false
}

// Status is the last known quota state of one credential.
type Status string

const (
	StatusUnknown     Status = "unknown"
	StatusOK          Status = "ok"
	StatusExhausted   Status = "exhausted"
	StatusRateLimited Status = "rate_limited"
	StatusInvalid     Status = "invalid"
)

// Valid reports whether s is a known credential status.
func (s Status) Valid() bool {
	switch s {
	case StatusUnknown, StatusOK, StatusExhausted, StatusRateLimited, StatusInvalid:
		return true
	}
	return false
}

// Provider is one upstream endpoint.
type Provider struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      Kind   `json:"kind"`
	BaseURL   string `json:"base_url"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Credential is one key or OAuth grant. SecretCiphertext never leaves the
// server: it is tagged json:"-" and cleared before list responses are returned.
type Credential struct {
	ID         string         `json:"id"`
	ProviderID string         `json:"provider_id"`
	Label      string         `json:"label,omitempty"`
	Kind       CredentialKind `json:"kind"`
	Status     Status         `json:"status"`
	// Masked is a short hint such as "…a1b2"; the real secret is not returned.
	Masked string `json:"masked,omitempty"`
	// HasSecret tells the console whether something is stored.
	HasSecret bool `json:"has_secret"`
	// Priority orders credentials for probing and for pool selection; lower
	// comes first. It is assigned at creation so the order cannot drift with
	// random ids.
	Priority  int   `json:"priority"`
	Remaining int64 `json:"remaining,omitempty"`
	Total     int64 `json:"total,omitempty"`
	ExpiresAt int64 `json:"expires_at,omitempty"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	SecretCiphertext string `json:"-"`
}

// Route maps an exposed model name (or alias) to an upstream model.
type Route struct {
	ID            string   `json:"id"`
	ExposedModel  string   `json:"exposed_model"`
	ProviderID    string   `json:"provider_id"`
	UpstreamModel string   `json:"upstream_model"`
	Aliases       []string `json:"aliases,omitempty"`
	Priority      int      `json:"priority"`
	Enabled       bool     `json:"enabled"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
}

// Errors returned by the service; the API maps them to stable codes.
var (
	ErrNotFound     = errors.New("provider resource not found")
	ErrConflict     = errors.New("provider resource already exists")
	ErrInUse        = errors.New("provider still has credentials or routes")
	ErrSecretBox    = errors.New("secret box is not configured")
	ErrInvalidInput = errors.New("invalid provider input")
)

// Store persists providers, credentials and routes.
type Store interface {
	SaveProvider(ctx context.Context, p Provider) error
	GetProvider(ctx context.Context, id string) (Provider, error)
	ListProviders(ctx context.Context) ([]Provider, error)
	DeleteProvider(ctx context.Context, id string) error

	SaveCredential(ctx context.Context, c Credential) error
	// ListCredentials with an empty providerID returns every credential.
	ListCredentials(ctx context.Context, providerID string) ([]Credential, error)
	DeleteCredential(ctx context.Context, id string) error

	SaveRoute(ctx context.Context, r Route) error
	ListRoutes(ctx context.Context) ([]Route, error)
	DeleteRoute(ctx context.Context, id string) error
}

// Service is the provider/credential/route application layer.
type Service struct {
	Store Store
	Box   *SecretBox
	Now   func() int64
	// ProbeTimeout bounds one connectivity probe; zero uses DefaultProbeTimeout.
	ProbeTimeout time.Duration
}

// New returns a service. box may be nil only for read-only deployments, in
// which case creating credentials fails with ErrSecretBox.
func New(store Store, box *SecretBox) *Service {
	return &Service{Store: store, Box: box}
}

// CreateProvider validates and stores a provider.
func (s *Service) CreateProvider(ctx context.Context, p Provider) (Provider, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.ID == "" {
		return Provider{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if !p.Kind.Valid() {
		return Provider{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, p.Kind)
	}
	if err := validateBaseURL(p.BaseURL); err != nil {
		return Provider{}, err
	}
	if p.Name == "" {
		p.Name = p.ID
	}

	if _, err := s.Store.GetProvider(ctx, p.ID); err == nil {
		return Provider{}, fmt.Errorf("%w: provider %q", ErrConflict, p.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return Provider{}, err
	}

	now := s.now()
	p.Enabled = true
	p.CreatedAt = now
	p.UpdatedAt = now
	if err := s.Store.SaveProvider(ctx, p); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// ProviderPatch carries optional updates.
type ProviderPatch struct {
	Name    *string
	BaseURL *string
	Enabled *bool
}

// UpdateProvider applies a patch. Disabling a provider leaves its rows intact.
func (s *Service) UpdateProvider(ctx context.Context, id string, patch ProviderPatch) (Provider, error) {
	p, err := s.Store.GetProvider(ctx, id)
	if err != nil {
		return Provider{}, err
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return Provider{}, fmt.Errorf("%w: name cannot be empty", ErrInvalidInput)
		}
		p.Name = name
	}
	if patch.BaseURL != nil {
		base := strings.TrimRight(strings.TrimSpace(*patch.BaseURL), "/")
		if err := validateBaseURL(base); err != nil {
			return Provider{}, err
		}
		p.BaseURL = base
	}
	if patch.Enabled != nil {
		p.Enabled = *patch.Enabled
	}
	p.UpdatedAt = s.now()
	if err := s.Store.SaveProvider(ctx, p); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// GetProvider returns one provider.
func (s *Service) GetProvider(ctx context.Context, id string) (Provider, error) {
	return s.Store.GetProvider(ctx, id)
}

// ListProviders returns providers sorted by name.
func (s *Service) ListProviders(ctx context.Context) ([]Provider, error) {
	list, err := s.Store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// DeleteProvider removes a provider only when nothing references it, so a
// delete cannot silently orphan routes or destroy credentials.
func (s *Service) DeleteProvider(ctx context.Context, id string) error {
	if _, err := s.Store.GetProvider(ctx, id); err != nil {
		return err
	}

	routes, err := s.Store.ListRoutes(ctx)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.ProviderID == id {
			return fmt.Errorf("%w: route %q", ErrInUse, route.ExposedModel)
		}
	}
	credentials, err := s.Store.ListCredentials(ctx, id)
	if err != nil {
		return err
	}
	if len(credentials) > 0 {
		return fmt.Errorf("%w: %d credential(s)", ErrInUse, len(credentials))
	}
	return s.Store.DeleteProvider(ctx, id)
}

// CreateCredential seals the secret and stores only the ciphertext.
func (s *Service) CreateCredential(ctx context.Context, providerID, label string, kind CredentialKind, secret string) (Credential, error) {
	providerID = strings.TrimSpace(providerID)
	if _, err := s.Store.GetProvider(ctx, providerID); err != nil {
		return Credential{}, err
	}
	if !kind.Valid() {
		return Credential{}, fmt.Errorf("%w: unknown credential kind %q", ErrInvalidInput, kind)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Credential{}, fmt.Errorf("%w: secret required", ErrInvalidInput)
	}
	if s.Box == nil {
		return Credential{}, ErrSecretBox
	}
	sealed, err := s.Box.Seal(secret)
	if err != nil {
		return Credential{}, fmt.Errorf("seal credential: %w", err)
	}

	existing, err := s.Store.ListCredentials(ctx, providerID)
	if err != nil {
		return Credential{}, err
	}
	priority := 0
	for _, item := range existing {
		if item.Priority >= priority {
			priority = item.Priority + 1
		}
	}

	now := s.now()
	credential := Credential{
		ID:               newResourceID("cred"),
		Priority:         priority,
		ProviderID:       providerID,
		Label:            strings.TrimSpace(label),
		Kind:             kind,
		Status:           StatusUnknown,
		Masked:           Mask(secret),
		HasSecret:        true,
		SecretCiphertext: sealed,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	// A coarse id keeps inserts readable while still unique enough for a single
	// node; multi-node would use a UUID.
	if err := s.Store.SaveCredential(ctx, credential); err != nil {
		return Credential{}, err
	}
	return publicCredential(credential), nil
}

// ListCredentials returns masked credentials only.
func (s *Service) ListCredentials(ctx context.Context, providerID string) ([]Credential, error) {
	if strings.TrimSpace(providerID) != "" {
		if _, err := s.Store.GetProvider(ctx, providerID); err != nil {
			return nil, err
		}
	}
	list, err := s.Store.ListCredentials(ctx, providerID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Priority != list[j].Priority {
			return list[i].Priority < list[j].Priority
		}
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt < list[j].CreatedAt
		}
		return list[i].ID < list[j].ID
	})

	out := make([]Credential, 0, len(list))
	for _, credential := range list {
		out = append(out, publicCredential(credential))
	}
	return out, nil
}

// DeleteCredential removes one credential.
func (s *Service) DeleteCredential(ctx context.Context, id string) error {
	return s.Store.DeleteCredential(ctx, id)
}

// CreateRoute validates that the exposed name and aliases are unique across
// every route, so clients never resolve the same name to two providers.
func (s *Service) CreateRoute(ctx context.Context, route Route) (Route, error) {
	route.ExposedModel = strings.TrimSpace(route.ExposedModel)
	route.UpstreamModel = strings.TrimSpace(route.UpstreamModel)
	route.ProviderID = strings.TrimSpace(route.ProviderID)
	if route.ExposedModel == "" || route.UpstreamModel == "" {
		return Route{}, fmt.Errorf("%w: exposed and upstream model required", ErrInvalidInput)
	}
	if _, err := s.Store.GetProvider(ctx, route.ProviderID); err != nil {
		return Route{}, err
	}
	if route.Priority < 0 {
		return Route{}, fmt.Errorf("%w: priority must be >= 0", ErrInvalidInput)
	}

	existing, err := s.Store.ListRoutes(ctx)
	if err != nil {
		return Route{}, err
	}
	taken := make(map[string]string, len(existing)*2)
	for _, r := range existing {
		taken[r.ExposedModel] = r.ID
		for _, alias := range r.Aliases {
			taken[alias] = r.ID
		}
	}
	names := append([]string{route.ExposedModel}, route.Aliases...)
	seen := make(map[string]struct{}, len(names))
	aliases := make([]string, 0, len(route.Aliases))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			// An alias equal to the exposed name (or to another alias) is
			// redundant, not an error: the name already resolves to this route.
			continue
		}
		seen[name] = struct{}{}
		if _, ok := taken[name]; ok {
			return Route{}, fmt.Errorf("%w: model %q", ErrConflict, name)
		}
		if name != route.ExposedModel {
			aliases = append(aliases, name)
		}
	}
	route.Aliases = aliases

	now := s.now()
	route.ID = newResourceID("route")
	route.Enabled = true
	route.CreatedAt = now
	route.UpdatedAt = now
	if err := s.Store.SaveRoute(ctx, route); err != nil {
		return Route{}, err
	}
	return route, nil
}

// ListRoutes returns every route, stable-sorted by exposed name.
func (s *Service) ListRoutes(ctx context.Context) ([]Route, error) {
	list, err := s.Store.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ExposedModel < list[j].ExposedModel })
	return list, nil
}

// DeleteRoute removes one route.
func (s *Service) DeleteRoute(ctx context.Context, id string) error {
	return s.Store.DeleteRoute(ctx, id)
}

// Clients builds the gateway clients for every enabled route. Disabled
// providers, disabled routes and invalid credentials are skipped. A credential
// that cannot be decrypted is a hard error: starting with a silently shrunk
// pool would be worse than refusing to start.
func (s *Service) Clients(ctx context.Context) (map[string]*gateway.Client, error) {
	providers, err := s.Store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byID[p.ID] = p
	}

	credentials, err := s.Store.ListCredentials(ctx, "")
	if err != nil {
		return nil, err
	}
	byProvider := make(map[string][]Credential, len(providers))
	for _, credential := range credentials {
		byProvider[credential.ProviderID] = append(byProvider[credential.ProviderID], credential)
	}

	routes, err := s.Store.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]*gateway.Client, len(routes)*2)
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		p, ok := byID[route.ProviderID]
		if !ok || !p.Enabled {
			continue
		}

		client, err := gateway.New(gateway.Config{
			Name:    p.Name,
			BaseURL: p.BaseURL,
			Model:   route.UpstreamModel,
		})
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route.ExposedModel, err)
		}

		accounts := pool.New(s.now)
		for index, credential := range byProvider[p.ID] {
			if credential.Status == StatusInvalid {
				continue
			}
			if s.Box == nil {
				return nil, ErrSecretBox
			}
			secret, err := s.Box.Open(credential.SecretCiphertext)
			if err != nil {
				return nil, fmt.Errorf("credential %q: %w", credential.ID, err)
			}
			account := pool.Account{
				ID:         credential.ID,
				ProviderID: p.Name,
				APIKey:     secret,
				Models:     []string{route.UpstreamModel},
				Priority:   index,
				Total:      credential.Total,
				Remaining:  credential.Remaining,
			}
			if err := accounts.Add(account); err != nil {
				return nil, err
			}
		}
		if accounts.Len() > 0 {
			client.Credentials = credentialSource{accounts: accounts}
		}

		out[route.ExposedModel] = client
		for _, alias := range route.Aliases {
			out[alias] = client
		}
	}
	return out, nil
}

// credentialSource adapts the pool to gateway.CredentialSource.
type credentialSource struct {
	accounts *pool.Pool
}

func (c credentialSource) Next(ctx context.Context, provider, model string) (gateway.Credential, error) {
	account, err := c.accounts.Next(provider, model)
	if err != nil {
		return gateway.Credential{}, err
	}
	return gateway.Credential{ID: account.ID, APIKey: account.APIKey}, nil
}

func (c credentialSource) Report(_ context.Context, credentialID string, status int, body string, retryAfter time.Duration) {
	if status >= 200 && status <= 299 {
		c.accounts.ReportSuccess(credentialID)
		return
	}
	c.accounts.ReportFailure(credentialID, pool.ClassifyHTTP(status, body, retryAfter))
}

func (s *Service) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().Unix()
}

func publicCredential(c Credential) Credential {
	c.SecretCiphertext = ""
	return c
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: base_url required", ErrInvalidInput)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%w: base_url %q is not a URL", ErrInvalidInput, raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%w: base_url must be http(s)", ErrInvalidInput)
	}
	return nil
}

func newResourceID(prefix string) string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// MemoryStore is the in-process Store used by tests.
type MemoryStore struct {
	mu          sync.Mutex
	providers   map[string]Provider
	credentials map[string]Credential
	routes      map[string]Route
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		providers:   make(map[string]Provider),
		credentials: make(map[string]Credential),
		routes:      make(map[string]Route),
	}
}

func (m *MemoryStore) SaveProvider(_ context.Context, p Provider) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers[p.ID] = p
	return nil
}

func (m *MemoryStore) GetProvider(_ context.Context, id string) (Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.providers[id]
	if !ok {
		return Provider{}, ErrNotFound
	}
	return p, nil
}

func (m *MemoryStore) ListProviders(_ context.Context) ([]Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		out = append(out, p)
	}
	return out, nil
}

func (m *MemoryStore) DeleteProvider(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[id]; !ok {
		return ErrNotFound
	}
	delete(m.providers, id)
	return nil
}

func (m *MemoryStore) SaveCredential(_ context.Context, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentials[c.ID] = c
	return nil
}

func (m *MemoryStore) ListCredentials(_ context.Context, providerID string) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Credential, 0, len(m.credentials))
	for _, c := range m.credentials {
		if providerID != "" && c.ProviderID != providerID {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemoryStore) DeleteCredential(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.credentials[id]; !ok {
		return ErrNotFound
	}
	delete(m.credentials, id)
	return nil
}

func (m *MemoryStore) SaveRoute(_ context.Context, r Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[r.ID] = r
	return nil
}

func (m *MemoryStore) ListRoutes(_ context.Context) ([]Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Route, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	return out, nil
}

func (m *MemoryStore) DeleteRoute(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.routes[id]; !ok {
		return ErrNotFound
	}
	delete(m.routes, id)
	return nil
}
