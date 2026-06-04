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
