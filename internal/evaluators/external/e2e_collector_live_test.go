package external

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
)

// memSink captures every score the Collector hands to the durable
// store. Used by the live E2E collector tests to assert that a vendor
// payload actually reached the storage seam after ResultMapping —
// that is the cross-component invariant the per-adapter unit tests
// cannot prove.
//
// Thread-safe so poller goroutines and synchronous webhook calls can
// share one instance.
type memSink struct {
	mu     sync.Mutex
	scores []evals.Score
}

func (s *memSink) WriteScores(_ context.Context, scores []evals.Score) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scores = append(s.scores, scores...)
	return nil
}

func (s *memSink) snapshot() []evals.Score {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]evals.Score, len(s.scores))
	copy(out, s.scores)
	return out
}

// memOtlpSink captures every (trace id, score) pair the Collector
// fans out after a successful WriteScores. The fan-out is the
// separate OTLP seam that mirrors successful scores to an
// OTel-aware downstream — a sink failure here must not lose the
// score (Collector logs and continues). Used by the OTLP fan-out
// live E2E.
type memOtlpSink struct {
	mu     sync.Mutex
	events []otlpEvent
}

type otlpEvent struct {
	TraceID string
	Score   evals.Score
}

func (s *memOtlpSink) EmitShip(_ context.Context, traceID string, score evals.Score) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, otlpEvent{TraceID: traceID, Score: score})
	return nil
}

func (s *memOtlpSink) snapshot() []otlpEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]otlpEvent, len(s.events))
	copy(out, s.events)
	return out
}

// collectorLiveRegistry is the same seam the dispatcher live E2E
// used: a webhook-shaped plugin manifest whose validation passes,
// stored in a Registry the Collector can syncPollers from. We use
// the webhook service type so no vendor adapter is required — the
// collector pipeline is the unit under test, not the wire shape.
func collectorLiveRegistry(t *testing.T, mode, interval string) *evalplugin.Registry {
	t.Helper()
	manifest := `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: collector-live
spec:
  service:
    type: webhook
    endpoint: http://placeholder.invalid/never
    auth:
      keyRef: k1
  send:
    trigger: on_trace
    sampling: 1
  collect:
    mode: ` + mode + `
    interval: ` + interval + `
    mapping:
      name: name
      score: value
      explanation: comment
      trace_id: traceId
  timeout: 30s
`
	reg := evalplugin.NewRegistry()
	p, err := evalplugin.Decode([]byte(manifest))
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if err := evalplugin.Validate(p); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if discarded := reg.Merge([]evalplugin.Record{{
		Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin:  p,
		Enabled: true,
	}}); len(discarded) > 0 {
		t.Fatalf("merge discard: %d", len(discarded))
	}
	return reg
}

// TestE2E_Collector_Webhook_SingleObjectReachesSink validates the
// most common inbound path: an external service posts a single
// JSON object to the plugin's webhook URL. The Collector parses it,
// runs it through ResultMapping, and writes one Score to the
// durable sink. The live assertion is on the post-mapping fields,
// not the wire bytes — wire shape is covered by the per-adapter
// unit tests.
func TestE2E_Collector_Webhook_SingleObjectReachesSink(t *testing.T) {
	reg := collectorLiveRegistry(t, "webhook", "60s")
	sink := &memSink{}
	c := NewCollector(reg, sink, nil)
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"k1"}}})
	c.SetLogger(silentLog())

	body := io.NopCloser(bytesNewReader([]byte(`{
		"name": "answer-relevance",
		"value": 0.91,
		"comment": "covered the question",
		"traceId": "live-trc-1"
	}`)))
	if err := c.Webhook("collector-live", body); err != nil {
		t.Fatalf("Webhook returned err: %v", err)
	}

	scores := sink.snapshot()
	if len(scores) != 1 {
		t.Fatalf("expected exactly 1 score, got %d (%+v)", len(scores), scores)
	}
	got := scores[0]
	if got.TraceID != "live-trc-1" {
		t.Errorf("TraceID: got %q want %q", got.TraceID, "live-trc-1")
	}
	if got.Metric != "answer-relevance" {
		t.Errorf("Metric: got %q want %q", got.Metric, "answer-relevance")
	}
	if got.Score != 0.91 {
		t.Errorf("Score: got %v want 0.91", got.Score)
	}
	if !got.Passed {
		t.Errorf("Passed: got false, want true (0.91 >= 0.5)")
	}
	if got.Rationale != "covered the question" {
		t.Errorf("Rationale: got %q want %q", got.Rationale, "covered the question")
	}
	if got.Evaluator != "plugin:collector-live" {
		t.Errorf("Evaluator: got %q want plugin:collector-live", got.Evaluator)
	}
	if time.Since(got.Timestamp) > 10*time.Second {
		t.Errorf("Timestamp not recent: %v", got.Timestamp)
	}
}

