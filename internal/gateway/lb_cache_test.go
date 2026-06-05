package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ffxnexus/nexus/internal/balancer"
	"github.com/ffxnexus/nexus/internal/semcache"
)

func TestLoadBalancingRotatesPrimary(t *testing.T) {
	bad := &stubProvider{name: "bad", models: []string{"m-a"}, fail: false}
	good := &stubProvider{name: "good", models: []string{"m-b"}, fail: false}
	h := newTestHandler(bad, good)
	h.SetRouter(stubRouter{chain: []string{"m-a", "m-b"}}, map[string][]string{"pool": {"m-a", "m-b"}})
	h.SetLoadBalancing(balancer.NewWeightedRR())

	var primaries []string
	for i := 0; i < 8; i++ {
		rec := doChat(h, `{"model":"pool","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: want 200, got %d", i, rec.Code)
		}
		var resp ChatCompletionResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		primaries = append(primaries, resp.Choices[0].Message.Content)
	}
	// stubProvider returns "ok from {name}" — weighted rotation still spreads the
	// primary across providers, so not every answer should come from the same one.
	uniq := map[string]bool{}
	for _, p := range primaries {
		uniq[p] = true
	}
	if len(uniq) < 2 {
		t.Fatalf("load balancing should rotate primary across providers, got %v", primaries)
	}
}

type fixedEmbedder struct {
	vec []float32
}

func (f fixedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return f.vec, nil
}

func TestSemanticCacheHit(t *testing.T) {
	calls := 0
	p := &stubProvider{name: "p", models: []string{"m"}, fail: false, calls: &calls}
	h := newTestHandler(p)
	mem := semcache.NewMemory(semcache.Config{Threshold: 0.99, MaxEntriesPerModel: 100})
	svc := semcache.NewService(mem, fixedEmbedder{vec: []float32{1, 0, 0}}, semcache.Config{Enabled: true})
	h.SetSemanticCache(svc)

	body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	rec1 := doChat(h, body)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call want 200, got %d", rec1.Code)
	}

	callsBefore := calls
	// Second identical-ish call should hit cache (same embedding stub).
	rec2 := doChat(h, body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call want 200, got %d", rec2.Code)
	}
	if calls != callsBefore {
		t.Fatalf("cache hit should skip upstream, calls went from %d to %d", callsBefore, calls)
	}
}

// TestSemanticCacheSurvivesLoadBalancing verifies the cache is keyed by the
// client-requested alias, not the concrete model the balancer rotates to, so a
// near-duplicate prompt hits the cache regardless of LB rotation.
func TestSemanticCacheSurvivesLoadBalancing(t *testing.T) {
	total := 0
	a := &stubProvider{name: "a", models: []string{"m-a"}, calls: &total}
	b := &stubProvider{name: "b", models: []string{"m-b"}, calls: &total}
	h := newTestHandler(a, b)
	h.SetRouter(stubRouter{chain: []string{"m-a", "m-b"}}, map[string][]string{"pool": {"m-a", "m-b"}})
	h.SetLoadBalancing(balancer.NewWeightedRR())
	mem := semcache.NewMemory(semcache.Config{Threshold: 0.99, MaxEntriesPerModel: 100})
	svc := semcache.NewService(mem, fixedEmbedder{vec: []float32{1, 0, 0}}, semcache.Config{Enabled: true})
	h.SetSemanticCache(svc)

	body := `{"model":"pool","messages":[{"role":"user","content":"hello"}]}`
	if rec := doChat(h, body); rec.Code != http.StatusOK {
		t.Fatalf("first call want 200, got %d", rec.Code)
	}
	if total != 1 {
		t.Fatalf("first call should hit upstream once, got %d", total)
	}
	// Several more calls: even as LB rotates the primary model, all should be
	// served from the alias-keyed cache without new upstream calls.
	for i := 0; i < 5; i++ {
		if rec := doChat(h, body); rec.Code != http.StatusOK {
			t.Fatalf("call %d want 200, got %d", i, rec.Code)
		}
	}
	if total != 1 {
		t.Fatalf("alias-keyed cache should absorb LB rotation, got %d upstream calls", total)
	}
}

func TestSemanticCacheSkippedForTools(t *testing.T) {
	calls := 0
	p := &stubProvider{name: "p", models: []string{"m"}, calls: &calls}
	h := newTestHandler(p)
	mem := semcache.NewMemory(semcache.Config{Threshold: 0.99})
	svc := semcache.NewService(mem, fixedEmbedder{vec: []float32{1, 0, 0}}, semcache.Config{Enabled: true})
	h.SetSemanticCache(svc)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	doChat(h, body)
	doChat(h, body)
	if calls != 2 {
		t.Fatalf("tool requests must bypass cache, got %d upstream calls", calls)
	}
}
