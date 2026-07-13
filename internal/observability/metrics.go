// Package observability · Prometheus exposition adapter.
//
// MetricsRecorder is a Recorder implementation that fans out per-request
// counters/histograms in the Prometheus text exposition format to an HTTP
// scrape endpoint. It is deliberately stdlib-only: pulling in
// prometheus/client_golang would add a transitive dependency on
// google.golang.org/protobuf, which we keep out of the core gateway to
// honor the zero-dep fast path.
//
// The exporter is opt-in — flips on when METRICS_ADDR is set, otherwise the
// surface stays empty so binary size and cold start are unchanged for the
// common case.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// MetricsRecorder collects per-Trace counters in memory and exposes them as
// Prometheus text on the configured HTTP endpoint. Safe for concurrent use;
// all updates go through mu. Reads (served on the scrape goroutine) acquire
// mu briefly to snapshot the maps then format without holding the lock.
type MetricsRecorder struct {
	mu sync.RWMutex

	// requestCount: model x status x credential_source → count
	requestCount map[labelsKey]uint64
	// latencyMsHist: simple per-model exponential buckets for p50/p95/p99 via
	// histogram_quantile. Buckets: 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000 ms.
	latencyHist map[string]*latencyBuckets
	// cacheHitCount: (model, scope) → count
	cacheHitCount map[string]uint64
	// errorsTotal: provider x reason → count
	errorsTotal map[labelsKey]uint64
	// failoverTotal: from x to → count
	failoverTotal map[labelsKey]uint64
	// costTotal: model → usd micros (to keep integer arithmetic)
	costTotal map[string]uint64
	// qualityScoreSum / qualityScoreCount: model → sum, count (for avg)
	qualityScoreSum   map[string]float64
	qualityScoreCount map[string]uint64

	logger *slog.Logger
	srv    *http.Server
	addr   string
}

type labelsKey struct {
	L1, L2, L3 string
}

type latencyBuckets struct {
	// cumulative buckets named like LatencyBucket{le="10"} → count ≤ 10ms.
	buckets map[string]uint64
	count   uint64
	sumMs   float64
}

// NewMetricsRecorder starts the scrape server on addr. If addr is empty, the
// call returns nil. The caller should add MetricsRecorder into the existing
// observability.MultiRecorder (see internal/observability/multi.go).
func NewMetricsRecorder(addr string, logger *slog.Logger) *MetricsRecorder {
	if addr == "" {
		return nil
	}
	r := &MetricsRecorder{
		requestCount:      map[labelsKey]uint64{},
		latencyHist:       map[string]*latencyBuckets{},
		cacheHitCount:     map[string]uint64{},
		errorsTotal:       map[labelsKey]uint64{},
		failoverTotal:     map[labelsKey]uint64{},
		costTotal:         map[string]uint64{},
		qualityScoreSum:   map[string]float64{},
		qualityScoreCount: map[string]uint64{},
		logger:            logger,
		addr:              addr,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", r.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := r.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Warn("metrics server stopped", "err", err)
		}
	}()
	logger.Info("prometheus metrics endpoint listening", "addr", addr)
	return r
}

// Record implements Recorder. Trace fields are mapped into counters and the
// latency histogram; cache hits and failovers are tracked as their own series.
func (r *MetricsRecorder) Record(t Trace) {
	if r == nil {
		return
	}

	rk := labelsKey{L1: t.RequestModel, L2: statusBucket(t.StatusCode), L3: t.CredentialSource}
	r.mu.Lock()
	r.requestCount[rk]++
	if t.CacheHit {
		r.cacheHitCount[t.RequestModel]++
	}
	if t.LatencyMs > 0 {
		b, ok := r.latencyHist[t.RequestModel]
		if !ok {
			b = &latencyBuckets{buckets: map[string]uint64{}}
			r.latencyHist[t.RequestModel] = b
		}
		latencyMs := float64(t.LatencyMs)
		b.count++
		b.sumMs += latencyMs
		for _, le := range latencyBucketBounds {
			if latencyMs <= le {
				bucketsKey := fmt.Sprintf("%g", le)
				b.buckets[bucketsKey]++
			}
		}
	}
	if t.StatusCode >= 400 {
		r.errorsTotal[labelsKey{L1: t.ProviderName, L2: errorBucket(t.ErrorType)}]++
	}
	if t.CostUSD > 0 {
		r.costTotal[t.RequestModel] += uint64(t.CostUSD * 1_000_000)
	}
	r.mu.Unlock()
}

// RecordFailover is called by the quality-aware router when a candidate is
// skipped because of an upstream error. The MultiRecorder dispatches to it
// when the trace carries an off-hotpath flag; here we accept a direct update
// for simplicity.
func (r *MetricsRecorder) RecordFailover(fromModel, toModel, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.failoverTotal[labelsKey{L1: fromModel, L2: toModel, L3: reason}]++
	r.mu.Unlock()
}

// RecordQualityScore is invoked by the eval worker. Pass metric="quality"
// (or whatever the eval scored) and a 0..1 value.
func (r *MetricsRecorder) RecordQualityScore(model string, score float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.qualityScoreSum[model] += score
	r.qualityScoreCount[model]++
	r.mu.Unlock()
}

// Close implements Recorder.
func (r *MetricsRecorder) Close(ctx context.Context) error {
	if r == nil || r.srv == nil {
		return nil
	}
	return r.srv.Shutdown(ctx)
}

var latencyBucketBounds = []float64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}
var latencyBucketLabels = func() []string {
	out := make([]string, len(latencyBucketBounds))
	for i, b := range latencyBucketBounds {
		out[i] = fmt.Sprintf("%g", b)
	}
	return out
}()