// TestE2E_Collector_Webhook_BatchArrayReachesSink validates the
// second inbound path: an external service posts a JSON array (the
// "batched webhook" shape vendors use when several evaluations
// settle in one delivery). The Collector splits the array before
// applying the mapping, so each element is independent — the
// assertion is one Score per array element, each carrying *its own*
// traceId.
func TestE2E_Collector_Webhook_BatchArrayReachesSink(t *testing.T) {
	reg := collectorLiveRegistry(t, "webhook", "60s")
	sink := &memSink{}
	c := NewCollector(reg, sink, nil)
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"k1"}}})
	c.SetLogger(silentLog())

	batch := `[
		{"name":"x","value":0.4,"traceId":"b-1"},
		{"name":"x","value":0.95,"traceId":"b-2"},
		{"name":"x","value":0.6,"traceId":"b-3"}
	]`
	body := io.NopCloser(bytesNewReader([]byte(batch)))
	if err := c.Webhook("collector-live", body); err != nil {
		t.Fatalf("Webhook returned err: %v", err)
	}

	scores := sink.snapshot()
	if len(scores) != 3 {
		t.Fatalf("expected 3 scores, got %d", len(scores))
	}
	gotIDs := map[string]float64{}
	for _, sc := range scores {
		gotIDs[sc.TraceID] = sc.Score
	}
	want := map[string]float64{
		"b-1": 0.4,
		"b-2": 0.95,
		"b-3": 0.6,
	}
	for id, s := range want {
		if gotIDs[id] != s {
			t.Errorf("trace %q score: got %v want %v", id, gotIDs[id], s)
		}
	}
	// 0.4 should fail the 0.5 threshold; 0.6 and 0.95 should pass.
	passedPerID := map[string]bool{}
	for _, sc := range scores {
		passedPerID[sc.TraceID] = sc.Passed
	}
	if passedPerID["b-1"] {
		t.Errorf("b-1 should fail 0.5 threshold (got 0.4)")
	}
	if !passedPerID["b-2"] || !passedPerID["b-3"] {
		t.Errorf("b-2/b-3 should pass 0.5 threshold")
	}
}

