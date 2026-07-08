package main

import (
	"log/slog"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
)

const (
	traceStoreClickHouse = "clickhouse"
	traceStoreLiveOnly   = "live_only"

	statsStoreClickHouse = "clickhouse"
	statsStorePostgres   = "postgres"
)

// NexusStack holds composed trace, eval, and routing subsystems. Each piece
// uses its own interface (Recorder, Sink, StatsProvider) so backends can be
// swapped independently.
type NexusStack struct {
	Recorder          observability.Recorder
	Reader            *observability.Reader
	EvalWorker        *evals.Worker
	ModelRouter       *router.Router
	ScoreStore        evals.StoreKind
	TraceStore        string
	RoutingStatsStore string
}

func buildStack(cfg config.Config, hub *console.Hub, chRec *observability.CHRecorder, store *core.Store, log *slog.Logger) NexusStack {
	var stack NexusStack
	stack.TraceStore = traceStoreLiveOnly

	recorders := []observability.Recorder{hub}
	if chRec != nil {
		stack.Reader = chRec.NewReader()
		stack.TraceStore = traceStoreClickHouse
		recorders = append(recorders, chRec)
	}

	pgConnected := store != nil
	stack.ScoreStore = evals.ScoreStoreKind(chRec != nil, pgConnected)
	if cfg.EvalEnabled {
		stack.EvalWorker = buildEvalWorker(cfg, chRec, store, stack.ScoreStore, stack.TraceStore, log)
		recorders = append(recorders, stack.EvalWorker)
	} else {
		log.Info("eval worker disabled (NEXUS_EVAL_ENABLED=false)")
	}

	stack.Recorder = observability.NewMultiRecorder(recorders...)

	if provider, src := routingStatsProvider(chRec, store, stack.ScoreStore); provider != nil {
		stack.RoutingStatsStore = src
		stack.ModelRouter = router.New(
			provider,
			router.Weights{Quality: cfg.RouteWQuality, Cost: cfg.RouteWCost, Latency: cfg.RouteWLatency},
			cfg.RouteWindow, cfg.RouteRefresh, log,
		)
		log.Info("quality-aware routing enabled", "stats_store", src, "groups_spec", cfg.RouteGroups, "alias", "auto")
	} else {
		log.Info("quality-aware routing disabled (needs ClickHouse or Postgres eval scores)")
	}

	return stack
}

func routingStatsProvider(chRec *observability.CHRecorder, store *core.Store, scoreStore evals.StoreKind) (router.StatsProvider, string) {
	if chRec != nil {
		return router.NewCHStatsProvider(chRec.Conn()), statsStoreClickHouse
	}
	if store != nil && scoreStore == evals.StorePostgres {
		return router.NewPGStatsProvider(store.Pool()), statsStorePostgres
	}
	return nil, ""
}

func buildEvalWorker(cfg config.Config, chRec *observability.CHRecorder, store *core.Store, scoreKind evals.StoreKind, traceStore string, log *slog.Logger) *evals.Worker {
	var chConn driver.Conn
	if chRec != nil {
		chConn = chRec.Conn()
	}
	deps := evals.ScoreSinkDeps{CHConn: chConn}
	if store != nil {
		deps.PGPool = store.Pool()
	}
	sink := evals.NewScoreSink(scoreKind, deps)
	if scoreKind.Persisted() {
		log.Info("eval score persistence enabled", "store", scoreKind)
	} else {
		log.Info("eval scores not persisted", "store", scoreKind,
			"hint", "set NEXUS_CLICKHOUSE_URL or NEXUS_POSTGRES_URL for persistence")
	}

	var judges []evals.Evaluator
	if judge := evals.NewSLMJudge(evals.JudgeConfig{
		BaseURL: cfg.JudgeBaseURL,
		Model:   cfg.JudgeModel,
		APIKey:  cfg.JudgeAPIKey,
	}); judge != nil {
		judges = append(judges, judge)
		log.Info("eval SLM judge enabled", "model", cfg.JudgeModel, "sample_rate", cfg.EvalSampleRate)
	} else {
		log.Info("eval SLM judge disabled (set NEXUS_JUDGE_BASE_URL); heuristics still run")
	}
	if remote := evals.NewRemoteEvaluator(evals.RemoteConfig{
		BaseURL: cfg.EvalServiceURL,
		Metrics: splitCSV(cfg.EvalServiceMetrics),
		Timeout: cfg.EvalServiceTimeout,
	}); remote != nil {
		judges = append(judges, remote)
		log.Info("external eval service enabled", "url", cfg.EvalServiceURL, "metrics", cfg.EvalServiceMetrics)
	} else {
		log.Info("external eval service disabled (set NEXUS_EVAL_SERVICE_URL)")
	}

	worker := evals.NewWorker(evals.Options{
		PIIEnabled:          true,
		CompletenessEnabled: true,
		Judges:              judges,
		Sink:                sink,
		JudgeBaseURL:        cfg.JudgeBaseURL,
		JudgeModel:          cfg.JudgeModel,
		JudgeAPIKey:         cfg.JudgeAPIKey,
		RemoteURL:           cfg.EvalServiceURL,
		RemoteMetrics:       splitCSV(cfg.EvalServiceMetrics),
		RemoteTimeout:       cfg.EvalServiceTimeout,
		JudgeSampleRate:     cfg.EvalSampleRate,
		Workers:             cfg.EvalWorkers,
	}, log)
	log.Info("eval worker enabled", "score_store", scoreKind, "trace_store", traceStore)
	return worker
}
