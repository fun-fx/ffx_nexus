package semcache

import (
	"context"
	"testing"
)

type stubEmbedder struct {
	vec []float32
}

func (s stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return s.vec, nil
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if sim := cosineSimilarity(a, b); sim < 0.99 {
		t.Fatalf("identical vectors want ~1, got %f", sim)
	}
	c := []float32{0, 1, 0}
	if sim := cosineSimilarity(a, c); sim > 0.01 {
		t.Fatalf("orthogonal vectors want ~0, got %f", sim)
	}
}

func TestMemoryCacheHit(t *testing.T) {
	c := NewMemory(Config{Threshold: 0.99, MaxEntriesPerModel: 10})
	ctx := context.Background()
	vec := []float32{1, 0, 0}
	resp := []byte(`{"choices":[{"message":{"content":"Paris"}}]}`)

	if err := c.Store(ctx, "m", "q1", vec, resp); err != nil {
		t.Fatal(err)
	}
	hit, err := c.Lookup(ctx, "m", "q2", []float32{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || string(hit.ResponseJSON) != string(resp) {
		t.Fatalf("want cache hit, got %+v", hit)
	}
}

func TestMemoryCacheMiss(t *testing.T) {
	c := NewMemory(Config{Threshold: 0.99})
	ctx := context.Background()
	vec := []float32{1, 0, 0}
	_ = c.Store(ctx, "m", "q", vec, []byte(`{"ok":true}`))

	hit, err := c.Lookup(ctx, "m", "other", []float32{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("orthogonal vector should miss, got %+v", hit)
	}
}

func TestServiceLookupStore(t *testing.T) {
	mem := NewMemory(Config{Threshold: 0.95})
	svc := NewService(mem, stubEmbedder{vec: []float32{0.6, 0.8, 0}}, Config{Enabled: true})
	ctx := context.Background()

	hit, vec, err := svc.Lookup(ctx, "gpt-4o-mini", "capital of France?")
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatal("empty cache should miss")
	}

	resp := []byte(`{"model":"gpt-4o-mini","choices":[{"message":{"content":"Paris"}}]}`)
	if err := svc.Store(ctx, "gpt-4o-mini", "capital of France?", vec, resp); err != nil {
		t.Fatal(err)
	}

	hit, _, err = svc.Lookup(ctx, "gpt-4o-mini", "what is the capital of France")
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("expected hit after store with same embedding stub")
	}
}

func TestNewServiceDisabled(t *testing.T) {
	if NewService(nil, stubEmbedder{vec: []float32{1}}, Config{Enabled: false}) != nil {
		t.Fatal("disabled config should return nil")
	}
	if NewService(NewMemory(Config{}), nil, Config{Enabled: true}) != nil {
		t.Fatal("nil embedder should return nil")
	}
}
