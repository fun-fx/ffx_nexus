package evals

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// stubEvaluator returns a fixed score after an optional delay, or
// blocks until released — the shape of an evaluator whose backend
// hangs.
type stubEvaluator struct {
	name  string
	block chan struct{}
	score float64
}

func (s stubEvaluator) Name() string { return s.name }

func (s stubEvaluator) Evaluate(_ context.Context, _ observability.Trace) ([]Score, error) {
	if s.block != nil {
		<-s.block
	}
	return []Score{{Evaluator: s.name, Metric: "stub", Score: s.score}}, nil
}

func testWorker(out *syncWriter) *Worker {
	return NewWorker(Options{
		Sink:       &captureSink{},
		Workers:    1,
		BufferSize: 4,
	}, slog.New(slog.NewTextHandler(out, nil)))
}

// TestRunEvaluatorsCollectsEveryScore is the baseline: nothing hangs,
// so every evaluator contributes and no warning is emitted.
func TestRunEvaluatorsCollectsEveryScore(t *testing.T) {
	out := &syncWriter{}
	w := testWorker(out)
	bag := newScoreBag(2)

	w.runEvaluators(context.Background(), observability.Trace{TraceID: "t1"},
		[]Evaluator{
			stubEvaluator{name: "fast-a", score: 1},
			stubEvaluator{name: "fast-b", score: 0.5},
		}, bag)

	if got := len(bag.take()); got != 2 {
		t.Fatalf("collected %d scores, want 2", got)
	}
	if strings.Contains(out.String(), "did not return") {
		t.Fatalf("healthy evaluators must not warn: %s", out.String())
	}
}

// TestRunEvaluatorsReturnsWhenOneEvaluatorHangs is the fix for the
// failure mode that took the pipeline down: a single evaluator stuck in
// a non-cancellable loop held wg.Wait forever, so the worker never
// picked up another trace and never said why. The join must now end
// with the deadline and name the offender.
func TestRunEvaluatorsReturnsWhenOneEvaluatorHangs(t *testing.T) {
	out := &syncWriter{}
	w := testWorker(out)
	bag := newScoreBag(2)

	release := make(chan struct{})
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	returned := make(chan struct{})
	go func() {
		w.runEvaluators(ctx, observability.Trace{TraceID: "t2"}, []Evaluator{
			stubEvaluator{name: "healthy", score: 1},
			stubEvaluator{name: "wedged", block: release},
		}, bag)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("runEvaluators never returned; a stuck evaluator still owns the worker")
	}

	// The healthy evaluator's score is still usable.
	if got := len(bag.take()); got != 1 {
		t.Fatalf("collected %d scores, want the healthy evaluator's 1", got)
	}
	logged := out.String()
	if !strings.Contains(logged, "did not return before the deadline") {
		t.Fatalf("the timeout must be reported: %s", logged)
	}
	if !strings.Contains(logged, "wedged") {
		t.Fatalf("the warning must name the stuck evaluator: %s", logged)
	}
	if strings.Contains(logged, "healthy") {
		t.Fatalf("an evaluator that finished must not be blamed: %s", logged)
	}
}

// TestRunEvaluatorsIgnoresEmptyList keeps the no-profile, no-plugin
// deployment off the goroutine path entirely.
func TestRunEvaluatorsIgnoresEmptyList(t *testing.T) {
	out := &syncWriter{}
	w := testWorker(out)
	bag := newScoreBag(1)
	w.runEvaluators(context.Background(), observability.Trace{}, nil, bag)
	if bag.len() != 0 || out.String() != "" {
		t.Fatal("an empty evaluator list should do nothing at all")
	}
}

// TestPendingSetTracksDuplicateNames: two profiles can share a name
// (same kind, different scope), so the set has to count rather than
// hold a single flag per name.
func TestPendingSetTracksDuplicateNames(t *testing.T) {
	p := newPendingSet([]Evaluator{
		stubEvaluator{name: "judge"},
		stubEvaluator{name: "judge"},
		stubEvaluator{name: "pii"},
	})
	p.done("judge")
	if got := p.names(); len(got) != 2 || got[0] != "judge" || got[1] != "pii" {
		t.Fatalf("one of two judges finished, want [judge pii], got %v", got)
	}
	p.done("judge")
	p.done("pii")
	if got := p.names(); len(got) != 0 {
		t.Fatalf("everything finished, want empty, got %v", got)
	}
}

// syncWriter lets the assertions read a log that a background
// goroutine may still be writing to.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
