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
	// otlpExportFailures: reason → count. reason values:
	//   "http_4xx", "http_5xx", "network", "other".
	otlpExportFailures map[string]uint64
	// otlpExportSuccess count: traces successfully POSTed to OTLP.
	otlpExportSuccess uint64
	// otlpExportBytes: total payload bytes successfully POSTed.
	otlpExportBytes uint64
	// auditWriteFailures: action_category x reason → count.
	// action_category is a coarse classification of audit_log.action ("auth",
	// "user", "key", "credential", "routing", "eval", "benchmark", "integration",
	// "policy", "security", "audit", "other") so cardinality stays bounded;
	// reason is short ("missing_column", "timeout", "constraint", "other").
	// org_id / actor_id are deliberately NOT labels — c0.5 enforces that.
	auditWriteFailures map[labelsKey]uint64

	logger *slog.Logger
	srv    *http.Server
	addr   string
}

type labelsKey struct {
	L1, L2, L3, L4 string
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
		requestCount:       map[labelsKey]uint64{},
		latencyHist:        map[string]*latencyBuckets{},
		cacheHitCount:      map[string]uint64{},
		errorsTotal:        map[labelsKey]uint64{},
		failoverTotal:      map[labelsKey]uint64{},
		costTotal:          map[string]uint64{},
		qualityScoreSum:    map[string]float64{},
		qualityScoreCount:  map[string]uint64{},
		otlpExportFailures: map[string]uint64{},
		auditWriteFailures: map[labelsKey]uint64{},
		logger:             logger,
		addr:               addr,
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
	// Failover is signalled by ErrorType="upstream_error_failover" as set by
	// the gateway handler when a primary → secondary hop happened. We bucket
	// the counter by (from, to, reason, replica) so a single replica's flap
	// doesn't get lost in an aggregate; the dashboard `Failover events /
	// hour · by replica` panel queries `sum by (replica)`.
	if t.ErrorType == "upstream_error_failover" && t.ResponseModel != "" && t.RequestModel != "" {
		r.failoverTotal[labelsKey{
			L1: t.RequestModel,
			L2: t.ResponseModel,
			L3: t.ErrorType,
			L4: t.ReplicaID,
		}]++
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
//
// replicaID is the per-process id of the gateway instance surfacing the
// failover (mirrors Trace.ReplicaID / `NEXUS_REPLICA_ID`). Empty is allowed
// for single-replica deploys — Prometheus label will be the empty string,
// which is what most multi-replica operators filter on to find unhealthy
// pods.
func (r *MetricsRecorder) RecordFailover(fromModel, toModel, reason, replicaID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.failoverTotal[labelsKey{L1: fromModel, L2: toModel, L3: reason, L4: replicaID}]++
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

// RecordOTLPExportFailure increments the OTLP-failure counter for the given
// reason bucket. Reasons are callers' choice — the OTLPRecorder wires:
//
//   - "http_4xx"  : collector rejected the envelope (e.g. malformed)
//   - "http_5xx"  : collector/forwarder-side errors
//   - "network"   : dns/tcp/tls dial failures
//   - "other"     : catch-all (marshalling, context cancel)
//
// Surface is exposed as `nexus_otlp_export_failures_total{reason}` so an
// Alertmanager / Grafana rule can page the operator when the failure rate
// stays above zero for several minutes. We deliberately bucket by reason
// (not by HTTP status code alone) so non-HTTP failures — DNS, hard reset
// — stay observable separately from "collector says 400".
func (r *MetricsRecorder) RecordOTLPExportFailure(reason string) {
	if r == nil {
		return
	}
	if reason == "" {
		reason = "other"
	}
	r.mu.Lock()
	r.otlpExportFailures[reason]++
	r.mu.Unlock()
}

// RecordOTLPExportSuccess increments the success counter and adds the
// payload byte size. Mirrors RecordOTLPExportFailure so a Grafana panel
// can show success-vs-failure ratio over time. bytes is the size of the
// successfully POSTed envelope; 0 is fine (caller didn't measure).
func (r *MetricsRecorder) RecordOTLPExportSuccess(bytes int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.otlpExportSuccess++
	if bytes > 0 {
		r.otlpExportBytes += uint64(bytes)
	}
	r.mu.Unlock()
}

// AuditWriteFailed is called by the audit store when an INSERT into audit_log
// failed. action is the raw action constant from the audit row;
// auditActionCategory resolves it to a bounded category label and
// auditErrorReason bounds the error label, keeping cardinality low.
//
// The renderer in handleMetrics emits nexus_audit_write_failed_total{category,
// reason}. action itself is NOT used as a label because c0.5 limits us to
// reason + action_category, and unbounded action strings would re-create
// the tail-of-org_id cardinality bug Prometheus users got burned on.
func (r *MetricsRecorder) AuditWriteFailed(action string, err error) {
	if r == nil {
		return
	}
	category := auditActionCategory(action)
	reason := auditErrorReason(err)
	rk := labelsKey{L1: category, L2: reason}
	r.mu.Lock()
	r.auditWriteFailures[rk]++
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

	// nexus_router_failover_total{from, to, reason, replica}
	fmt.Fprintf(&b, "# HELP nexus_router_failover_total Failover events emitted by the quality-aware router.\n")
	fmt.Fprintf(&b, "# TYPE nexus_router_failover_total counter\n")
	fkeys := make([]labelsKey, 0, len(r.failoverTotal))
	for k := range r.failoverTotal {
		fkeys = append(fkeys, k)
	}
	sort.Slice(fkeys, func(i, j int) bool { return labelKeyCmp(fkeys[i], fkeys[j]) < 0 })
	for _, k := range fkeys {
		fmt.Fprintf(&b, "nexus_router_failover_total{from=%q,to=%q,reason=%q,replica=%q} %d\n",
			k.L1, k.L2, k.L3, k.L4, r.failoverTotal[k])
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

	// nexus_otlp_export_failures_total{reason}: counter — incremented by
	// OTLPRecorder.send() on any non-2xx response. Drives the
	// NexusOTLPExportsFailing alert (5m rate > 0 ⇒ page).
	fmt.Fprintf(&b, "# HELP nexus_otlp_export_failures_total OTLP exporter failures, bucketed by reason (http_4xx / http_5xx / network / other).\n")
	fmt.Fprintf(&b, "# TYPE nexus_otlp_export_failures_total counter\n")
	for _, reason := range sortedKeys(r.otlpExportFailures) {
		fmt.Fprintf(&b, "nexus_otlp_export_failures_total{reason=%q} %d\n",
			reason, r.otlpExportFailures[reason])
	}

	// nexus_otlp_export_traces_total: counter — successful envelope POSTs.
	// Success/failure ratio is what dashboards graph long-term; a sudden
	// drop to 0 alongside rising failures_total is the "OTLP went dark"
	// signature.
	fmt.Fprintf(&b, "# HELP nexus_otlp_export_traces_total OTLP exporter successful envelope POSTs.\n")
	fmt.Fprintf(&b, "# TYPE nexus_otlp_export_traces_total counter\n")
	fmt.Fprintf(&b, "nexus_otlp_export_traces_total %d\n", r.otlpExportSuccess)

	// nexus_otlp_export_bytes_total: counter — bytes successfully POSTed.
	// Useful when capacity-planning the collector (rate > N MB/s means
	// we're near the exporter back-pressure threshold).
	fmt.Fprintf(&b, "# HELP nexus_otlp_export_bytes_total OTLP exporter payload bytes successfully POSTed.\n")
	fmt.Fprintf(&b, "# TYPE nexus_otlp_export_bytes_total counter\n")
	fmt.Fprintf(&b, "nexus_otlp_export_bytes_total %d\n", r.otlpExportBytes)

	// nexus_audit_write_failed_total{category, reason}: counter — incremented
	// by Store.Audit on every audit INSERT failure. Category is enum-like
	// (closed set below); reason maps the Postgres / network error to a
	// short stable label.
	//
	// This series is the operational tripwire for c0.5: a non-zero rate here
	// means at least one audit row that should have been written wasn't.
	// Operator dashboards graph rate(action_category=...) over time so a
	// category-specific outage (e.g. only "key." actions failing) shows up
	// even when "audit" globally has not gone dark.
	fmt.Fprintf(&b, "# HELP nexus_audit_write_failed_total Audit INSERT failures by action category and reason.\n")
	fmt.Fprintf(&b, "# TYPE nexus_audit_write_failed_total counter\n")
	akeys := make([]labelsKey, 0, len(r.auditWriteFailures))
	for k := range r.auditWriteFailures {
		akeys = append(akeys, k)
	}
	sort.Slice(akeys, func(i, j int) bool { return labelKeyCmp(akeys[i], akeys[j]) < 0 })
	for _, k := range akeys {
		fmt.Fprintf(&b, "nexus_audit_write_failed_total{category=%q,reason=%q} %d\n",
			k.L1, k.L2, r.auditWriteFailures[k])
	}

	_, _ = w.Write([]byte(b.String()))
}

// auditActionCategory collapses every AuditAction constant into one of
// a small fixed set of categories. The list mirrors the action-category
// registry in core/audit.go and is the source of truth for the Prometheus
// label set.
//
// An action that does not match any prefix falls back to "other" rather
// than exploding the label cardinality. The renderer emits exactly these
// strings — adding a new category here without the inventory test
// (c0.8) would be a silent drift; the rule is "category list changes need
// a test update."
func auditActionCategory(action string) string {
	switch {
	case strings.HasPrefix(action, "auth."):
		return "auth"
	case strings.HasPrefix(action, "user."):
		return "user"
	case strings.HasPrefix(action, "invite."):
		return "invite"
	case strings.HasPrefix(action, "key."):
		return "key"
	case strings.HasPrefix(action, "credential."):
		return "credential"
	case strings.HasPrefix(action, "routing."):
		return "routing"
	case strings.HasPrefix(action, "eval."):
		return "eval"
	case strings.HasPrefix(action, "benchmark."):
		return "benchmark"
	case strings.HasPrefix(action, "integration."):
		return "integration"
	case strings.HasPrefix(action, "policy."):
		return "policy"
	case strings.HasPrefix(action, "audit."):
		return "audit"
	case strings.HasPrefix(action, "security."):
		return "security"
	case action == "":
		return "unknown"
	}
	return "other"
}

// auditErrorReason maps a Store.Audit INSERT failure error to a short
// stable label. The string-sniffing here is light: we look for the SQLSTATE
// or well-known pgx error class. Anything that doesn't match falls back to
// "other" so a malformed label value cannot land in the time-series.
func auditErrorReason(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "connection"):
		return "network"
	case strings.Contains(msg, "23505"): // unique_violation
		return "unique_violation"
	case strings.Contains(msg, "23503"): // foreign_key_violation
		return "fk_violation"
	case strings.Contains(msg, "column"):
		return "missing_column"
	case strings.Contains(msg, "syntax"):
		return "syntax"
	}
	return "other"
}

func labelKeyCmp(a, b labelsKey) int {
	if c := strings.Compare(a.L1, b.L1); c != 0 {
		return c
	}
	if c := strings.Compare(a.L2, b.L2); c != 0 {
		return c
	}
	if c := strings.Compare(a.L3, b.L3); c != 0 {
		return c
	}
	return strings.Compare(a.L4, b.L4)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
