package gateway

import "context"

// CallerCredential is a per-request provider secret override (BYOK). When
// present in the request context, provider adapters use it instead of their
// process-wide configured key/base URL. This keeps a single shared adapter
// instance per provider while letting each caller authenticate with their own
// key, so streaming, timeouts, and connection pooling are unchanged.
type CallerCredential struct {
	Secret  string // upstream provider API key
	BaseURL string // optional base URL override (e.g. self-hosted/proxy)
	Source  string // "user" | "org" | "env" — for observability only
}

type credentialCtxKey struct{}

// WithCallerCredential returns a context carrying a per-request credential
// override. A zero/empty Secret is treated as "no override" by adapters.
func WithCallerCredential(ctx context.Context, c CallerCredential) context.Context {
	return context.WithValue(ctx, credentialCtxKey{}, c)
}

// CallerCredentialFrom extracts a per-request credential override, if any.
func CallerCredentialFrom(ctx context.Context) (CallerCredential, bool) {
	c, ok := ctx.Value(credentialCtxKey{}).(CallerCredential)
	if !ok || c.Secret == "" {
		return CallerCredential{}, false
	}
	return c, true
}
