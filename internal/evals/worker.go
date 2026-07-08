package evals

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// Worker consumes completed traces and runs evaluators on background
// goroutines. It implements observability.Recorder so it can be attached to the
// gateway's recorder fan-out; Record() is a non-blocking enqueue that never
// adds latency to the request path.
type Worker struct {
	mu sync.RWMutex

	piiEnabled          bool
	completenessEnabled bool
	judges              []Evaluator // LLM-as-judge; gated by judgeSampleRate (expensive)
	sink                Sink
	log                 *slog.Logger

	judgeBaseURL    string
	judgeModel      string
	judgeAPIKey     string
	remoteURL       string
	remoteMetrics   []string
	remoteTimeout   time.Duration
	judgeSampleRate float64
	workerCount     int
	evalTimeout     time.Duration

	ch     chan observability.Trace
	done   chan struct{}
	closed chan struct{}
	wg     sync.WaitGroup
	rnd    *rand.Rand
	rndMu  sync.Mutex
}

// Options configures the Worker.
type Options struct {
	PIIEnabled          bool
	CompletenessEnabled bool
	Judges              []Evaluator
	Sink                Sink
	JudgeBaseURL        string
	JudgeModel          string
	JudgeAPIKey         string
	RemoteURL           string
	RemoteMetrics       []string
	RemoteTimeout       time.Duration
	JudgeSampleRate     float64 // 0..1, fraction of traces sent to LLM judges
	Workers             int     // concurrent eval goroutines
	BufferSize          int
	EvalTimeout         time.Duration
}

// NewWorker builds and starts an eval worker.
func NewWorker(opts Options, log *slog.Logger) *Worker {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = 2000
	}
	if opts.EvalTimeout <= 0 {
		opts.EvalTimeout = 25 * time.Second
	}
	if opts.Sink == nil {
		opts.Sink = NoopSink{}
	}

	w := &Worker{
		piiEnabled:          opts.PIIEnabled,
		completenessEnabled: opts.CompletenessEnabled,
		judges:              opts.Judges,
		sink:                opts.Sink,
		log:                 log,
		judgeBaseURL:        opts.JudgeBaseURL,
		judgeModel:          opts.JudgeModel,
		judgeAPIKey:         opts.JudgeAPIKey,
		remoteURL:           opts.RemoteURL,
		remoteMetrics:       opts.RemoteMetrics,
		remoteTimeout:       opts.RemoteTimeout,
		judgeSampleRate:     opts.JudgeSampleRate,
		workerCount:         opts.Workers,
		evalTimeout:         opts.EvalTimeout,
		ch:                  make(chan observability.Trace, opts.BufferSize),
		done:                make(chan struct{}),
		closed:              make(chan struct{}),
		rnd:                 rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	w.wg.Add(opts.Workers)
	for i := 0; i < opts.Workers; i++ {
		go w.loop()
	}
	return w
}

// Record implements observability.Recorder. Non-blocking: drops the trace if
// the eval buffer is full (evaluation must never back-pressure the gateway).
func (w *Worker) Record(t observability.Trace) {
	// Only evaluate successful, non-empty completions.
	if t.StatusCode != 200 {
		return
	}
	select {
	case w.ch <- t:
	default:
		w.log.Warn("eval buffer full, dropping trace", "trace_id", t.TraceID)
	}
}

func (w *Worker) loop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.done:
			// Drain remaining traces before exiting.
			for {
				select {
				case t := <-w.ch:
					w.evaluate(t)
				default:
					return
				}
			}
		case t := <-w.ch:
			w.evaluate(t)
		}
	}
}

func (w *Worker) evaluate(t observability.Trace) {
	ctx, cancel := context.WithTimeout(context.Background(), w.evalTimeout)
	defer cancel()

	w.mu.RLock()
	piiOn := w.piiEnabled
	compOn := w.completenessEnabled
	judges := append([]Evaluator(nil), w.judges...)
	rate := w.judgeSampleRate
	w.mu.RUnlock()

	evaluators := make([]Evaluator, 0, 4+len(judges))
	if piiOn {
		evaluators = append(evaluators, PIIEvaluator{})
	}
	if compOn {
		evaluators = append(evaluators, CompletenessEvaluator{})
	}
	if len(judges) > 0 && w.sampleJudge(rate) {
		evaluators = append(evaluators, judges...)
	}

	var scores []Score
	for _, e := range evaluators {
		s, err := e.Evaluate(ctx, t)
		if err != nil {
			w.log.Warn("evaluator failed", "evaluator", e.Name(), "trace_id", t.TraceID, "err", err)
			continue
		}
		scores = append(scores, s...)
	}
	if len(scores) == 0 {
		return
	}
	// Stamp the caller's user_id so eval scores can be aggregated per user
	// (per-user quality), not just per virtual key — evaluators don't need to
	// know about tenancy.
	for i := range scores {
		if scores[i].UserID == "" {
			scores[i].UserID = t.UserID
		}
		if scores[i].RequestModel == "" {
			scores[i].RequestModel = traceModel(t)
		}
	}
	if err := w.sink.WriteScores(ctx, scores); err != nil {
		w.log.Error("write eval scores failed", "trace_id", t.TraceID, "err", err)
	}
}

func (w *Worker) sampleJudge(rate float64) bool {
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	w.rndMu.Lock()
	defer w.rndMu.Unlock()
	return w.rnd.Float64() < rate
}

// Close stops the workers and drains buffered traces.
func (w *Worker) Close(ctx context.Context) error {
	close(w.done)
	doneCh := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-ctx.Done():
	}
	return nil
}
