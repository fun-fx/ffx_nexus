package external

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// LocalEvaluator is the per-trace scoring entry point for plugins
// that run in-process (ServiceHeuristic). Implementations live in
// internal/evaluators/heuristic and compute the score on the same
// worker goroutine, returning a single evals.Score. Returning nil
// scores with a nil error means "the metric was a no-op" (e.g. an
// unknown metric kind after a future version); callers then proceed
// to the next plugin as if nothing fired.
//
// MultiEvaluator holds an optional LocalEvaluator. Wiring it from
// main.go is the test seam: the heuristic package gets registered
// without main having to import every metric implementation.
type LocalEvaluator interface {
	Evaluate(ctx context.Context, metricName string, args map[string]any, t observability.Trace) ([]evals.Score, error)
}

// MultiEvaluator fans out one Dispatch call per enabled plugin in the
// registry. The Evaluator Name() returns "external" so dashboards
// can segment plugin-originated scores as a single kind, while the
// individual rows in eval_scores carry "plugin:<name>" in the
// Evaluator field.
//
// ServiceHeuristic plugins short-circuit Dispatch and go through
// the LocalEvaluator registered via SetLocalEvaluator; the score
// they return is fed forward alongside any synchronous plugin
// results so the worker can persist them in the same evals.Sink
// write batch as other sources.
type MultiEvaluator struct {
	reg        *evalplugin.Registry
	dispatcher *Dispatcher
	local      LocalEvaluator
	log        *slog.Logger

	mu         sync.RWMutex
	attachOnce sync.Once
}

// NewMultiEvaluator constructs a MultiEvaluator. The caller must
// ensure the registry has been populated (via LoadFromDir /
// LoadFromStore) before Evaluate() runs.
func NewMultiEvaluator(reg *evalplugin.Registry, dispatcher *Dispatcher) *MultiEvaluator {
	return &MultiEvaluator{reg: reg, dispatcher: dispatcher}
}

// SetLocalEvaluator attaches the in-process scorer for
// ServiceHeuristic plugins. nil means heuristic plugins fail with
// "no local evaluator wired" at dispatch time; the orchestrator
// never silently drops them.
func (m *MultiEvaluator) SetLocalEvaluator(le LocalEvaluator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.local = le
}

// SetLogger attaches a logger for dispatch failures. Strongly
// recommended: a plugin whose credentials or endpoint are wrong fails
// on every trace, and without a logger the only symptom is that the
// vendor dashboard stays empty.
func (m *MultiEvaluator) SetLogger(l *slog.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = l
}

// Name implements evals.Evaluator. UI surfaces report "external
// plugin(s): N" via the worker's ProfileStatus helper.
func (m *MultiEvaluator) Name() string { return "external" }

// Evaluate implements evals.Evaluator. It walks the registry's
// enabled set, samples each independently, and forwards via the
// Dispatcher. The evaluator never produces a Score row directly —
// plugins return results out-of-band (sync, poll, or webhook);
// either way the sink write happens in a Collector.
//
// Exception: ServiceHeuristic plugins run in-process via
// LocalEvaluator and we attach their *synchronous* score here so
// the worker persists it in the same batch as the rest of the
// trace's scores.
func (m *MultiEvaluator) Evaluate(ctx context.Context, t observability.Trace) ([]evals.Score, error) {
	// Scope to the trace's org: the registry is process-wide and holds
	// every tenant's plugins, so dispatching the unfiltered Enabled()
	// set would ship this org's prompts and completions to another
	// org's vendor account.
	enabled := m.reg.EnabledForOrg(t.OrgID)
	m.mu.RLock()
	log := m.log
	local := m.local
	m.mu.RUnlock()
	var localScores []evals.Score
	for _, rec := range enabled {
		if !sampleTrace(float64(rec.Plugin.Spec.Send.Sampling)) {
			continue
		}
		// ServiceHeuristic: route through LocalEvaluator, capture
		// the synchronous score.
		if rec.Plugin.Spec.Service.Type == evalplugin.ServiceHeuristic {
			if local == nil {
				if log != nil {
					log.Warn("heuristic plugin skipped: LocalEvaluator not wired",
						"plugin", rec.Plugin.Metadata.Name,
						"trace_id", t.TraceID)
				}
				continue
			}
			scores, err := local.Evaluate(ctx,
				rec.Plugin.Spec.Service.Metric.Name,
				rec.Plugin.Spec.Service.Metric.Args,
				t)
			if err != nil {
				if log != nil {
					log.Warn("heuristic plugin failed",
						"plugin", rec.Plugin.Metadata.Name,
						"metric", rec.Plugin.Spec.Service.Metric.Name,
						"trace_id", t.TraceID,
						"err", err)
				}
				continue
			}
			localScores = append(localScores, scores...)
			continue
		}
		if err := m.dispatcher.Dispatch(ctx, t, rec.Plugin); err != nil && log != nil {
			// Never fail the trace: eval dispatch is best-effort and
			// must not affect the gateway. But it has to be *visible* —
			// discarding this error is what made a plugin that rejected
			// every request look like a plugin with nothing to say.
			log.Warn("plugin dispatch failed",
				"plugin", rec.Plugin.Metadata.Name,
				"service_type", string(rec.Plugin.Spec.Service.Type),
				"trace_id", t.TraceID,
				"err", err)
		}
	}
	return localScores, nil
}

// sampleTrace is the per-plugin sample gate. It rolls once per plugin
// so each enabled plugin samples independently: two plugins at
// sampling=0.1 each see ~10% of traces rather than sharing one roll.
//
// This used to return true for any fraction above zero, deferring to a
// gate in worker.collectEvaluators that never applied — the plugin
// evaluator is appended outside that function. The effect was that
// `sampling: 0.1` forwarded 100% of traces, which is a vendor bill and
// a rate limit rather than a cosmetic bug.
//
// math/rand/v2's top-level source is safe for concurrent use, which
// matters because Evaluate runs on every eval worker goroutine.
func sampleTrace(p float64) bool {
	if p >= 1 {
		return true
	}
	if p <= 0 {
		return false
	}
	return rand.Float64() < p
}
