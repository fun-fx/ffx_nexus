package observability

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOTLPRecorderRoundTrip verifies the OTLP exporter posts a JSON
// envelope per flush and accepts 2xx as a happy path. Other adapters
// (CHRecorder, MetricsRecorder) are exercised elsewhere; this test only
// covers OTLP-specific envelope plumbing.
func TestOTLPRecorderRoundTrip(t *testing.T) {
	var (
		received   atomic.Int32
		bodiesMu   sync.Mutex
		lastBodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back a valid 200 so the exporter's happy path reaches the
		// "send complete" branch without warn logs.
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		lastBodies = append(lastBodies, body)
		bodiesMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := NewOTLPRecorder(OTLPOptions{
		Endpoint:   srv.URL + "/v1/traces",
		BatchSize:  1, // flush every record so the test doesn't have to wait
		FlushEvery: 100 * time.Millisecond,
		BufferSize: 8,
		Timeout:    2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rec == nil {
		t.Fatal("endpoint not empty; expected a non-nil recorder")
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	rec.Record(Trace{
		TraceID:       "abcdef01abcdef01abcdef01abcdef01",
		SpanID:        "abcdef01",
		OperationName: "chat",
		RequestModel:  "gemini-2.5-flash",
		ReplicaID:     "test-replica",
		LatencyMs:     42,
		StatusCode:    200,
	})
	rec.Record(Trace{
		TraceID:    "cafef00dcafef00dcafef00dcafef00d",
		SpanID:     "cafef00d",
		StatusCode: 502,
		ErrorType:  "upstream_error",
	})

	if !waitUntil(time.Second, func() bool { return received.Load() >= 1 }) {
		t.Fatalf("expected ≥1 POST to the OTLP endpoint, got %d", received.Load())
	}

	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(lastBodies) == 0 {
		t.Fatal("server received no bodies")
	}

	// Decode at least one body and confirm it's the OTLP envelope
	// shape (`resourceSpans` array, not a bare JSON array of Trace).
	// This is the exact fix for the V3 `otlp unexpected status code 400`
	// reported from production.
	//
	// Note: with BatchSize=1, each Record call flushes individually, so
	// `lastBodies` has 2 envelopes. We scan them all and assert both
	// trace ids made it into the envelope in some order.
	//
	// OTLP uses snake_case JSON keys (`scope_spans`, `resource_spans`,
	// `trace_id`, …) so we decode into a `map[string]any` instead of a
	// Go struct to avoid a json tag schema duplicate.
	seenTraceIDs := map[string]bool{}
	scopeNames := map[string]int{}
	for _, body := range lastBodies {
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("body is not OTLP envelope: %v (%q)", err, body)
		}
		rsAny, _ := env["resourceSpans"].([]any)
		if len(rsAny) == 0 {
			continue
		}
		rs, _ := rsAny[0].(map[string]any)
		ssAny, _ := rs["scope_spans"].([]any)
		if len(ssAny) == 0 {
			t.Fatalf("scope_spans empty; body=%s", body)
		}
		ss, _ := ssAny[0].(map[string]any)
		scope := ss["scope"].(map[string]any)
		scopeNames[scope["name"].(string)]++
		spans, _ := ss["spans"].([]any)
		for _, sAny := range spans {
			s, _ := sAny.(map[string]any)
			if id, ok := s["trace_id"].(string); ok {
				seenTraceIDs[id] = true
			}
		}
	}
	if scopeNames["ffx_nexus"] == 0 {
		t.Errorf("expected scope name ffx_nexus in some envelope, got %v", scopeNames)
	}
	for _, want := range []string{"abcdef01abcdef01abcdef01abcdef01", "cafef00dcafef00dcafef00dcafef00d"} {
		if !seenTraceIDs[want] {
			t.Errorf("trace id %q did not appear in any envelope", want)
		}
	}
}

// TestOTLPRecorderDisabledWhenEmptyEndpoint confirms that an empty
// NEXUS_OTLP_ENDPOINT returns a nil recorder so MultiRecorder doesn't
// fan out to a dead sink.
func TestOTLPRecorderDisabledWhenEmptyEndpoint(t *testing.T) {
	rec := NewOTLPRecorder(OTLPOptions{Endpoint: ""}, nil)
	if rec != nil {
		t.Fatal("empty endpoint must produce nil recorder (opt-in contract)")
	}
	// nil-receiver Record must be a no-op (so MultiRecorder is safe).
	rec.Record(Trace{TraceID: "noop"})
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
