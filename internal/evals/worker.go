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
//
// PR #135: the worker is now profile-driven. Each EvalProfile can
// toggle Enabled, set its own sample_rate, scope to org or user, and
// attach to an external endpoint. The default `workerPool` (Workers
// in Options) is replaced by a bounded goroutine fan-out at evaluate()
// time — sequential evaluator dispatch in PR #132 era cost O(n) round
// trips per trace; with PR #135 we run all enabled evaluators
// concurrently with a WaitGroup so a slow remote judge no longer
// blocks cheap heuristics.
type Worker struct {
	mu sync.RWMutex

	// judges is the legacy LLM-as-judge slice built once from env and
	// (optionally) updated by ConfigureJudges. Profile-driven evaluators
	// arrive via ReplaceProfiles and are kept as configuredProfiles
	// below; this slice is the env-var seeding fallback for tenants
	// that don't run with profile-driven CRDs yet.
	judges []Evaluator
	sink   Sink
	log    *slog.Logger
	// metricsRecorder is the gateway's Prometheus recorder. When non-nil,
	// every successful eval score for metric="quality" is also propagated to
	// nexus_eval_quality_score so the Grafana `Quality judge score (rolling
	// 1h mean)` panel is fed even when the clickhouse sink is filtering at a
	// different rate. Optional — keeps existing callers source-compatible.
	metricsRecorder *observability.MetricsRecorder

	judgeBaseURL    string
	judgeModel      string
	judgeAPIKey     string
	remoteURL       string
	remoteMetrics   []string
	remoteTimeout   time.Duration
	judgeSampleRate float64
	workerCount     int
	evalTimeout     time.Duration

	// ConfiguredProfiles is the snapshot used by the next evaluate()
	// call. Refreshed via ReplaceProfiles() which holds w.mu briefly so
	// readers run against a consistent slice. The snapshot is built in
	// cmd/nexus/eval_runtime.go (PR #135) from the ProfileStore plus
	// the runtime controller.
	configuredProfiles []EvalProfile

	// secretResolver is the per-profile secret lookup hook set by
	// SetSecretResolver. Resolved at evaluate() time so a profile's
	// referenced credential is fetched on the worker's goroutine and
	// the plaintext is gone once Evaluate() finishes.
	secretResolver SecretResolver

	ch     chan observability.Trace
	done   chan struct{}
	closed chan struct{}
	wg     sync.WaitGroup
	rnd    *rand.Rand
	rndMu  sync.Mutex
}

// Options configures the Worker.
type Options struct {
	Judges          []Evaluator
	Sink            Sink
	JudgeBaseURL    string
	JudgeModel      string
	JudgeAPIKey     string
	RemoteURL       string
	RemoteMetrics   []string
	RemoteTimeout   time.Duration
	JudgeSampleRate float64 // 0..1, fraction of traces sent to LLM judges
	Workers         int     // concurrent eval goroutines
	BufferSize      int
	EvalTimeout     time.Duration
	// MetricsRecorder, if non-nil, receives RecordQualityScore calls so eval
	// results feed the Prometheus nexus_eval_quality_score gauge as well as
	// the clickhouse/pg sink. Optional; nil = no metric propagation.
	MetricsRecorder *observability.MetricsRecorder
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
		judges:          opts.Judges,
		sink:            opts.Sink,
		log:             log,
		judgeBaseURL:    opts.JudgeBaseURL,
		judgeModel:      opts.JudgeModel,
		judgeAPIKey:     opts.JudgeAPIKey,
		remoteURL:       opts.RemoteURL,
		remoteMetrics:   opts.RemoteMetrics,
		remoteTimeout:   opts.RemoteTimeout,
		judgeSampleRate: opts.JudgeSampleRate,
		workerCount:     opts.Workers,
		evalTimeout:     opts.EvalTimeout,
		metricsRecorder: opts.MetricsRecorder,
		ch:              make(chan observability.Trace, opts.BufferSize),
		done:            make(chan struct{}),
		closed:          make(chan struct{}),
		rnd:             rand.New(rand.NewSource(time.Now().UnixNano())),
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

	// Per-trace evaluator slice built from configured profiles plus the
	// legacy single-config path. PR #135 widens this to "any number of
	// EvalProfile entries" so multiple scoring strategies (admin
	// heuristic, user BYOK judge, org remote eval) can co-exist.
	resolver := w.SecretResolver()
	w.mu.RLock()
	profiles := append([]EvalProfile(nil), w.configuredProfiles...)
	judges := append([]Evaluator(nil), w.judges...)
	rate := w.judgeSampleRate
	w.mu.RUnlock()

	evaluators := w.collectEvaluators(t, profiles, judges, rate, resolver)
	if len(evaluators) == 0 {
		return
	}
	initialGuess := len(evaluators) * 2
	bag := newScoreBag(initialGuess)
	w.runEvaluators(ctx, t, evaluators, bag)
	scs := bag.take()
	if len(scs) == 0 {
		return
	}
	// Stamp caller info on every score so per-user aggregates remain
	// intact. Evaluators don't know about tenancy today; centralising
	// the stamping ensures we don't drown the schema later.
	for i := range scs {
		if scs[i].UserID == "" {
			scs[i].UserID = t.UserID
		}
		if scs[i].RequestModel == "" {
			scs[i].RequestModel = traceModel(t)
		}
	}
	if err := w.sink.WriteScores(ctx, scs); err != nil {
		w.log.Error("write eval scores failed", "trace_id", t.TraceID, "err", err)
	}
	// Mirror successful quality scores into the in-process metrics recorder so
	// Prometheus shows nexus_eval_quality_score{model="…"} alongside the
	// clickhouse persist path. We do this after WriteScores (not before) so
	// a sink outage never causes a fake metric spike. Only metric=="quality"
	// is propagated — the heuristic panels are aggregated later and the
	// per-model consult would just add noise.
	if w.metricsRecorder != nil {
		for _, s := range scs {
			if s.Metric != "quality" {
				continue
			}
			w.metricsRecorder.RecordQualityScore(s.RequestModel, s.Score)
		}
	}
}