// TestE2E_Collector_Poll_CollectFuncReachesSink validates the
// polling side of the collector — the third inbound path, and the
// one most prone to silent regression because failures show up as
// "no results" rather than a typed error. The test points the
// plugin's spec.collect.endpoint at a real httptest vendor; the
// vendor returns a JSON array; the Collector applies the mapping
// and writes to the sink within one tick of a 30ms interval.
//
// Using a real HTTP server (rather than an in-Process CollectFunc)
// is what makes this a live E2E — it's the same carrier path the
// production HTTP adapters take when an operator hits "Test".
func TestE2E_Collector_Poll_CollectFuncReachesSink(t *testing.T) {
	srv, hits, mu, serverHits := startMockVendor(t)
	// Mock vendor's /collect path returns a single result.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collect", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(serverHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`[{"name":"poll-quality","value":0.88,"comment":"polled","traceId":"polled-1"}]`))
	})
	// Replace the test server's default mux with a collector-specific
	// route. The dispatcher live E2E used the default mux; here we
	// want /api/collect, so build a parallel mux that *also* serves
	// the original hit-recorder so the parent server's cleanup still
	// owns its lifetime. We keep the parent server alive because the
	// vendor URL needs to be stable for the manifest.
	srv.Close()
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mu.Lock()
	*hits = nil // discard whatever default-mux handler captured before close
	mu.Unlock()

	// Build a manifest whose /api/collect endpoint is the mock vendor.
	manifest := `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: poll-live
spec:
  service:
    type: webhook
    endpoint: ` + srv.URL + `
    auth:
      keyRef: k1
  send:
    trigger: on_trace
    sampling: 1
  collect:
    mode: poll
    interval: 30ms
    mapping:
      name: name
      score: value
      explanation: comment
      trace_id: traceId
  timeout: 30s
`
	reg := evalplugin.NewRegistry()
	p, err := evalplugin.Decode([]byte(manifest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := evalplugin.Validate(p); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if discarded := reg.Merge([]evalplugin.Record{{
		Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin:  p,
		Enabled: true,
	}}); len(discarded) > 0 {
		t.Fatalf("merge discard: %d", len(discarded))
	}

	// Register a CollectFunc that hits the mock vendor over HTTP.
	// This is the lazy langfuse/llm-judge adapter shape: it takes
	// the plugin's target.Endpoint, GETs /api/collect, and returns
	// the JSON as []json.RawMessage.
	c := NewCollector(reg, nil, srv.Client())
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"k1"}}})
	c.SetLogger(silentLog())
	sink := &memSink{}
	c.sink = sink // wire AFTER construction so we can use the lib's NewCollector
	c.Register(evalplugin.ServiceWebhook, func(ctx context.Context, tgt Target) ([]json.RawMessage, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.Endpoint+"/api/collect", nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("mock vendor returned %d", resp.StatusCode)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		// The mock vendor returns an array; mirror the production
		// adapter shape that Returns []json.RawMessage one record
		// per element so applyAll sees the same input whether we
		// arrived via webhook or poll.
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("vendor returned non-array: %w", err)
		}
		return arr, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncNow(t, c, ctx)
	// Wait until the poller has fired at least once and the score
	// has reached the sink.
	deadline := time.Now().Add(2 * time.Second)
	var scores []evals.Score
	for time.Now().Before(deadline) {
		scores = sink.snapshot()
		if len(scores) > 0 && atomic.LoadInt32(serverHits) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(scores) != 1 {
		t.Fatalf("poller: expected 1 score via poll, got %d (serverHits=%d)", len(scores), atomic.LoadInt32(serverHits))
	}
	if scores[0].TraceID != "polled-1" {
		t.Errorf("TraceID: got %q want %q", scores[0].TraceID, "polled-1")
	}
	if scores[0].Score != 0.88 {
		t.Errorf("Score: got %v want 0.88", scores[0].Score)
	}
	if scores[0].Metric != "poll-quality" {
		t.Errorf("Metric: got %q want %q", scores[0].Metric, "poll-quality")
	}
}

// TestE2E_Collector_OTLPFanOutReachesBothSinks validates the OTLP
// mirror seam: a successful WriteScores at the durable sink MUST
// also be fanned out to the OTLPEvaluationLogSink SetEvaluationLogSink
// wired. The mirroring is what lets downstream OTel-aware
// collectors see exactly the scores Nexus persisted — a regression
// that drops the mirror but keeps WriteScores would be silent
// because ClickHouse/Postgres still hold the score; only the
// mirror would vanish.
//
// The live assertion is: one Score landed in both sinks, with
// matching TraceID and Evaluator fields.
func TestE2E_Collector_OTLPFanOutReachesBothSinks(t *testing.T) {
	reg := collectorLiveRegistry(t, "webhook", "60s")
	durable := &memSink{}
	otlp := &memOtlpSink{}
	c := NewCollector(reg, durable, nil)
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"k1"}}})
	c.SetLogger(silentLog())
	c.SetEvaluationLogSink(otlp)

	body := io.NopCloser(bytesNewReader([]byte(`{
		"name": "groundedness",
		"value": 0.71,
		"traceId": "mir-1"
	}`)))
	if err := c.Webhook("collector-live", body); err != nil {
		t.Fatalf("Webhook returned err: %v", err)
	}

	durScores := durable.snapshot()
	if len(durScores) != 1 {
		t.Fatalf("durable sink: got %d scores want 1", len(durScores))
	}
	if durScores[0].TraceID != "mir-1" {
		t.Errorf("durable.TraceID: got %q want %q", durScores[0].TraceID, "mir-1")
	}

	otlpEvts := otlp.snapshot()
	if len(otlpEvts) != 1 {
		t.Fatalf("otlp sink: got %d events want 1", len(otlpEvts))
	}
	if otlpEvts[0].TraceID != "mir-1" {
		t.Errorf("otlp.TraceID: got %q want %q", otlpEvts[0].TraceID, "mir-1")
	}
	if otlpEvts[0].Score.TraceID != "mir-1" {
		t.Errorf("otlp.Score.TraceID: got %q want %q", otlpEvts[0].Score.TraceID, "mir-1")
	}
	if otlpEvts[0].Score.Metric != "groundedness" {
		t.Errorf("otlp.Score.Metric: got %q want %q", otlpEvts[0].Score.Metric, "groundedness")
	}
	if otlpEvts[0].Score.Score != 0.71 {
		t.Errorf("otlp.Score.Score: got %v want 0.71", otlpEvts[0].Score.Score)
	}
}

