package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Scope is one permission granted to an API key. Scopes are coarse on purpose:
// the board enforces role and lease rules, scopes only decide who may call.
type Scope string

const (
	ScopeRead      Scope = "read"      // list, get, handoff
	ScopeWrite     Scope = "write"     // create work items
	ScopeClaim     Scope = "claim"     // claim, heartbeat, release
	ScopeSubmit    Scope = "submit"    // submit evidence
	ScopeVerify    Scope = "verify"    // run checks, post verdicts
	ScopeIntegrate Scope = "integrate" // complete an approved item
	ScopeAuthorize Scope = "authorize" // request a capability token for a sensitive action
	ScopeChat      Scope = "chat"      // use the /v1 model proxy
	ScopeAdmin     Scope = "admin"     // everything
)

// Identity is the authenticated caller. AgentID is authoritative: request
// bodies never supply it, so one key cannot claim work as another agent.
type Identity struct {
	AgentID string  `json:"agent_id"`
	Kind    string  `json:"kind,omitempty"`
	Scopes  []Scope `json:"scopes"`
}

// Has reports whether the identity may perform scope.
func (i Identity) Has(scope Scope) bool {
	for _, s := range i.Scopes {
		if s == scope || s == ScopeAdmin {
			return true
		}
	}
	return false
}

// ErrUnauthenticated means the presented key is missing or unknown.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator resolves a bearer key to an identity.
type Authenticator interface {
	Authenticate(ctx context.Context, key string) (Identity, error)
}

// StaticKeys is an in-memory authenticator for development and tests. Keys are
// indexed by SHA-256 so a lookup does not compare secrets byte by byte.
type StaticKeys struct {
	mu   sync.RWMutex
	keys map[string]Identity
}

// NewStaticKeys returns an empty key set.
func NewStaticKeys() *StaticKeys {
	return &StaticKeys{keys: make(map[string]Identity)}
}

// Add registers key for id. Empty keys are ignored.
func (k *StaticKeys) Add(key string, id Identity) {
	if key == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[digest(key)] = id
}

// Len reports how many keys are registered.
func (k *StaticKeys) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.keys)
}

