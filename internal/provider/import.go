package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"janus/internal/oauth"
)

// ImportSource names a local CLI whose sign-in can be adopted.
type ImportSource string

const (
	ImportCodex    ImportSource = "codex"
	ImportClaude   ImportSource = "claude"
	ImportOpenCode ImportSource = "opencode"
)

// importFileLimit bounds how much of a credential file is read. These files are
// small; a huge one is not a credential store.
const importFileLimit = 1 << 20

// ImportResult reports what was adopted. Skipped carries reasons, not secrets.
type ImportResult struct {
	Source   string       `json:"source"`
	Path     string       `json:"path"`
	Imported []Credential `json:"imported"`
	Skipped  []string     `json:"skipped,omitempty"`
}

// ImportLocal reads a coding agent's own sign-in from the server's home
// directory and stores it as an oauth credential. This is how an operator can
// reuse an existing Codex/Claude/OpenCode login instead of authorizing again.
//
// The file is read once, as the service user. Tokens never leave the server;
// the response is masked. Re-importing the same account updates it in place.
func (s *Service) ImportLocal(ctx context.Context, providerID string, source ImportSource, entry, label string) (ImportResult, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return ImportResult{}, err
	}
	if s.Box == nil {
		return ImportResult{}, ErrSecretBox
	}

	path, err := importPath(source)
	if err != nil {
		return ImportResult{}, err
	}
	raw, err := readImportFile(path)
	if err != nil {
		return ImportResult{}, err
	}

	result := ImportResult{Source: string(source), Path: path, Imported: []Credential{}}
	if entry == "" && source == ImportOpenCode {
		entry = defaultOpenCodeEntry(p)
	}
	sets, err := parseImport(source, raw, entry)
	if err != nil {
		return ImportResult{}, err
	}
	if len(sets) == 0 {
		return result, fmt.Errorf("%w: no oauth sign-in found for %q", ErrInvalidInput, entry)
	}

	for _, set := range sets {
		account, plan := tokenIdentity(set.tokens)
		if account == "" {
			account = set.account
		}
		if account == "" {
			account = string(source) + " account"
		}
		displayLabel := account
		if label != "" && len(sets) == 1 {
			displayLabel = label
		}

		existing, found, err := s.FindCredentialByAccount(ctx, p.ID, account)
		if err != nil {
			return result, err
		}
		if found {
			updated, err := s.SaveOAuthTokens(ctx, existing.ID, account, planOr(set.plan, plan), set.tokens)
			if err != nil {
				return result, err
			}
			result.Imported = append(result.Imported, publicCredential(updated))
			continue
		}
		created, err := s.CreateOAuthCredential(ctx, p.ID, displayLabel, account, planOr(set.plan, plan), set.tokens)
		if err != nil {
			return result, err
		}
		result.Imported = append(result.Imported, created)
	}
	return result, nil
}

func planOr(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}

type importedSet struct {
	account string
	plan    string
	tokens  oauth.TokenSet
}

func importPath(source ImportSource) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	switch source {
	case ImportCodex:
		return filepath.Join(home, ".codex", "auth.json"), nil
	case ImportClaude:
		return filepath.Join(home, ".claude", ".credentials.json"), nil
	case ImportOpenCode:
		dataDir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
		if dataDir == "" {
			dataDir = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(dataDir, "opencode", "auth.json"), nil
	}
	return "", fmt.Errorf("%w: unknown import source %q", ErrInvalidInput, source)
}

func readImportFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not readable (%v)", ErrInvalidInput, path, err)
	}
	if info.IsDir() || info.Size() > importFileLimit {
		return nil, fmt.Errorf("%w: %s is not a credential file", ErrInvalidInput, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrInvalidInput, path, err)
	}
	return raw, nil
}

func parseImport(source ImportSource, raw []byte, entry string) ([]importedSet, error) {
	switch source {
	case ImportCodex:
		return parseCodexAuth(raw)
	case ImportClaude:
		return parseClaudeCredentials(raw)
	case ImportOpenCode:
		return parseOpenCodeAuth(raw, entry)
	}
	return nil, fmt.Errorf("%w: unknown import source %q", ErrInvalidInput, source)
}

