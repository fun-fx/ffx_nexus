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
		TraceID:       "trace-1",
		SpanID:        "span-1",
		OperationName: "chat",
		RequestModel:  "gemini-2.5-flash",
		ReplicaID:     "test-replica",
		LatencyMs:     42,
		StatusCode:    200,
	})
	rec.Record(Trace{
		TraceID:    "trace-2",
		SpanID:     "span-2",
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

	// Decode at least one body and confirm it's a valid JSON envelope of
	// our Trace type — i.e. the adapter preserved the data shape.
	var rows []Trace
	if err := json.Unmarshal(lastBodies[len(lastBodies)-1], &rows); err != nil {
		t.Fatalf("last body is not our JSON envelope: %v (%q)", err, lastBodies[len(lastBodies)-1])
	}
	if len(rows) == 0 {
		t.Fatal("envelope was empty")
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
