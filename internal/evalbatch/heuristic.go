package evalbatch

import (
	"context"

	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// HeuristicEvaluator runs the deterministic, LLM-free heuristic evaluators
// (PII leakage + completeness) over a case. It exists so a regression batch can
// run hermetically — no Python eval service, no judge, no provider API key — and
// still produce stable, reproducible scores suitable for a CI quality gate.
type HeuristicEvaluator struct {
	evaluators []evals.Evaluator
}

// NewHeuristicEvaluator returns an evaluator that combines the built-in
// heuristics used by the online worker.
func NewHeuristicEvaluator() *HeuristicEvaluator {
	return &HeuristicEvaluator{
		evaluators: []evals.Evaluator{
			evals.PIIEvaluator{},
			evals.CompletenessEvaluator{},
		},
	}
}

// Name implements evals.Evaluator.
func (h *HeuristicEvaluator) Name() string { return "heuristic" }

// Evaluate implements evals.Evaluator by running each heuristic and
// concatenating their scores. Heuristics are pure functions of the trace, so the
// result is deterministic.
func (h *HeuristicEvaluator) Evaluate(ctx context.Context, t observability.Trace) ([]evals.Score, error) {
	out := make([]evals.Score, 0, len(h.evaluators))
	for _, e := range h.evaluators {
		s, err := e.Evaluate(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	return out, nil
}
