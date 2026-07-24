// Package providers: dynamic OpenAI-compatible adapter that namespaces a
// owner-defined credential's model ids under "user/<provider>/<model>" so
// they cannot collide with the built-in catalog id space at /v1/models.
//
// The wrapper forwards chat completion / streaming / embeddings to the inner
// OpenAICompat adapter at a user-supplied base URL (per credential), stripping
// the "user/<provider>/" prefix from `req.Model` before the upstream call so
// the third-party sees the model id it expects.
package providers

import (
	"context"
	"strings"

	"log/slog"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// UserCompat wraps an OpenAICompat adapter so its model ids are exposed in
// the registry under "user/<provider>/<model>" while the upstream wire call
// still sends the raw model id. This is what backs credentials registered
// through the new console flow (any provider name + base URL), so an owner
// can plug in an OpenAI-shaped endpoint without us shipping a Go adapter for
// every new vendor.
type UserCompat struct {
	inner *OpenAICompat
	// prefix is "user/<inner.Name()>/", cached on construction so per-request
	// prefix-strip is just a string comparison + slice.
	prefix string

	// ownerID is the user_id of the credential owner. Empty when the
	// credential is org-level (i.e. registered by an admin for everyone).
	ownerID string
	// scope duplicates the registry's ScopeHint so callers that already hold
	// the wrapper (and not the Registry) can answer scoping questions without
	// reaching back through the package boundary.
	scope gateway.Scope
}

// UserCompatOpts describes the visibility/tag metadata attached when a
// tenant BYOK credential is wired into the gateway. Pass OwnerID="" + nil
// Scope to tag the registration as org-level (sharing the credential across
// every member of the org); pass OwnerID=<user> to limit visibility to a
// single personal account.
type UserCompatOpts struct {
	OwnerID string
	Scope   gateway.Scope // ScopePublic | ScopeOrg | ScopeUser
}

// NewUserCompat returns a wrapper around an OpenAICompat adapter registered
// under name (e.g. "myprov"). The wrapper's Models() and EmbeddingModels()
// return strings prefixed with "user/<name>/" so the registry indexes the
// user-namespaced form; ChatCompletion/Stream/Embed strip that prefix before
// the inner openai call. Scope defaults to user when only OwnerID is set,
// and to org otherwise; pass an explicit Scope in opts to override.
func NewUserCompat(inner *OpenAICompat, opts UserCompatOpts) *UserCompat {
	scope := opts.Scope
	if scope == "" {
		if opts.OwnerID != "" {
			scope = gateway.ScopeUser
		} else {
			scope = gateway.ScopeOrg
		}
	}
	return &UserCompat{
		inner:   inner,
		prefix:  "user/" + inner.Name() + "/",
		ownerID: opts.OwnerID,
		scope:   scope,
	}
}

// OwnerID returns the credential owner's user_id (empty for org-shared
// credentials). Used by the registry/console catalog to answer the
// "is this user allowed to see this provider?" question without a second
// source-of-truth lookup.
func (u *UserCompat) OwnerID() string { return u.ownerID }

// Scope returns the visibility scope attached to this wrapper (see
// internal/gateway.Scope). Mirrors the ScopeHint the registry stores for
// the same provider name.
func (u *UserCompat) Scope() gateway.Scope { return u.scope }

// LogValue makes UserCompat printable via slog without dumping the inner
// adapter. Handy in boot logs where we already iterate every registered
// provider.
func (u *UserCompat) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", u.inner.Name()),
		slog.String("scope", string(u.scope)),
		slog.String("owner", u.ownerID),
	)
}

// Name implements Provider. Returns the wrapped provider's name (unprefixed).
func (u *UserCompat) Name() string { return u.inner.Name() }

// Models implements Provider. Returns the inner adapter's model ids each
// prefixed with "user/<name>/".
func (u *UserCompat) Models() []string {
	raw := u.inner.Models()
	out := make([]string, len(raw))
	for i, m := range raw {
		out[i] = u.prefix + m
	}
	return out
}

// EmbeddingModels implements EmbeddingsProvider. Returns prefixed ids.
func (u *UserCompat) EmbeddingModels() []string {
	raw := u.inner.EmbeddingModels()
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, len(raw))
	for i, m := range raw {
		out[i] = u.prefix + m
	}
	return out
}

// ChatCompletion strips the user/<name>/ prefix from req.Model and forwards
// to the inner adapter.
func (u *UserCompat) ChatCompletion(ctx context.Context, req gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	req.Model = strings.TrimPrefix(req.Model, u.prefix)
	return u.inner.ChatCompletion(ctx, req)
}

// ChatCompletionStream strips the prefix and forwards.
func (u *UserCompat) ChatCompletionStream(ctx context.Context, req gateway.ChatCompletionRequest) (<-chan gateway.StreamEvent, error) {
	req.Model = strings.TrimPrefix(req.Model, u.prefix)
	return u.inner.ChatCompletionStream(ctx, req)
}

// Embed strips the prefix and forwards.
func (u *UserCompat) Embed(ctx context.Context, req gateway.EmbeddingRequest) (*gateway.EmbeddingResponse, error) {
	req.Model = strings.TrimPrefix(req.Model, u.prefix)
	return u.inner.Embed(ctx, req)
}