func statusBucket(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "other"
	}
}

func errorBucket(t string) string {
	if t == "" {
		return "unknown"
	}
	return t
}

func (r *MetricsRecorder) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder

	// nexus_gateway_requests_total{model, status, credential_source}
	fmt.Fprintf(&b, "# HELP nexus_gateway_requests_total Total gateway requests by model, status, and credential source.\n")
	fmt.Fprintf(&b, "# TYPE nexus_gateway_requests_total counter\n")
	keys := make([]labelsKey, 0, len(r.requestCount))
	for k := range r.requestCount {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return labelKeyCmp(keys[i], keys[j]) < 0 })
	for _, k := range keys {
		fmt.Fprintf(&b, "nexus_gateway_requests_total{gen_ai_request_model=%q,status=%q,credential_source=%q} %d\n",
			k.L1, k.L2, k.L3, r.requestCount[k])
	}

	// nexus_gateway_request_duration_seconds_bucket{le, model}
	fmt.Fprintf(&b, "# HELP nexus_gateway_request_duration_seconds Request latency per model (exposition-format histogram).\n")
	fmt.Fprintf(&b, "# TYPE nexus_gateway_request_duration_seconds histogram\n")
	models := sortedKeys(r.latencyHist)
	for _, m := range models {
		hb := r.latencyHist[m]
		for _, lbl := range latencyBucketLabels {
			fmt.Fprintf(&b, "nexus_gateway_request_duration_seconds_bucket{gen_ai_request_model=%q,le=%q} %d\n",
				m, lbl, hb.buckets[lbl])
		}
		fmt.Fprintf(&b, "nexus_gateway_request_duration_seconds_bucket{gen_ai_request_model=%q,le=\"+Inf\"} %d\n",
			m, hb.count)
		fmt.Fprintf(&b, "nexus_gateway_request_duration_seconds_count{gen_ai_request_model=%q} %d\n",
			m, hb.count)
		fmt.Fprintf(&b, "nexus_gateway_request_duration_seconds_sum{gen_ai_request_model=%q} %f\n",
			m, hb.sumMs/1000.0)
	}

	// nexus_gateway_cache_hits_total{model}
	fmt.Fprintf(&b, "# HELP nexus_gateway_cache_hits_total Semantic cache hits per model.\n")
	fmt.Fprintf(&b, "# TYPE nexus_gateway_cache_hits_total counter\n")
	for _, m := range sortedKeys(r.cacheHitCount) {
		fmt.Fprintf(&b, "nexus_gateway_cache_hits_total{gen_ai_request_model=%q} %d\n", m, r.cacheHitCount[m])
	}

	// nexus_gateway_errors_total{provider, reason}
	fmt.Fprintf(&b, "# HELP nexus_gateway_errors_total Error responses by provider and reason.\n")
	fmt.Fprintf(&b, "# TYPE nexus_gateway_errors_total counter\n")
	ekeys := make([]labelsKey, 0, len(r.errorsTotal))
	for k := range r.errorsTotal {
		ekeys = append(ekeys, k)
	}
	sort.Slice(ekeys, func(i, j int) bool { return labelKeyCmp(ekeys[i], ekeys[j]) < 0 })
	for _, k := range ekeys {
		fmt.Fprintf(&b, "nexus_gateway_errors_total{provider=%q,reason=%q} %d\n",
			k.L1, k.L2, r.errorsTotal[k])
	}

	// nexus_router_failover_total{from, to, reason}
	fmt.Fprintf(&b, "# HELP nexus_router_failover_total Failover events emitted by the quality-aware router.\n")
	fmt.Fprintf(&b, "# TYPE nexus_router_failover_total counter\n")
	fkeys := make([]labelsKey, 0, len(r.failoverTotal))
	for k := range r.failoverTotal {
		fkeys = append(fkeys, k)
	}
	sort.Slice(fkeys, func(i, j int) bool { return labelKeyCmp(fkeys[i], fkeys[j]) < 0 })
	for _, k := range fkeys {
		fmt.Fprintf(&b, "nexus_router_failover_total{from=%q,to=%q,reason=%q} %d\n",
			k.L1, k.L2, k.L3, r.failoverTotal[k])
	}

	// nexus_gateway_cost_usd_total{model}
	fmt.Fprintf(&b, "# HELP nexus_gateway_cost_usd_total Total cost (USD) per model since startup.\n")
	fmt.Fprintf(&b, "# TYPE nexus_gateway_cost_usd_total counter\n")
	for _, m := range sortedKeys(r.costTotal) {
		fmt.Fprintf(&b, "nexus_gateway_cost_usd_total{gen_ai_request_model=%q} %f\n",
			m, float64(r.costTotal[m])/1_000_000)
	}

	// nexus_eval_quality_score{model}
	fmt.Fprintf(&b, "# HELP nexus_eval_quality_score Rolling mean quality judge score per model.\n")
	fmt.Fprintf(&b, "# TYPE nexus_eval_quality_score gauge\n")
	for _, m := range sortedKeys(r.qualityScoreCount) {
		if r.qualityScoreCount[m] == 0 {
			continue
		}
		fmt.Fprintf(&b, "nexus_eval_quality_score{gen_ai_request_model=%q} %f\n",
			m, r.qualityScoreSum[m]/float64(r.qualityScoreCount[m]))
	}

	_, _ = w.Write([]byte(b.String()))
}

func labelKeyCmp(a, b labelsKey) int {
	if c := strings.Compare(a.L1, b.L1); c != 0 {
		return c
	}
	if c := strings.Compare(a.L2, b.L2); c != 0 {
		return c
	}
	return strings.Compare(a.L3, b.L3)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
