package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SyncResult describes what one /models comparison changed. Nothing is deleted
// unless Prune was requested, because a route may be the only way a client
// reaches a model that the vendor briefly stopped advertising.
type SyncResult struct {
	ProviderID   string `json:"provider_id"`
	CredentialID string `json:"credential_id,omitempty"`
	Masked       string `json:"masked,omitempty"`
	// Total is how many model ids the upstream returned.
	Total    int      `json:"total"`
	Created  []string `json:"created"`
	Existing []string `json:"existing"`
	// Conflicts are upstream models whose exposed name already belongs to a
	// different provider; they are reported instead of overwritten.
	Conflicts []string `json:"conflicts"`
	// Skipped lists upstream models the caller explicitly ignored, typically a
	// gateway's own aliases such as "default".
	Skipped []string `json:"skipped"`
	// Stale lists routes of this provider whose upstream model is no longer
	// advertised.
	Stale  []string `json:"stale"`
	Pruned []string `json:"pruned"`
	Error  string   `json:"error,omitempty"`
}

// SyncOptions controls route synchronisation.
type SyncOptions struct {
	// Prune deletes this provider's routes whose upstream model disappeared.
	// Routes owned by other providers are never touched.
	Prune bool
	// Skip names upstream models to ignore, for endpoints that advertise their
	// own aliases alongside real models.
	Skip []string
}

// SyncRoutes reads GET {base}/models with the first working credential and
// creates a route for every model that has none yet. Exposed names are the
// upstream ids, matching what /models already returns.
func (s *Service) SyncRoutes(ctx context.Context, providerID string, opts SyncOptions) (SyncResult, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{
		ProviderID: p.ID,
		Created:    []string{},
		Existing:   []string{},
		Conflicts:  []string{},
		Skipped:    []string{},
		Stale:      []string{},
		Pruned:     []string{},
	}
	if s.Box == nil {
		result.Error = "secret storage is not configured"
		return result, nil
	}

	credentials, err := s.Store.ListCredentials(ctx, p.ID)
	if err != nil {
		return SyncResult{}, err
	}
	if len(credentials) == 0 {
		result.Error = "provider has no credentials"
		return result, nil
	}

	timeout := s.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	var models []string
	var answered *Credential
	for index := range credentials {
		credential := credentials[index]
		secret, err := s.Box.Open(credential.SecretCiphertext)
		if err != nil {
			continue
		}
		outcome := probeModels(ctx, p.BaseURL, secret, timeout)
		if outcome.err != "" {
			if status := probeStatus(outcome.status); status != "" {
				s.markCredential(ctx, credential, status)
			}
			result.Error = outcome.err
			continue
		}
		models = outcome.models
		answered = &credential
		s.markCredential(ctx, credential, StatusOK)
		result.Error = ""
		break
	}
	if answered == nil {
		if result.Error == "" {
			result.Error = "no credential could list models"
		}
		return result, nil
	}
	result.CredentialID = answered.ID
	result.Masked = answered.Masked
	result.Total = len(models)
	if len(opts.Skip) > 0 {
		skip := make(map[string]bool, len(opts.Skip))
		for _, name := range opts.Skip {
			skip[strings.TrimSpace(name)] = true
		}
		kept := models[:0]
		for _, model := range models {
			if skip[model] {
				result.Skipped = append(result.Skipped, model)
				continue
			}
			kept = append(kept, model)
		}
		models = kept
	}

	// Routes of other providers keep their exposed names. A conflicting name is
	// reported so an operator can rename, never silently reassigned.
	routes, err := s.Store.ListRoutes(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	owned := make(map[string]bool, len(routes))
	upstreams := make(map[string]bool, len(models))
	for _, model := range models {
		upstreams[model] = true
	}
	providerRoutes := make([]Route, 0)
	for _, route := range routes {
		if route.ProviderID != p.ID {
			owned[route.ExposedModel] = true
			continue
		}
		providerRoutes = append(providerRoutes, route)
	}
	seen := make(map[string]bool, len(providerRoutes))
	for _, route := range providerRoutes {
		seen[route.UpstreamModel] = true
	}

	now := s.now()
	for _, model := range models {
		if seen[model] {
			result.Existing = append(result.Existing, model)
			continue
		}
		if owned[model] {
			result.Conflicts = append(result.Conflicts, model)
			continue
		}
		_, err := s.CreateRoute(ctx, Route{
			ExposedModel:  model,
			ProviderID:    p.ID,
			UpstreamModel: model,
			Enabled:       true,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
		if err != nil {
			// The unique exposed name can also collide with a route created
			// between the listing and this insert.
			result.Conflicts = append(result.Conflicts, model)
			continue
		}
		result.Created = append(result.Created, model)
	}

	for _, route := range providerRoutes {
		if upstreams[route.UpstreamModel] {
			continue
		}
		result.Stale = append(result.Stale, route.ExposedModel)
		if !opts.Prune {
			continue
		}
		if err := s.Store.DeleteRoute(ctx, route.ID); err != nil {
			return result, fmt.Errorf("prune route %q: %w", route.ExposedModel, err)
		}
		result.Pruned = append(result.Pruned, route.ExposedModel)
	}

	sort.Strings(result.Created)
	sort.Strings(result.Existing)
	sort.Strings(result.Conflicts)
	sort.Strings(result.Skipped)
	sort.Strings(result.Stale)
	sort.Strings(result.Pruned)
	return result, nil
}

// dedupeModels drops blanks and duplicates while keeping the upstream order.
func dedupeModels(models []string) []string {
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	return out
}
