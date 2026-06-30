package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Provider is a backend LLM provider adapter. Adapters translate the canonical
// OpenAI-compatible schema to/from their native API.
type Provider interface {
	// Name returns the provider identifier (e.g. "openai", "anthropic").
	Name() string

	// Models returns the set of model IDs this provider serves.
	Models() []string

	// ChatCompletion performs a non-streaming completion.
	ChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error)

	// ChatCompletionStream performs a streaming completion. The returned channel
	// is closed when the stream ends. The caller must drain it.
	ChatCompletionStream(ctx context.Context, req ChatCompletionRequest) (<-chan StreamEvent, error)
}

// EmbeddingsProvider is an optional capability for providers that also serve
// an OpenAI-compatible /v1/embeddings endpoint. The gateway uses the type
// assertion to discover embed-capable providers and to validate embedding
// model ids. Providers that do not implement this interface simply do not
// satisfy any embeddings request.
type EmbeddingsProvider interface {
	Provider
	// EmbeddingModels returns the set of embedding model ids this provider serves.
	EmbeddingModels() []string
	// Embed performs the embeddings call and returns one vector per input.
	Embed(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error)
}

// Registry maps model IDs to providers and supports prefix-based routing
// (e.g. "openai/gpt-4o" forces the openai provider).
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider // by name
	byModel   map[string]Provider // exact model id -> provider
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
		byModel:   make(map[string]Provider),
	}
}

// Register adds a provider and indexes its advertised models.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
	for _, m := range p.Models() {
		r.byModel[m] = p
	}
}

// Resolve picks a provider for the requested model. It supports an explicit
// "provider/model" prefix and falls back to exact model-id lookup. It returns
// the resolved provider and the de-prefixed model id to forward.
func (r *Registry) Resolve(model string) (Provider, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if name, rest, ok := strings.Cut(model, "/"); ok {
		if p, found := r.providers[name]; found {
			return p, rest, nil
		}
	}
	if p, found := r.byModel[model]; found {
		return p, model, nil
	}
	return nil, "", fmt.Errorf("no provider registered for model %q", model)
}

// ProviderFor returns a provider by name.
func (r *Registry) ProviderFor(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// AllModels returns every registered model id, sorted.
func (r *Registry) AllModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byModel))
	for m := range r.byModel {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// AllEmbeddingModels returns the union of embedding model ids from every
// provider that implements EmbeddingsProvider, sorted for stable output.
func (r *Registry) AllEmbeddingModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, p := range r.providers {
		if ep, ok := p.(EmbeddingsProvider); ok {
			for _, m := range ep.EmbeddingModels() {
				seen[m] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ResolveEmbedding picks an EmbeddingsProvider for the requested model. The
// returned Provider is also a generic Provider so callers can read its name for
// trace attribution. ok=false means no embed-capable provider serves that model.
func (r *Registry) ResolveEmbedding(model string) (EmbeddingsProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		ep, ok := p.(EmbeddingsProvider)
		if !ok {
			continue
		}
		// Honor an explicit "provider/model" prefix so a multi-tenant deployment
		// can disambiguate when two providers share a model id.
		if name, rest, hit := strings.Cut(model, "/"); hit {
			if p.Name() == name {
				for _, m := range ep.EmbeddingModels() {
					if m == rest {
						return ep, true
					}
				}
			}
			continue
		}
		for _, m := range ep.EmbeddingModels() {
			if m == model {
				return ep, true
			}
		}
	}
	return nil, false
}
