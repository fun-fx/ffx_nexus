package external

import (
	"context"
	"errors"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/observability"
)

// Credentials carries the secrets a plugin's auth block resolved to,
// in the order they appear in keyRef. Most vendors take a single
// token; Langfuse takes a (public key, secret key) pair for HTTP
// Basic auth, which is why this is a slice rather than one string.
type Credentials struct {
	Values []string
}

// Primary returns the first resolved secret, which is the token for
// single-key vendors. Empty when nothing resolved.
func (c Credentials) Primary() string {
	if len(c.Values) == 0 {
		return ""
	}
	return c.Values[0]
}

// Pair returns the first two resolved secrets as a username/password
// pair. ok is false unless both are present and non-empty, so callers
// cannot accidentally send half-formed Basic auth.
func (c Credentials) Pair() (string, string, bool) {
	if len(c.Values) < 2 || c.Values[0] == "" || c.Values[1] == "" {
		return "", "", false
	}
	return c.Values[0], c.Values[1], true
}

// Empty reports whether no secret resolved at all.
func (c Credentials) Empty() bool { return c.Primary() == "" }

// SecretResolver turns a manifest auth block into concrete secrets.
// Implementations must be safe for concurrent use — dispatch runs on
// every worker goroutine.
type SecretResolver interface {
	Resolve(ctx context.Context, auth evalplugin.AuthSpec) (Credentials, error)
}

// ErrNoSecretResolver is returned when a plugin declares an auth block
// but no resolver is wired. Failing loudly beats sending an
// unauthenticated request that the vendor rejects out of sight.
var ErrNoSecretResolver = errors.New("plugin declares auth but no secret resolver is configured")

// Target is everything an adapter needs to talk to a vendor for one
// trace. It replaced a bare `endpoint string` parameter, which had no
// room for credentials — the reason every adapter shipped
// unauthenticated and silently failed against real vendor APIs.
type Target struct {
	// Endpoint is spec.service.endpoint verbatim; adapters join their
	// own vendor path onto it.
	Endpoint string
	// Auth holds the resolved secrets for spec.service.auth.
	Auth Credentials
	// Plugin is the manifest, for adapters that need collect mappings
	// or per-plugin timeouts.
	Plugin *evalplugin.Plugin
	// Trace is the trace being evaluated. Adapters that speak OTLP
	// build their envelope from it. Note that it carries unredacted
	// content: anything derived from the prompt or completion must come
	// from the rendered payload, which has passed spec.send.redact.
	Trace observability.Trace
}

// PluginName is a nil-safe accessor used in log lines and errors.
func (t Target) PluginName() string {
	if t.Plugin == nil {
		return ""
	}
	return t.Plugin.Metadata.Name
}

// resolveAuth resolves a plugin's auth block through the resolver.
// A manifest with no auth block resolves to empty credentials without
// consulting the resolver, so self-hosted endpoints that need no key
// keep working.
func resolveAuth(ctx context.Context, r SecretResolver, p *evalplugin.Plugin) (Credentials, error) {
	if p == nil {
		return Credentials{}, nil
	}
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		return Credentials{}, nil
	}
	if r == nil {
		return Credentials{}, ErrNoSecretResolver
	}
	return r.Resolve(ctx, auth)
}
