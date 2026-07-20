// Tests for the dynamic model sync plumbing:
//
//   - DynamicProvider: snapshot semantics under concurrent Set/Models calls.
//   - ModelFetcher (OpenAI/Gemini): JSON decoding + status handling against
//     httptest servers, so we don't depend on real network during CI.
//   - StartDynamicSync: end-to-end retry + registry update against a fake
//     fetcher that becomes healthy after a fixed number of attempts.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDynamicProviderSetAndModels(t *testing.T) {
	dp := NewDynamicProvider("openai")

	if got := dp.Models(); got != nil {
		t.Fatalf("initial Models() should be nil, got %v", got)
	}

	prev := dp.Set([]string{"a", "b"})
	if prev != nil {
		t.Fatalf("Set with empty prior should return nil, got %v", prev)
	}
	if got := dp.Models(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Models() = %v; want [a b]", got)
	}

	old := dp.Set([]string{"a", "c"})
	if len(old) != 2 || old[0] != "a" || old[1] != "b" {
		t.Fatalf("Set returned wrong previous slice: %v", old)
	}
	if n := dp.SnapshotLen(); n != 2 {
		t.Fatalf("SnapshotLen = %d; want 2", n)
	}
}

func TestDynamicProviderConcurrentSafety(t *testing.T) {
	dp := NewDynamicProvider("anthropic")
	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				dp.Set([]string{"c1", "c2", "c3"})
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = dp.Models()
			}
		}()
	}
	wg.Wait()

	if dp.Models() == nil {
		t.Fatal("final Models() should not be nil")
	}
}

func TestRegistryUpdateModels(t *testing.T) {
	reg := NewRegistry()
	stub := &dynamicStubProvider{name: "openai", models: []string{"gpt-4o", "gpt-4o-mini"}}
	reg.Register(stub)

	if reg.AllModels() == nil || len(reg.AllModels()) != 2 {
		t.Fatalf("expected 2 models registered, got %v", reg.AllModels())
	}

	if !reg.UpdateModels("openai", []string{"gpt-4o-mini", "gpt-4.1"}) {
		t.Fatal("UpdateModels returned false unexpectedly")
	}
	got := reg.AllModels()
	want := []string{"gpt-4.1", "gpt-4o-mini"}
	if !equalStrings(got, want) {
		t.Fatalf("AllModels() = %v; want %v", got, want)
	}

	if _, _, err := reg.Resolve("gpt-4o"); err == nil {
		t.Fatal("gpt-4o should not resolve after removal (got nil err)")
	}
	p, m, err := reg.Resolve("gpt-4.1")
	if err != nil || m != "gpt-4.1" || p == nil {
		t.Fatalf("Resolve gpt-4.1: p=%v m=%q err=%v", p, m, err)
	}

	if reg.UpdateModels("nonexistent", nil) {
		t.Fatal("UpdateModels on missing provider should return false")
	}
}

func TestOpenAIModelFetcher(t *testing.T) {
	var hitCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing/wrong Authorization header: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "gpt-4o"},
				{"id": "gpt-4o-mini"},
				{"id": ""}, // empty id is filtered
			},
		})
	}))
	defer srv.Close()

	fetcher := NewOpenAIModelFetcher("test-key", srv.URL, 2*time.Second)
	models, err := fetcher(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hitCount != 1 {
		t.Fatalf("expected 1 request, got %d", hitCount)
	}
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-4o-mini" {
		t.Fatalf("models = %v; want [gpt-4o gpt-4o-mini]", models)
	}
}

func TestOpenAIModelFetcherUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	defer srv.Close()

	fetcher := NewOpenAIModelFetcher("k", srv.URL, 2*time.Second)
	_, err := fetcher(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 in error, got %v", err)
	}
}

func TestOpenAIModelFetcherMissingKey(t *testing.T) {
	fetcher := NewOpenAIModelFetcher("", "http://x", 2*time.Second)
	if _, err := fetcher(context.Background()); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestGeminiModelFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "gk" {
			t.Fatalf("expected ?key=gk, got %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{
				{"name": "models/gemini-2.5-flash"},
				{"name": "models/gemini-2.5-pro"},
				{"name": "models/"},
				{"name": ""},
			},
		})
	}))
	defer srv.Close()

	fetcher := NewGeminiModelFetcher("gk", srv.URL, 2*time.Second)
	models, err := fetcher(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 || models[0] != "gemini-2.5-flash" || models[1] != "gemini-2.5-pro" {
		t.Fatalf("models = %v; want [gemini-2.5-flash gemini-2.5-pro]", models)
	}
}

func TestStartDynamicSyncEndToEnd(t *testing.T) {
	reg := NewRegistry()
	stub := &dynamicStubProvider{name: "openai"}
	reg.Register(stub)

	dp := NewDynamicProvider("openai")
	try := 0
	fetcher := func(ctx context.Context) ([]string, error) {
		try++
		if try < 2 {
			return nil, errors.New("warming")
		}
		return []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1"}, nil
	}

	counters := &DynamicSyncCounters{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartDynamicSync(ctx, reg, dp, fetcher, 5*time.Second, 3, counters, discardLogger())

	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	for time.Now().Before(deadline) {
		if dp.SnapshotLen() == 3 {
			ok = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("dynamic sync did not converge; tries=%d", try)
	}
	got := reg.AllModels()
	want := []string{"gpt-4.1", "gpt-4o", "gpt-4o-mini"}
	if !equalStrings(got, want) {
		t.Fatalf("registry = %v; want %v", got, want)
	}
	s, e, _, _ := counters.Export()
	if s != 1 {
		t.Fatalf("success counter = %d; want 1", s)
	}
	if e != 1 {
		t.Fatalf("error counter = %d; want 1", e)
	}
}

// --- helpers ---------------------------------------------------------------

type dynamicStubProvider struct {
	name   string
	models []string
}

func (s *dynamicStubProvider) Name() string { return s.name }
func (s *dynamicStubProvider) Models() []string {
	return append([]string(nil), s.models...)
}
func (s *dynamicStubProvider) ChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, nil
}
func (s *dynamicStubProvider) ChatCompletionStream(ctx context.Context, req ChatCompletionRequest) (<-chan StreamEvent, error) {
	return nil, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
