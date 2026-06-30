package providers

import (
	"strings"
	"testing"
)

// Verify that the Groq / Mistral compat constructors expose the keys the
// gateway needs: a stable provider name for routing, the catalog of chat
// model ids advertised in /v1/models, and the right wire shape (same
// OpenAI adapter under the hood).
func TestGroqProviderShape(t *testing.T) {
	g := NewGroq("gsk-test", 0)
	if g.Name() != "groq" {
		t.Fatalf("want name=groq, got %q", g.Name())
	}
	models := g.Models()
	if len(models) == 0 {
		t.Fatalf("Groq adapter advertises no chat models")
	}
	if !strings.HasPrefix(g.OpenAI.baseURL, "https://api.groq.com") {
		t.Fatalf("Groq adapter should point at the Groq base URL; got %q", g.OpenAI.baseURL)
	}
	// All chat models should be unique.
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m] {
			t.Fatalf("duplicate chat model on Groq provider: %q", m)
		}
		seen[m] = true
	}
	// Groq does NOT expose OpenAI-shaped embeddings on the production API.
	if ems := g.EmbeddingModels(); len(ems) != 0 {
		t.Fatalf("Groq adapter should not advertise embedding models; got %v", ems)
	}
}

func TestMistralProviderShape(t *testing.T) {
	m := NewMistral("mistral-test", 0)
	if m.Name() != "mistral" {
		t.Fatalf("want name=mistral, got %q", m.Name())
	}
	models := m.Models()
	if len(models) == 0 {
		t.Fatalf("Mistral adapter advertises no chat models")
	}
	if !strings.HasPrefix(m.OpenAI.baseURL, "https://api.mistral.ai") {
		t.Fatalf("Mistral adapter should point at the Mistral base URL; got %q", m.OpenAI.baseURL)
	}
	if ems := m.EmbeddingModels(); len(ems) == 0 {
		t.Fatalf("Mistral adapter must advertise its embedding models")
	}
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m] {
			t.Fatalf("duplicate chat model on Mistral provider: %q", m)
		}
		seen[m] = true
	}
}

// OpenAICompat should let callers pre-register an OpenAI-shaped backend
// under a custom name plus a custom chat/model list (used by admin when
// they need to limit the model catalog or add a private deployment).
func TestNewOpenAICompatCustomCatalog(t *testing.T) {
	c := NewOpenAICompat(
		"localai", "sk-local", "https://localai.example/v1",
		[]string{"phi-3", "llama-3.1-8b"}, nil, nil, nil, 0,
	)
	if c.Name() != "localai" {
		t.Fatalf("want name=localai, got %q", c.Name())
	}
	if got := c.Models(); len(got) != 2 || got[0] != "phi-3" || got[1] != "llama-3.1-8b" {
		t.Fatalf("custom chat catalog not surfaced; got %v", got)
	}
}
