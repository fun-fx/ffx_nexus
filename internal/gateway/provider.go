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

// ModerationsProvider is an optional capability for OpenAI-compatible
// /v1/moderations. A provider that does not implement it cannot serve
// moderation requests; the handler returns 404 model_not_found.
type ModerationsProvider interface {
	Provider
	// ModerationModels is the set of moderation model ids the provider serves
	// (e.g. omni-moderation-latest, text-moderation-stable).
	ModerationModels() []string
	// Moderate runs the moderation call.
	Moderate(ctx context.Context, req ModerationRequest) (*ModerationResponse, error)
}

// ImageGenerationProvider is an optional capability for OpenAI-compatible
// /v1/images/generations. Same discovery pattern via type assertion.
type ImageGenerationProvider interface {
	Provider
	// ImageModels returns the set of image model ids the provider serves
	// (e.g. dall-e-3, gpt-image-1).
	ImageModels() []string
	// GenerateImages runs the image generation call.
	GenerateImages(ctx context.Context, req ImageGenerationRequest) (*ImageGenerationResponse, error)
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

// UpdateModels replaces the catalog of model ids indexed under the given
// provider name. The provider instance itself stays the same; only the
// byModel index is rewritten. Use this from the dynamic sync worker so a
// refresh does not need to re-register (which would lock the registry and
// race with the hot path).
//
// If name does not refer to a registered provider the call is a no-op and
// returns false so the caller can log a stale refresh.
func (r *Registry) UpdateModels(name string, models []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[name]
	if !ok {
		return false
	}
	for id, existing := range r.byModel {
		if existing == p {
			delete(r.byModel, id)
		}
	}
	for _, m := range models {
		r.byModel[m] = p
	}
	return true
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
// returned Provider is also a generic Provider so callers can read its name
// for trace attribution. ok=false means no embed-capable provider serves
// that model. Honors a "provider/model" prefix just like Resolve.
func (r *Registry) ResolveEmbedding(model string) (EmbeddingsProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name, rest, hit := strings.Cut(model, "/"); hit {
		for _, p := range r.providers {
			ep, ok := p.(EmbeddingsProvider)
			if !ok || p.Name() != name {
				continue
			}
			for _, m := range ep.EmbeddingModels() {
				if m == rest {
					return ep, true
				}
			}
		}
		return nil, false
	}
	for _, p := range r.providers {
		ep, ok := p.(EmbeddingsProvider)
		if !ok {
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

// AllModerationModels returns the union of moderation model ids from every
// provider that implements ModerationsProvider, sorted for stable output.
func (r *Registry) AllModerationModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, p := range r.providers {
		mp, ok := p.(ModerationsProvider)
		if !ok {
			continue
		}
		for _, m := range mp.ModerationModels() {
			seen[m] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ResolveModeration picks a ModerationsProvider for the requested model. An
// empty model falls back to the first registered provider's primary model
// (matches the OpenAI default of omni-moderation-latest).
func (r *Registry) ResolveModeration(model string) (ModerationsProvider, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		mp, ok := p.(ModerationsProvider)
		if !ok {
			continue
		}
		for _, m := range mp.ModerationModels() {
			if model == "" || model == m {
				return mp, m, true
			}
		}
		if name, rest, hit := strings.Cut(model, "/"); hit && p.Name() == name {
			for _, m := range mp.ModerationModels() {
				if m == rest {
					return mp, m, true
				}
			}
		}
	}
	return nil, "", false
}

// AllImageModels returns the union of image model ids across providers.
func (r *Registry) AllImageModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, p := range r.providers {
		ip, ok := p.(ImageGenerationProvider)
		if !ok {
			continue
		}
		for _, m := range ip.ImageModels() {
			seen[m] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ResolveImage picks an ImageGenerationProvider for the requested model.
// An empty model resolves to the first image-capable provider's primary
// model (matches the OpenAI default dall-e-3). Honors a "provider/model"
// prefix the same way Resolve does.
func (r *Registry) ResolveImage(model string) (ImageGenerationProvider, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name, rest, ok := strings.Cut(model, "/"); ok {
		for _, p := range r.providers {
			ip, ok := p.(ImageGenerationProvider)
			if !ok || p.Name() != name {
				continue
			}
			for _, m := range ip.ImageModels() {
				if m == rest {
					return ip, m, true
				}
			}
		}
		return nil, "", false
	}
	for _, p := range r.providers {
		ip, ok := p.(ImageGenerationProvider)
		if !ok {
			continue
		}
		ms := ip.ImageModels()
		if model == "" && len(ms) > 0 {
			return ip, ms[0], true
		}
		for _, m := range ms {
			if m == model {
				return ip, m, true
			}
		}
	}
	return nil, "", false
}
