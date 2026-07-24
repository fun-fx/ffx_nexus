package gateway

import (
	"context"
	"testing"
)

// Fake provider used to prove RegisterHint records ScopeHint per name and
// per model id and that Resolve's hint lookup matches.
type fakeScopeProvider struct {
	name   string
	models []string
}

func (f *fakeScopeProvider) Name() string { return f.name }
func (f *fakeScopeProvider) Models() []string {
	return f.models
}
func (f *fakeScopeProvider) ChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{Model: req.Model}, nil
}
func (f *fakeScopeProvider) ChatCompletionStream(_ context.Context, req ChatCompletionRequest) (<-chan StreamEvent, error) {
	c := make(chan StreamEvent)
	close(c)
	return c, nil
}

func TestRegisterHintStoresScopeAndOwner(t *testing.T) {
	r := NewRegistry()
	p := &fakeScopeProvider{name: "teamprov", models: []string{"teamprov/gpt-5", "teamprov/gpt-4o"}}
	r.RegisterHint("teamprov", ScopeHint{Scope: ScopeOrg}, p)

	h, ok := r.HintForName("teamprov")
	if !ok || h.Scope != ScopeOrg {
		t.Fatalf("HintForName(name) = (%v, %v); want (org, true)", h, ok)
	}
	for _, m := range p.Models() {
		h, ok := r.HintForModel(m)
		if !ok || h.Scope != ScopeOrg {
			t.Fatalf("HintForModel(%q) = (%v, %v); want (org, true)", m, h, ok)
		}
	}
}

func TestRegisterHintDistinguishesPublicAndUser(t *testing.T) {
	r := NewRegistry()
	// Builtin = public
	r.RegisterHint("openai", ScopeHint{Scope: ScopePublic}, &fakeScopeProvider{name: "openai", models: []string{"openai/gpt-4o"}})
	// BYOK router owned by Alice
	r.RegisterHint("aliceprov", ScopeHint{Scope: ScopeUser, OwnerID: "u-alice"}, &fakeScopeProvider{name: "aliceprov", models: []string{"aliceprov/llama3"}})

	h1, _ := r.HintForName("openai")
	if h1.Scope != ScopePublic {
		t.Fatalf("openai scope: got %q, want public", h1.Scope)
	}
	h2, _ := r.HintForModel("aliceprov/llama3")
	if h2.Scope != ScopeUser || h2.OwnerID != "u-alice" {
		t.Fatalf("alice hint: got %+v, want user/u-alice", h2)
	}
}

func TestRegisterDefaultsToPublicWhenHintIsZero(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeScopeProvider{name: "legacy", models: []string{"legacy/foo"}})
	h, ok := r.HintForModel("legacy/foo")
	if !ok || h.Scope != ScopePublic {
		t.Fatalf("legacy model hint = (%v, %v); want (public, true)", h, ok)
	}
}

func TestUpdateModelsPreservesScope(t *testing.T) {
	r := NewRegistry()
	r.RegisterHint("teamprov", ScopeHint{Scope: ScopeOrg}, &fakeScopeProvider{name: "teamprov", models: []string{"teamprov/gpt-4o"}})
	// Replace catalog while keeping the hint.
	if !r.UpdateModels("teamprov", []string{"teamprov/gpt-3.5", "teamprov/gpt-4o"}) {
		t.Fatalf("UpdateModels returned false")
	}
	if h, ok := r.HintForModel("teamprov/gpt-4o"); !ok || h.Scope != ScopeOrg {
		t.Fatalf("post-update hint: %+v/%v", h, ok)
	}
	if _, ok := r.HintForModel("teamprov/gpt-4.1"); ok {
		t.Fatalf("removed model still has a hint")
	}
}
