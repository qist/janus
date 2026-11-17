// Package auth owns API-key scopes and the short-lived capability tokens used
// for sensitive operations. A token is only useful to the executor; it is not
// an "approval message" the model can talk its way past.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTTL int64 = 60
	MaxTTL     int64 = 300
)

var (
	ErrInvalidToken   = errors.New("invalid capability token")
	ErrExpired        = errors.New("capability token expired")
	ErrReplay         = errors.New("capability token replayed")
	ErrActionMismatch = errors.New("capability action mismatch")
	ErrTTLTooLong     = errors.New("capability ttl too long")
)

// Action is the exact operation being authorized. It is hashed into the token,
// so changing a command or target invalidates the token.
type Action struct {
	Kind     string   `json:"kind"`
	Targets  []string `json:"targets,omitempty"`
	Command  string   `json:"command,omitempty"`
	DiffHash string   `json:"diff_hash,omitempty"`
}

// Claims is the signed body of a capability token.
type Claims struct {
	WorkItemID string   `json:"work_item_id"`
	LeaseEpoch int64    `json:"lease_epoch"`
	ActionHash string   `json:"action_hash"`
	Paths      []string `json:"paths,omitempty"`
	IssuedAt   int64    `json:"issued_at"`
	ExpiresAt  int64    `json:"expires_at"`
	Nonce      string   `json:"nonce"`
}

// Authority issues and verifies capability tokens. used tracks nonces so a
// token is single-use; production can replace it with shared storage.
type Authority struct {
	key  []byte
	now  func() int64
	mu   sync.Mutex
	used map[string]int64
}

// NewAuthority requires a strong HMAC key.
func NewAuthority(key []byte, now func() int64) (*Authority, error) {
	if len(key) < 32 {
		return nil, errors.New("capability key must be at least 32 bytes")
	}
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Authority{
		key:  append([]byte(nil), key...),
		now:  now,
		used: make(map[string]int64),
	}, nil
}

// Issue signs claims. Empty IssuedAt/ExpiresAt/Nonce are filled in; a TTL over
// MaxTTL is rejected rather than silently clamped.
func (a *Authority) Issue(claims Claims) (string, error) {
	if strings.TrimSpace(claims.WorkItemID) == "" {
		return "", errors.New("work item id required")
	}
	if claims.LeaseEpoch <= 0 {
		return "", errors.New("lease epoch required")
	}
	if strings.TrimSpace(claims.ActionHash) == "" {
		return "", errors.New("action hash required")
	}

	now := a.now()
	if claims.IssuedAt == 0 {
		claims.IssuedAt = now
	}
	if claims.ExpiresAt == 0 {
		claims.ExpiresAt = now + DefaultTTL
	}
	if claims.ExpiresAt <= now {
		return "", ErrExpired
	}
	if claims.ExpiresAt > now+MaxTTL {
		return "", ErrTTLTooLong
	}
	if claims.Nonce == "" {
		nonce, err := newNonce()
		if err != nil {
			return "", err
		}
		claims.Nonce = nonce
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode claims: %w", err)
	}
	sig := a.sign(payload)

	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify checks the signature, expiry, action hash, and nonce, then burns the
// nonce. The returned claims are the ones the executor must enforce.
func (a *Authority) Verify(token string, action Action) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Claims{}, ErrInvalidToken
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	if !hmac.Equal(sig, a.sign(payload)) {
		return Claims{}, ErrInvalidToken
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if claims.Nonce == "" {
		return Claims{}, ErrInvalidToken
	}

	now := a.now()
	if claims.ExpiresAt <= now {
		return Claims{}, ErrExpired
	}
	if claims.ActionHash != HashAction(action) {
		return Claims{}, ErrActionMismatch
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for nonce, expiresAt := range a.used {
		if expiresAt <= now {
			delete(a.used, nonce)
		}
	}
	if _, ok := a.used[claims.Nonce]; ok {
		return Claims{}, ErrReplay
	}
	a.used[claims.Nonce] = claims.ExpiresAt
	return claims, nil
}

// HashAction returns the canonical action hash embedded in a token.
func HashAction(action Action) string {
	canonical := action
	canonical.Targets = append([]string(nil), action.Targets...)
	sort.Strings(canonical.Targets)

	payload, _ := json.Marshal(canonical)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (a *Authority) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
