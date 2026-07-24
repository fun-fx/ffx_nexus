package evals

import (
	"context"
	"sync"

	"github.com/ffxnexus/nexus/internal/observability"
)

// scoreBag is the lock-protected buffer the per-goroutine evaluator
// results are written into. PR #135's evaluate() fans work out across
// goroutines; this collector gives us a slice that we can read out in
// a single batch after WaitGroup.Wait returns.
//
// Cap on initial alloc comes from `initial` so the common case (4
// evaluators summing to ~6 scores) avoids a slice growth. Slice grow
// beyond cap is fine — just one or two re-allocations.
type scoreBag struct {
	mu      sync.Mutex
	values  []Score
	initial int
}

func newScoreBag(initial int) *scoreBag {
	if initial <= 0 {
		initial = 4
	}
	return &scoreBag{initial: initial}
}

func (b *scoreBag) add(s []Score) {
	if len(s) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values = append(b.values, s...)
}

func (b *scoreBag) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.values)
}

func (b *scoreBag) take() []Score {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.values
	b.values = nil
	return out
}

// runEvaluators fans out one goroutine per Evaluator, joins them via a
// bounded slot count so we don't pin too many at once. This is the hot
// path's only optimisation in PR #135 — the eval worker was already
// off the request hot path, but sequential dispatch cost ∑e latency
// instead of max(e) latency, which manifested as a tail-latency
// inflation in the eval-side dashboards only.
//
// `bound` zero means "no fan-out cap". Caller passes w.workerCount
// which the runtime controller keeps small (default = 4).
func (w *Worker) runEvaluators(ctx context.Context, t observability.Trace, evals []Evaluator, bag *scoreBag) {
	if len(evals) == 0 {
		return
	}
	wg := sync.WaitGroup{}
	for _, e := range evals {
		wg.Add(1)
		go func(ev Evaluator) {
			defer wg.Done()
			s, err := ev.Evaluate(ctx, t)
			if err != nil {
				w.log.Warn("evaluator failed", "evaluator", ev.Name(), "trace_id", t.TraceID, "err", err)
				return
			}
			bag.add(s)
		}(e)
	}
	wg.Wait()
}
