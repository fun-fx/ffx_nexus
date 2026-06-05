package evalbatch

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// CaseResult is the evaluation outcome for one case.
type CaseResult struct {
	ID     string        `json:"id"`
	Model  string        `json:"model,omitempty"`
	Scores []evals.Score `json:"scores,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// Runner evaluates a dataset of cases against an evals.Evaluator. It bypasses
// the online Worker's sampling so every case is scored.
type Runner struct {
	Eval        evals.Evaluator
	Concurrency int           // worker goroutines; <=0 means 1
	Timeout     time.Duration // per-case evaluation timeout; 0 disables
}

// Run evaluates all cases concurrently and returns results in dataset order.
func (r *Runner) Run(ctx context.Context, cases []Case) []CaseResult {
	conc := r.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > len(cases) {
		conc = len(cases)
	}

	results := make([]CaseResult, len(cases))
	jobs := make(chan int)
	var wg sync.WaitGroup

	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = r.evalCase(ctx, cases[i])
			}
		}()
	}
	for i := range cases {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func (r *Runner) evalCase(ctx context.Context, c Case) CaseResult {
	res := CaseResult{ID: c.ID, Model: c.Model}

	cctx := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	scores, err := r.Eval.Evaluate(cctx, c.trace())
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Scores = scores
	return res
}

// trace builds the observability.Trace the Evaluator consumes from a Case.
func (c Case) trace() observability.Trace {
	t := observability.Trace{
		TraceID:        c.ID,
		Timestamp:      time.Now().UTC(),
		StatusCode:     200,
		RequestModel:   c.Model,
		OutputMessages: c.Output,
		EvalReference:  c.Reference,
	}
	if msgs := c.ChatMessages(); len(msgs) > 0 {
		if b, err := json.Marshal(msgs); err == nil {
			t.InputMessages = string(b)
		}
	}
	if len(c.Contexts) > 0 {
		if b, err := json.Marshal(c.Contexts); err == nil {
			t.RetrievalContexts = string(b)
		}
	}
	return t
}
