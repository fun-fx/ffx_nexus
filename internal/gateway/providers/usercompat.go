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
}

// NewUserCompat returns a wrapper around an OpenAICompat adapter registered
// under name (e.g. "myprov"). The wrapper's Models() and EmbeddingModels()
// return strings prefixed with "user/<name>/" so the registry indexes the
// user-namespaced form; ChatCompletion/Stream/Embed strip that prefix before
// the inner openai call.
func NewUserCompat(inner *OpenAICompat) *UserCompat {
	return &UserCompat{
		inner:  inner,
		prefix: "user/" + inner.Name() + "/",
	}
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
