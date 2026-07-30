package evals

import (
	"context"
	"sort"
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
// An evaluator that never returns used to cost a worker permanently:
// wg.Wait blocked forever, the trace produced no scores and no error,
// and with the pool exhausted every later trace queued behind it in
// silence. The wait is now bounded by ctx, so a stuck evaluator costs
// one leaked goroutine and one warning that names it, instead of the
// whole eval pipeline going dark until the next restart.
func (w *Worker) runEvaluators(ctx context.Context, t observability.Trace, evals []Evaluator, bag *scoreBag) {
	if len(evals) == 0 {
		return
	}
	outstanding := newPendingSet(evals)
	wg := sync.WaitGroup{}
	for _, e := range evals {
		wg.Add(1)
		go func(ev Evaluator) {
			defer wg.Done()
			defer outstanding.done(ev.Name())
			s, err := ev.Evaluate(ctx, t)
			if err != nil {
				w.log.Warn("evaluator failed", "evaluator", ev.Name(), "trace_id", t.TraceID, "err", err)
				return
			}
			bag.add(s)
		}(e)
	}

	joined := make(chan struct{})
	go func() {
		wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-ctx.Done():
		w.log.Warn("evaluators did not return before the deadline; continuing without their scores",
			"evaluators", outstanding.names(), "trace_id", t.TraceID, "err", ctx.Err())
	}
}

// pendingSet tracks which evaluators have not finished yet so a
// deadline warning can name them. Without the names the operator sees
// a timeout and still has to guess which backend hung.
type pendingSet struct {
	mu      sync.Mutex
	pending map[string]int
}

func newPendingSet(evals []Evaluator) *pendingSet {
	p := &pendingSet{pending: make(map[string]int, len(evals))}
	for _, e := range evals {
		p.pending[e.Name()]++
	}
	return p
}

func (p *pendingSet) done(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending[name] <= 1 {
		delete(p.pending, name)
		return
	}
	p.pending[name]--
}

// names returns the still-running evaluators in a stable order so the
// warning reads the same way across occurrences.
func (p *pendingSet) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.pending))
	for name := range p.pending {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
