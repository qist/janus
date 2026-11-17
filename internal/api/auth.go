package api

import "janus/internal/auth"

// Identity, scopes and authenticators live in the auth package so the MCP
// transport can use the exact same key set. These aliases keep HTTP call sites
// (and main) free of an extra import.
type (
	Scope         = auth.Scope
	Identity      = auth.Identity
	Authenticator = auth.Authenticator
	StaticKeys    = auth.StaticKeys
)

const (
	ScopeRead      = auth.ScopeRead
	ScopeWrite     = auth.ScopeWrite
	ScopeClaim     = auth.ScopeClaim
	ScopeSubmit    = auth.ScopeSubmit
	ScopeVerify    = auth.ScopeVerify
	ScopeIntegrate = auth.ScopeIntegrate
	ScopeAdmin     = auth.ScopeAdmin
)

// ErrUnauthenticated is re-exported for callers that check it directly.
var ErrUnauthenticated = auth.ErrUnauthenticated

// NewStaticKeys returns an empty in-memory key set.
func NewStaticKeys() *StaticKeys { return auth.NewStaticKeys() }