// Authenticate implements Authenticator.
func (k *StaticKeys) Authenticate(_ context.Context, key string) (Identity, error) {
	if key == "" {
		return Identity{}, ErrUnauthenticated
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	id, ok := k.keys[digest(key)]
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

func digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// HashKey returns the digest stored for a plaintext key. The auth package owns
// hashing so callers never persist a secret themselves.
func HashKey(key string) string { return digest(key) }

// Key kinds. A kind decides which surface a key may reach; scopes refine what
// it may do there. The env key predates kinds and is treated as an admin key.
const (
	KeyKindAdmin  = "admin"
	KeyKindClient = "client"
	KeyKindAgent  = "agent"
	KeyKindTool   = "tool"
)

// ErrKeyNotFound means the digest is unknown.
var ErrKeyNotFound = errors.New("api key not found")

// KeyRecord is the stored form of a key. Plaintext is never part of it.
type KeyRecord struct {
	ID         string
	Kind       string
	Name       string
	Prefix     string
	Hash       string
	Scopes     []Scope
	Notes      string
	ExpiresAt  int64
	Revoked    bool
	LastUsedAt int64
	CreatedAt  int64
}

// ScopesForKind returns the default grants for a kind. Callers may store a
// narrower set, but never a wider one: the kind gate runs before scopes.
func ScopesForKind(kind string) []Scope {
	switch kind {
	case KeyKindAdmin:
		return []Scope{ScopeAdmin}
	case KeyKindClient:
		return []Scope{ScopeChat, ScopeRead}
	case KeyKindAgent:
		return []Scope{ScopeChat}
	case KeyKindTool:
		return []Scope{ScopeRead}
	default:
		return nil
	}
}

func keyPrefix(kind string) string {
	switch kind {
	case KeyKindAdmin:
		return "sk-janus-a-"
	case KeyKindClient:
		return "sk-janus-c-"
	case KeyKindAgent:
		return "sk-janus-g-"
	case KeyKindTool:
		return "sk-janus-t-"
	default:
		return "sk-janus-"
	}
}

// NewKeyRecord mints a key. The plaintext is returned exactly once; only its
// digest belongs in the record.
func NewKeyRecord(kind, name, notes string, expiresAt int64, now func() int64) (string, KeyRecord, error) {
	switch kind {
	case KeyKindAdmin, KeyKindClient, KeyKindAgent, KeyKindTool:
	default:
		return "", KeyRecord{}, fmt.Errorf("unknown key kind %q", kind)
	}
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", KeyRecord{}, fmt.Errorf("generate key: %w", err)
	}
	prefix := keyPrefix(kind)
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	plaintext := prefix + secret

	var idRaw [12]byte
	if _, err := rand.Read(idRaw[:]); err != nil {
		return "", KeyRecord{}, fmt.Errorf("generate key id: %w", err)
	}

	record := KeyRecord{
		ID:        "key_" + hex.EncodeToString(idRaw[:]),
		Kind:      kind,
		Name:      strings.TrimSpace(name),
		Prefix:    prefix + secret[:6],
		Hash:      digest(plaintext),
		Scopes:    ScopesForKind(kind),
		Notes:     strings.TrimSpace(notes),
		ExpiresAt: expiresAt,
		CreatedAt: now(),
	}
	if record.Name == "" {
		record.Name = kind
	}
	return plaintext, record, nil
}

// KeySource is the storage subset the dynamic authenticator needs. Keep it
// small so tests can use a map instead of a database.
type KeySource interface {
	LookupAPIKey(ctx context.Context, digest string) (KeyRecord, error)
	TouchAPIKey(ctx context.Context, id string)
}

// DynamicKeys authenticates stored keys. Unknown, revoked and expired keys all
// look the same to the caller: a stolen key should not learn why it failed.
type DynamicKeys struct {
	Source KeySource
	Now    func() int64
}

// NewDynamicKeys returns an authenticator backed by source.
func NewDynamicKeys(source KeySource) *DynamicKeys {
	return &DynamicKeys{Source: source}
}

// Authenticate implements Authenticator.
func (d *DynamicKeys) Authenticate(ctx context.Context, key string) (Identity, error) {
	if d == nil || d.Source == nil || key == "" {
		return Identity{}, ErrUnauthenticated
	}
	record, err := d.Source.LookupAPIKey(ctx, digest(key))
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	now := time.Now().Unix()
	if d.Now != nil {
		now = d.Now()
	}
	if record.Revoked || (record.ExpiresAt > 0 && record.ExpiresAt <= now) {
		return Identity{}, ErrUnauthenticated
	}

	scopes := record.Scopes
	if len(scopes) == 0 {
		scopes = ScopesForKind(record.Kind)
	}
	// Best effort: a failed usage stamp must not reject an authorised call.
	go d.Source.TouchAPIKey(context.WithoutCancel(ctx), record.ID)

	return Identity{AgentID: record.ID, Kind: record.Kind, Scopes: scopes}, nil
}

// Chain tries authenticators in order. Static keys come first so an operator
// can always get in with JANUS_API_KEY even if the table lookup breaks.
type Chain struct {
	authenticators []Authenticator
}

// NewChain returns an authenticator that accepts any member's key.
func NewChain(authenticators ...Authenticator) *Chain {
	return &Chain{authenticators: authenticators}
}

// Authenticate implements Authenticator.
func (c *Chain) Authenticate(ctx context.Context, key string) (Identity, error) {
	if c == nil {
		return Identity{}, ErrUnauthenticated
	}
	for _, item := range c.authenticators {
		if item == nil {
			continue
		}
		if identity, err := item.Authenticate(ctx, key); err == nil {
			return identity, nil
		}
	}
	return Identity{}, ErrUnauthenticated
}