func parseCodexAuth(raw []byte) ([]importedSet, error) {
	var payload struct {
		Tokens struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode codex auth: %v", ErrInvalidInput, err)
	}
	if payload.Tokens.AccessToken == "" {
		return nil, nil
	}
	tokens := oauth.TokenSet{
		AccessToken:  payload.Tokens.AccessToken,
		RefreshToken: payload.Tokens.RefreshToken,
		IDToken:      payload.Tokens.IDToken,
		TokenType:    "Bearer",
	}
	if payload.Tokens.IDToken != "" {
		tokens.ExpiresAt = jwtExpiry(payload.Tokens.IDToken)
	}
	return []importedSet{{account: payload.Tokens.AccountID, tokens: tokens}}, nil
}

func parseClaudeCredentials(raw []byte) ([]importedSet, error) {
	var payload struct {
		ClaudeAiOauth *struct {
			AccessToken      string   `json:"accessToken"`
			RefreshToken     string   `json:"refreshToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
			AccountEmail     string   `json:"accountEmail"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode claude credentials: %v", ErrInvalidInput, err)
	}
	auth := payload.ClaudeAiOauth
	if auth == nil || auth.AccessToken == "" {
		return nil, nil
	}
	tokens := oauth.TokenSet{
		AccessToken:  auth.AccessToken,
		RefreshToken: auth.RefreshToken,
		TokenType:    "Bearer",
		ExpiresAt:    normaliseMillis(auth.ExpiresAt),
	}
	return []importedSet{{
		account: auth.AccountEmail,
		plan:    auth.SubscriptionType,
		tokens:  tokens,
	}}, nil
}

// parseOpenCodeAuth reads OpenCode's auth.json. Entries are keyed by provider
// id; only oauth entries carry a refresh token. entry selects one key, which
// matters when several providers are signed in.
func parseOpenCodeAuth(raw []byte, entry string) ([]importedSet, error) {
	var payload map[string]struct {
		Type      string `json:"type"`
		Access    string `json:"access"`
		Refresh   string `json:"refresh"`
		Expires   int64  `json:"expires"`
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode opencode auth: %v", ErrInvalidInput, err)
	}
	entry = strings.TrimSpace(entry)
	var out []importedSet
	for id, item := range payload {
		if entry != "" && !strings.EqualFold(id, entry) {
			continue
		}
		if item.Type != "" && item.Type != "oauth" {
			continue
		}
		if strings.TrimSpace(item.Access) == "" {
			continue
		}
		out = append(out, importedSet{
			account: firstNonEmpty(item.AccountID, id+" account"),
			tokens: oauth.TokenSet{
				AccessToken:  item.Access,
				RefreshToken: item.Refresh,
				TokenType:    "Bearer",
				ExpiresAt:    normaliseMillis(item.Expires),
			},
		})
	}
	return out, nil
}

// normaliseMillis accepts both seconds and milliseconds, which the CLIs mix.
func normaliseMillis(value int64) int64 {
	if value > 1e12 {
		return value / 1000
	}
	return value
}

func jwtExpiry(token string) int64 {
	claims := decodeClaims(token)
	if claims == nil {
		return 0
	}
	if exp, ok := claims["exp"].(float64); ok {
		return int64(exp)
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// defaultOpenCodeEntry maps a janus provider to the key OpenCode uses, so an
// import with no explicit entry does not adopt an unrelated sign-in.
func defaultOpenCodeEntry(p Provider) string {
	switch {
	case p.Channel == "claude-code" || p.Kind == KindAnthropic:
		return "anthropic"
	case p.Channel == "openai-codex" || p.Kind == KindOpenAI:
		return "openai"
	case p.Channel == "github-copilot":
		return "github-copilot"
	}
	return ""
}