// collectEvaluators resolves the set of Evaluator instances for a
// single trace. PR #135: it's profile-driven, so you can have any
// combination of heuristic + judge + remote-eval entries in the same
// snapshot. Each profile can independently toggle itself on/off and
// contribute its own sample gate.
//
// resolver is the secret-resolution hook (defaults to nil in tests; the
// runtime controller wires the org/user/inline lookup in PR #136). A
// nil resolver short-circuits profile resolution unless the kind is
// builtin (heuristics never need a secret).
func (w *Worker) collectEvaluators(
	t observability.Trace,
	profiles []EvalProfile,
	judges []Evaluator,
	rate float64,
	resolver SecretResolver,
) []Evaluator {
	// "Policy A" personal override: a user-scope profile of kind X with
	// enabled=false suppresses the org-scope profile of the same kind
	// for that user's traces only. Additive rule: an enabled user-scope
	// profile still scores alongside the org profile (the user is
	// delegating to a different model, not replacing — that's a
	// documented operator decision). Owner identity comes from
	// Trace.UserID set by the gateway recorder fan-out.
	disabledKindsByOwner := map[ProfileKind]bool{}
	owner := t.UserID
	if owner != "" {
		for _, ep := range profiles {
			if ep.Scope != ScopeUser || ep.OwnerUserID != owner {
				continue
			}
			if ep.Enabled {
				continue
			}
			disabledKindsByOwner[ep.Kind] = true
		}
	}

	evs := make([]Evaluator, 0, len(profiles)+len(judges)+2)
	// v0.6.9: PII / Completeness are no longer toggled at the Worker
	// level — the env-var seeded default-pii / default-completeness
	// profiles (scope=org) are the single source of truth and arrive
	// via ReplaceProfiles on every boot. The previous `if w.piiEnabled`
	// short-circuit duplicated scoring for tenants that had both
	// legacy state and seeded profiles; v0.6.8 console patches that
	// still hit /api/eval/config PII fields are rejected by the
	// console router (we removed the handler there too).
	for _, ep := range profiles {
		if !ep.Enabled {
			continue
		}
		if ep.SampleRate <= 0 {
			continue
		}
		if ep.SampleRate < 1 && !w.sampleJudge(ep.SampleRate) {
			continue
		}
		// Org-scoped entries of a kind disabled-by-owner for the trace's
		// owner are skipped. The user-scoped entry that disabled them
		// already dropped out a few lines above (!ep.Enabled short-circuit),
		// so nothing else needs filtering.
		if ep.Scope == ScopeOrg && disabledKindsByOwner[ep.Kind] {
			continue
		}
		// Built-in heuristics don't need a secret; ignore resolver.
		switch ep.Kind {
		case ProfileHeuristicPII:
			evs = append(evs, PIIEvaluator{})
			continue
		case ProfileHeuristicCompleteness:
			evs = append(evs, CompletenessEvaluator{})
			continue
		}
		// Profiles that need an LLM resolution short-circuit when
		// the resolver is nil (callers in tests). In production the
		// runtime controller always wires a resolver.
		if resolver == nil {
			continue
		}
		secret, err := resolver(t, ep.Endpoint)
		if err != nil || secret == "" {
			w.log.Warn(
				"eval profile secret unresolved",
				"profile_id", ep.ID,
				"name", ep.Name,
				"kind", string(ep.Kind),
				"key_source", string(ep.Endpoint.KeySource),
				"err", err,
			)
			continue
		}
		switch ep.Kind {
		case ProfileSLMJudge:
			if j := NewSLMJudge(JudgeConfig{
				BaseURL: ep.Endpoint.BaseURL,
				Model:   ep.Endpoint.Model,
				APIKey:  secret,
			}); j != nil {
				evs = append(evs, j)
			}
		case ProfileRemoteEval:
			if r := NewRemoteEvaluator(RemoteConfig{
				BaseURL: ep.Endpoint.BaseURL,
				Metrics: ep.Metrics,
				APIKey:  secret,
				Timeout: 30 * time.Second,
			}); r != nil {
				evs = append(evs, r)
			}
		}
	}
	// Legacy judges slice preserved for backward compat — anything
	// already wired into Worker.judges at construction time still
	// participates. Sample-gated identically to before.
	if len(judges) > 0 && w.sampleJudge(rate) {
		evs = append(evs, judges...)
	}
	return evs
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

// ProfileKindSummary is exported here for runtime controller / UI consumption.
// We only need to know whether *any* profile of a given kind is enabled
// to drive the disabled-state UI; the rest of the profile API stays
// where the rest of the eval logic lives.
//
// Includes the heuristic kinds (PII / Completeness) so the console can
// present a single uniform "is this evaluator currently scoring?"
// strip without reaching into legacy Worker fields. v0.6.9 removed
// the legacy Worker.piiEnabled / Worker.completenessEnabled toggles —
// these are now derived purely from profile state.
type ProfileKindSummary struct {
	SLMJudgeEnabled     bool
	RemoteEvalEnabled   bool
	PIIEnabled          bool
	CompletenessEnabled bool
}

// ProfileStatus returns the user-facing "is this evaluator currently
// running?" view derived from the profile snapshot, mirroring the gate
// in collectEvaluators (which skips !Enabled profiles before anything
// else). Default-* profiles seeded from env count as profile state too;
// toggling default-judge.enabled=false flips this flag in lockstep.
func (w *Worker) ProfileStatus() ProfileKindSummary {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var out ProfileKindSummary
	for i := range w.configuredProfiles {
		ep := &w.configuredProfiles[i]
		if !ep.Enabled {
			continue
		}
		switch ep.Kind {
		case ProfileSLMJudge:
			out.SLMJudgeEnabled = true
		case ProfileRemoteEval:
			out.RemoteEvalEnabled = true
		case ProfileHeuristicPII:
			out.PIIEnabled = true
		case ProfileHeuristicCompleteness:
			out.CompletenessEnabled = true
		}
	}
	return out
}

// ReplaceProfiles atomically swaps in the next snapshot of profiles
// the worker uses for per-trace evaluation. The snapshot is owned by
// the worker after the swap (we copy in), so the runtime controller
// can drop its reference without keeping heavy objects alive.
func (w *Worker) ReplaceProfiles(profiles []EvalProfile) {
	cp := make([]EvalProfile, 0, len(profiles))
	for i := range profiles {
		cp = append(cp, *profiles[i].Clone())
	}
	w.mu.Lock()
	w.configuredProfiles = cp
	w.mu.Unlock()
}

// SecretResolver returns the plaintext API secret backing an endpoint,
// or "" if the source is nil / not found / forbidden to this caller.
// PR #136 wires the real implementation (org / user / inline lookup
// against provider_credentials and the eval_credentials table); for
// #135 it's a no-op slot so the worker compiles and tests cover the
// shape of the resolver signature.
type SecretResolver func(t observability.Trace, ep EvalEndpoint) (string, error)

// SetSecretResolver attaches a SecretResolver. nil disables profile
// evaluation that requires a secret (heuristics still run).
func (w *Worker) SetSecretResolver(r SecretResolver) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.secretResolver = r
}

// SecretResolver returns the currently bound resolver (or nil).
func (w *Worker) SecretResolver() SecretResolver {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.secretResolver
}

// SetMetricsRecorder wires an optional observability.MetricsRecorder so eval
// quality scores flow into Prometheus's nexus_eval_quality_score gauge. Safe
// to call once after construction (post-startup wire-up is the typical case).
// nil releases the previous recorder.
func (w *Worker) SetMetricsRecorder(r *observability.MetricsRecorder) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.metricsRecorder = r
}
