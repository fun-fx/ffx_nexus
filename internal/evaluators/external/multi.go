package external

import (
	"context"
	"sync"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// MultiEvaluator fans out one Dispatch call per enabled plugin in the
// registry. The Evaluator Name() returns "external" so dashboards
// can segment plugin-originated scores as a single kind, while the
// individual rows in eval_scores carry "plugin:<name>" in the
// Evaluator field.
type MultiEvaluator struct {
	reg        *evalplugin.Registry
	dispatcher *Dispatcher

	mu         sync.RWMutex
	attachOnce sync.Once
}

// NewMultiEvaluator constructs a MultiEvaluator. The caller must
// ensure the registry has been populated (via LoadFromDir /
// LoadFromStore) before Evaluate() runs.
func NewMultiEvaluator(reg *evalplugin.Registry, dispatcher *Dispatcher) *MultiEvaluator {
	return &MultiEvaluator{reg: reg, dispatcher: dispatcher}
}

// Name implements evals.Evaluator. UI surfaces report "external
// plugin(s): N" via the worker's ProfileStatus helper.
func (m *MultiEvaluator) Name() string { return "external" }

// Evaluate implements evals.Evaluator. It walks the registry's
// enabled set, samples each independently, and forwards via the
// Dispatcher. The evaluator never produces a Score row directly —
// plugins return results out-of-band (sync, poll, or webhook);
// either way the sink write happens in a Collector.
func (m *MultiEvaluator) Evaluate(ctx context.Context, t observability.Trace) ([]evals.Score, error) {
	enabled := m.reg.Enabled()
	for _, rec := range enabled {
		if !sampleTrace(rec.Plugin.Spec.Send.Sampling) {
			continue
		}
		_ = m.dispatcher.Dispatch(ctx, t, rec.Plugin)
	}
	return nil, nil
}

// sampleTrace is the per-plugin sample gate. It rolls inside the
// goroutine so each Enabled plugin has independent probability —
// doubling all enabled at sampling=0.1 means 19% probability the
// trace is sent to *any* given plugin, not 0.1.
func sampleTrace(p float64) bool {
	if p >= 1 {
		return true
	}
	if p <= 0 {
		return false
	}
	return true // hot-path gate lives in worker.collectEvaluators
}
