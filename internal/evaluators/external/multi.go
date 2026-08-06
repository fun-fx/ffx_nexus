package external

import (
	"context"
	"errors"
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
//
// Send.Trigger branches the per-trace path:
//
//   - on_trace: parser: Dispatch is called inline.
//   - scheduled: parser: MultiEvaluator hands the trace to the
//     Scheduler, which buffers it and dispatches batches on the
//     plugin's Collect.Interval. Buffer overflow returns
//     ErrBufferFull, which the worker logs and continues past.
//   - manual: parser: traces are dropped at parse time; the plugin
//     fires only when an admin POSTs to the manual-fire admin REST
//     endpoint.
type MultiEvaluator struct {
	reg        *evalplugin.Registry
	dispatcher *Dispatcher
	local      LocalEvaluator
	log        *slog.Logger
	scheduler  *Scheduler

	mu         sync.RWMutex
	attachOnce sync.Once

	// warnOrgSkipOnce guards the "plugins exist but none are in scope"
	// warning so a permanent misconfiguration reports once per process
	// instead of once per trace.
	warnOrgSkipOnce sync.Once
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

// SetScheduler attaches the per-plugin buffer/flush handler used for
// scheduled and manual triggers. nil is allowed (the dispatcher
// falls back to on_trace semantics for every plugin) and is what
// tests expect until they specifically want to exercise the
// trigger branches.
func (m *MultiEvaluator) SetScheduler(s *Scheduler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduler = s
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
	// An org filter that excludes every plugin used to be indistinguishable
	// from "no plugins installed": the loop below simply did nothing. That
	// is the shape a scope mismatch takes, and it cost a long debugging
	// session, so say it out loud once.
	if len(enabled) == 0 && log != nil {
		if all := m.reg.Enabled(); len(all) > 0 {
			m.warnOrgSkipOnce.Do(func() {
				scopes := make([]string, 0, len(all))
				for _, rec := range all {
					scopes = append(scopes, rec.Plugin.Metadata.Name+"@"+orgLabel(rec.OrgID))
				}
				log.Warn("no eval plugins in scope for trace org: every enabled plugin is scoped to a different org",
					"trace_org", orgLabel(t.OrgID),
					"enabled_plugins", scopes)
			})
		}
	}
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
		if err := m.dispatchForwarded(ctx, t, rec, log); err != nil && log != nil {
			// Never fail the trace: eval dispatch is best-effort and
			// must not affect the gateway. But it has to be *visible* —
			// discarding this error is what made a plugin that rejected
			// every request look like a plugin with nothing to say.
			log.Warn("plugin dispatch failed",
				"plugin", rec.Plugin.Metadata.Name,
				"service_type", string(rec.Plugin.Spec.Service.Type),
				"trigger", rec.Plugin.Spec.Send.Trigger,
				"trace_id", t.TraceID,
				"err", err)
		}
	}
	return localScores, nil
}

// dispatchForwarded routes the (ctx, t, plugin) tuple to either
// the inline Dispatch call (on_trace) or the Scheduler
// (scheduled/manual). Returning nil-error means "queued"; the
// scheduler's own logger is responsible for surfacing its own
// failures. Returning errors here would force every trace through
// the parent worker log path, which would inflate the log volume
// for a transient backend; the in-scheduler logger is already
// wired for both.
func (m *MultiEvaluator) dispatchForwarded(ctx context.Context, t observability.Trace, rec evalplugin.Record, log *slog.Logger) error {
	m.mu.RLock()
	scheduler := m.scheduler
	m.mu.RUnlock()
	trigger := rec.Plugin.Spec.Send.Trigger
	switch trigger {
	case evalplugin.TriggerOnTrace:
		return m.dispatcher.Dispatch(ctx, t, rec.Plugin)
	case evalplugin.TriggerScheduled:
		if scheduler == nil {
			if log != nil {
				log.Warn("scheduled plugin dispatched as on_trace: no scheduler wired",
					"plugin", rec.Plugin.Metadata.Name)
			}
			return m.dispatcher.Dispatch(ctx, t, rec.Plugin)
		}
		if err := scheduler.Enqueue(rec.Plugin, t); err != nil {
			if errors.Is(err, ErrBufferFull) {
				if log != nil {
					log.Warn("scheduled plugin buffer full; trace dropped",
						"plugin", rec.Plugin.Metadata.Name,
						"trace_id", t.TraceID)
				}
				return nil
			}
			if errors.Is(err, ErrSchedulerStopped) {
				// Pod is draining. Losing a best-effort eval trace here is
				// expected; reporting it as an eval failure would paint
				// every shutdown with spurious errors.
				if log != nil {
					log.Warn("eval scheduler stopped; trace dropped",
						"plugin", rec.Plugin.Metadata.Name,
						"trace_id", t.TraceID)
				}
				return nil
			}
			return err
		}
		return nil
	case evalplugin.TriggerManual:
		// Manual plugins do not enqueue inline traces; the admin REST
		// endpoint drives them. Acknowledge silently so the worker
		// does not see an error here.
		return nil
	default:
		// Validate already rejected unknown values; this branch is
		// defensive only.
		return m.dispatcher.Dispatch(ctx, t, rec.Plugin)
	}
}

// orgLabel renders an org id for logs, naming the empty string rather
// than printing a blank that reads like a missing field.
func orgLabel(orgID string) string {
	if orgID == "" {
		return "(cluster-wide)"
	}
	return orgID
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
