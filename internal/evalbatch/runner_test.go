package evalbatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// fakeEvaluator scores each trace and records what it saw, so the runner's
// trace construction and concurrency can be verified without a network call.
// It is concurrency-safe, as required by the Evaluator contract.
type fakeEvaluator struct {
	calls   int64
	failOn  string // case TraceID to fail
	mu      sync.Mutex
	seenIn  map[string]string
	seenOut map[string]string
	seenCtx map[string]string
}

func (f *fakeEvaluator) Name() string { return "fake" }

func (f *fakeEvaluator) Evaluate(_ context.Context, t observability.Trace) ([]evals.Score, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.seenIn != nil {
		f.mu.Lock()
		f.seenIn[t.TraceID] = t.InputMessages
		f.seenOut[t.TraceID] = t.OutputMessages
		f.seenCtx[t.TraceID] = t.RetrievalContexts
		f.mu.Unlock()
	}
	if t.TraceID == f.failOn {
		return nil, errors.New("boom")
	}
	return []evals.Score{
		{TraceID: t.TraceID, Metric: "answer_relevancy", Score: 0.8, Passed: true},
		{TraceID: t.TraceID, Metric: "toxicity", Score: 0.1, Passed: true},
	}, nil
}

func TestRunnerEvaluatesAllCases(t *testing.T) {
	fe := &fakeEvaluator{
		seenIn:  map[string]string{},
		seenOut: map[string]string{},
		seenCtx: map[string]string{},
	}
	cases := []Case{
		{ID: "a", Model: "m", Input: "q1", Output: "o1"},
		{ID: "b", Model: "m", Input: "q2", Output: "o2", Contexts: []string{"ctx"}},
	}
	r := &Runner{Eval: fe, Concurrency: 2, Timeout: time.Second}
	results := r.Run(context.Background(), cases)

	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].ID != "a" || results[1].ID != "b" {
		t.Fatalf("results out of order: %+v", results)
	}
	if atomic.LoadInt64(&fe.calls) != 2 {
		t.Fatalf("want 2 evaluator calls, got %d", fe.calls)
	}
	// Input messages are marshaled as a JSON chat array.
	var msgs []Message
	if err := json.Unmarshal([]byte(fe.seenIn["a"]), &msgs); err != nil {
		t.Fatalf("input not JSON messages: %v (%q)", err, fe.seenIn["a"])
	}
	if len(msgs) != 1 || msgs[0].Content != "q1" {
		t.Fatalf("unexpected input messages: %+v", msgs)
	}
	if fe.seenOut["a"] != "o1" {
		t.Fatalf("output not propagated: %q", fe.seenOut["a"])
	}
	if fe.seenCtx["b"] != `["ctx"]` {
		t.Fatalf("contexts not propagated: %q", fe.seenCtx["b"])
	}
}

func TestRunnerCapturesErrors(t *testing.T) {
	fe := &fakeEvaluator{failOn: "bad"}
	cases := []Case{
		{ID: "ok", Input: "q", Output: "o"},
		{ID: "bad", Input: "q", Output: "o"},
	}
	r := &Runner{Eval: fe, Concurrency: 1}
	results := r.Run(context.Background(), cases)

	if results[0].Error != "" {
		t.Errorf("first case should succeed, got error %q", results[0].Error)
	}
	if results[1].Error == "" {
		t.Errorf("second case should report an error")
	}
	if len(results[1].Scores) != 0 {
		t.Errorf("errored case should have no scores")
	}
}
