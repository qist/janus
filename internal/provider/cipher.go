package provider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SecretBox seals credentials with AES-256-GCM. The key lives in the process
// environment (JANUS_MASTER_KEY) and never touches the database.
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox returns a box for a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

// ParseMasterKey accepts 32 bytes as 64 hex chars, base64 (std or raw url), or
// the raw 32-byte string. Anything else is rejected instead of being hashed
// silently: a short passphrase would look configured but weaken the seal.
func ParseMasterKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("master key required")
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("master key must be 32 bytes (64 hex chars, base64, or raw); got %d chars", len(raw))
}

// Seal returns base64(nonce || ciphertext).
func (b *SecretBox) Seal(plaintext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrSecretBox
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := b.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Open reverses Seal. A wrong key or a tampered ciphertext fails the tag check.
func (b *SecretBox) Open(encoded string) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrSecretBox
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	size := b.aead.NonceSize()
	if len(raw) <= size {
		return "", errors.New("sealed secret is truncated")
	}
	plaintext, err := b.aead.Open(nil, raw[:size], raw[size:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt secret: wrong key or tampered data")
	}
	return string(plaintext), nil
}

// Mask keeps a short suffix so an operator can tell two keys apart without
// exposing enough to use them.
func Mask(secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) <= 8 {
		return "••••"
	}
	return "…" + secret[len(secret)-4:]
}
