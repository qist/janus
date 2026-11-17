package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
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
	ScopeAdmin     Scope = "admin"     // everything
)

// Identity is the authenticated caller. AgentID is authoritative: request
// bodies never supply it, so one key cannot claim work as another agent.
type Identity struct {
	AgentID string  `json:"agent_id"`
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
