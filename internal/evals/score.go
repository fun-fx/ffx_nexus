// Package evals runs asynchronous, out-of-band quality evaluations on gateway
// traces. It never sits on the request hot path: the gateway hands completed
// traces to a Worker (via the observability.Recorder interface), and evaluation
// runs on background goroutines. Results land in ClickHouse (eval_scores) and
// feed quality-aware routing (Phase 4).
package evals

import (
	"context"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// Score is a single evaluation result for one trace, mirroring the eval_scores
// ClickHouse table.
type Score struct {
	TraceID    string
	Timestamp  time.Time
	Evaluator  string  // e.g. "heuristic_pii", "slm_judge"
	Metric     string  // e.g. "pii_leak", "completeness", "quality"
	Score      float64 // normalized 0..1 (higher is better)
	Passed     bool
	Rationale  string
	JudgeModel string // model used for LLM-as-judge, empty for heuristics
}

// Evaluator scores a single trace. Implementations must be safe for concurrent
// use and must respect ctx cancellation/timeout.
type Evaluator interface {
	// Name identifies the evaluator (stored as eval_scores.evaluator).
	Name() string
	// Evaluate returns zero or more scores for the trace.
	Evaluate(ctx context.Context, t observability.Trace) ([]Score, error)
}

// Sink persists evaluation scores. Implementations should batch/flush
// internally and must be safe for concurrent use.
type Sink interface {
	WriteScores(ctx context.Context, scores []Score) error
}

// NoopSink discards scores. Used when ClickHouse is not configured.
type NoopSink struct{}

// WriteScores implements Sink.
func (NoopSink) WriteScores(context.Context, []Score) error { return nil }