// TestE2E_Collector_MappingRejectsBadPayload locks down the
// failure-mode contract on the inbound seam: a single-object
// payload whose JSON cannot be parsed MUST be rejected by Apply
// and dropped silently, leaving zero scores in the durable sink.
// The companion Single-Object recovery is then asserted to prove
// the Collector's state is not corrupted by the previous failure.
//
// The contract this test pins: "garbage in" produces zero scores
// out, never a zero-value Score with Metric:"". A silent passing
// of an empty Score would fan out to the OTLP mirror and pollute
// the gate's quality signal — and that regression would be
// invisible in production dashboards because the number of
// "scores" still goes up.
func TestE2E_Collector_MappingRejectsBadPayload(t *testing.T) {
	reg := collectorLiveRegistry(t, "webhook", "60s")
	sink := &memSink{}
	c := NewCollector(reg, sink, nil)
	c.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"k1"}}})
	c.SetLogger(silentLog())

	// Garbage payload — not valid JSON. Collector unmarshal at the
	// top level fails, falls back to single-object mode, Apply's
	// inner unmarshal fails, applyAll drops the element.
	bad := io.NopCloser(bytesNewReader([]byte(`{ this is not json`)))
	if err := c.Webhook("collector-live", bad); err != nil {
		t.Fatalf("Webhook should swallow decode errors (return nil per applyAll contract), got: %v", err)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("expected 0 scores from garbage payload, got %d (%+v)", len(got), got)
	}

	// Recovery — a well-formed single object lands normally.
	good := io.NopCloser(bytesNewReader([]byte(`{
		"name":"ok-c","value":0.7,"traceId":"recover-1"
	}`)))
	if err := c.Webhook("collector-live", good); err != nil {
		t.Fatalf("Webhook returned err after a previous failure: %v", err)
	}
	scores := sink.snapshot()
	if len(scores) != 1 {
		t.Fatalf("expected 1 score after recovery, got %d (%+v)", len(scores), scores)
	}
	if scores[0].TraceID != "recover-1" || scores[0].Metric != "ok-c" {
		t.Errorf("recovered score wrong: %+v", scores[0])
	}
}

// syncNow is a small helper used by the polling-path live test to
// bridge to the unexported syncPollers entry point. We can't call
// it from outside the package, but the file lives in the same
// `external` package so a thin wrapper keeps the production code
// surface untouched.
func syncNow(t *testing.T, c *Collector, ctx context.Context) {
	t.Helper()
	go func() { _ = c.Run(ctx) }()
	// Give Run one synchronization pass before we start polling.
	time.Sleep(5 * time.Millisecond)
	c.syncPollers(ctx)
}

// bytesNewReader is a tiny shim that wraps bytes.NewReader under a
// name that does not collide with the existing bytesReader type
// declared in the dispatcher test helper file.
func bytesNewReader(b []byte) io.Reader { return &bytesReaderShim{b: b} }

type bytesReaderShim struct {
	b []byte
	n int
}

func (r *bytesReaderShim) Read(p []byte) (int, error) {
	if r.n >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.n:])
	r.n += n
	return n, nil
}
