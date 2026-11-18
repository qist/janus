package gateway

import "context"

// affinityKey carries a conversation/session identity to the credential pool.
// Keeping it in the context avoids widening CredentialSource for callers that
// do not care: a request without affinity simply gets the normal selection.
type affinityKey struct{}

// WithAffinity marks a request as belonging to a conversation. The pool pins
// the same account while it is usable, which preserves the prompt cache.
func WithAffinity(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, affinityKey{}, key)
}

// Affinity returns the conversation identity, if any.
func Affinity(ctx context.Context) string {
	value, _ := ctx.Value(affinityKey{}).(string)
	return value
}
