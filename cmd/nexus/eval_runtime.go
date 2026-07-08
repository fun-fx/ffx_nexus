package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/router"
)

// formatDuration renders a duration in the shortest natural form so UI inputs
// show "1h" instead of "1h0m0s". Falls back to the canonical form for unusual
// durations like "90m" → "1h30m" if the duration doesn't divide cleanly.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	// Try common units first: round to nearest minute, hour, second.
	total := d
	if total%time.Minute == 0 {
		if total%time.Hour == 0 {
			return fmt.Sprintf("%dh", int(total/time.Hour))
		}
		return fmt.Sprintf("%dm", int(total/time.Minute))
	}
	if total%time.Second == 0 {
		return fmt.Sprintf("%ds", int(total/time.Second))
	}
	return d.String()
}

// evalRuntimeController holds mutable eval/routing settings for the console
// PATCH /api/eval/config endpoint. Env vars seed the initial state; runtime
// changes apply in-memory until the next process restart.
type evalRuntimeController struct {
	mu sync.Mutex

	cfg          config.Config
	worker       *evals.Worker
	modelRouter  *router.Router
	gwHandler    *gateway.Handler
	routeRefresh time.Duration
	loadBalance  bool
	scoreStore   evals.StoreKind
	traceStore   string
}

func newEvalRuntimeController(
	cfg config.Config,
	worker *evals.Worker,
	modelRouter *router.Router,
	gwHandler *gateway.Handler,
	scoreStore evals.StoreKind,
	traceStore string,
) *evalRuntimeController {
	return &evalRuntimeController{
		cfg:          cfg,
		worker:       worker,
		modelRouter:  modelRouter,
		gwHandler:    gwHandler,
		routeRefresh: cfg.RouteRefresh,
		loadBalance:  cfg.RouteLoadBalance,
		scoreStore:   scoreStore,
		traceStore:   traceStore,
	}
}

func (c *evalRuntimeController) Snapshot() console.EvalConfigSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buildSnapshot()
}

func (c *evalRuntimeController) buildSnapshot() console.EvalConfigSnapshot {
	var snap console.EvalConfigSnapshot
	snap.EvalEnabled = c.worker != nil
	snap.RoutingEnabled = c.modelRouter != nil
	snap.ScoreStore = c.scoreStore.String()
	snap.TraceStore = c.traceStore
	snap.ScorePersisted = c.scoreStore.Persisted()

	if c.worker != nil {
		st := c.worker.RuntimeState()
		snap.Eval.PIIEnabled = st.PIIEnabled
		snap.Eval.CompletenessEnabled = st.CompletenessEnabled
		snap.Eval.SampleRate = st.SampleRate
		snap.Eval.Workers = st.Workers
		snap.Eval.Judge.Enabled = st.JudgeBaseURL != "" && st.JudgeModel != ""
		snap.Eval.Judge.BaseURL = st.JudgeBaseURL
		snap.Eval.Judge.Model = st.JudgeModel
		snap.Eval.Judge.APIKeySet = st.JudgeAPIKeySet
		snap.Eval.Remote.Enabled = st.RemoteURL != ""
		snap.Eval.Remote.URL = st.RemoteURL
		snap.Eval.Remote.Metrics = st.RemoteMetrics
		snap.Eval.Remote.Timeout = formatDuration(st.RemoteTimeout)
	}

	if c.modelRouter != nil {
		w := c.modelRouter.Weights()
		snap.Routing.Weights = map[string]float64{
			"quality": w.Quality,
			"cost":    w.Cost,
			"latency": w.Latency,
		}
		snap.Routing.Window = formatDuration(c.modelRouter.Window())
		snap.Routing.Refresh = formatDuration(c.routeRefresh)
		snap.Routing.LoadBalance = c.loadBalance
	}
	if c.gwHandler != nil {
		snap.Routing.Groups = c.gwHandler.RouteGroups()
		snap.Routing.GroupsSpec = config.FormatRouteGroups(snap.Routing.Groups)
	}

	snap.RestartRequired = []string{
		"eval_workers (NEXUS_EVAL_WORKERS)",
		"route_refresh (NEXUS_ROUTE_REFRESH)",
		"route_load_balance (NEXUS_ROUTE_LOAD_BALANCE)",
	}
	return snap
}

func (c *evalRuntimeController) Apply(patch console.EvalConfigPatch) (console.EvalConfigSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.worker == nil {
		return console.EvalConfigSnapshot{}, fmt.Errorf("eval worker not running")
	}

	if patch.PIIEnabled != nil {
		c.worker.SetPIIEnabled(*patch.PIIEnabled)
	}
	if patch.CompletenessEnabled != nil {
		c.worker.SetCompletenessEnabled(*patch.CompletenessEnabled)
	}
	if patch.SampleRate != nil {
		c.worker.SetJudgeSampleRate(*patch.SampleRate)
	}

	judgeChanged := false
	remoteChanged := false

	cur := c.worker.RuntimeState()
	judgeCfg := evals.JudgeRuntimeConfig{
		BaseURL: cur.JudgeBaseURL,
		Model:   cur.JudgeModel,
	}
	remoteCfg := evals.RemoteRuntimeConfig{
		URL:     cur.RemoteURL,
		Metrics: cur.RemoteMetrics,
		Timeout: cur.RemoteTimeout,
	}

	if patch.JudgeBaseURL != nil {
		judgeCfg.BaseURL = strings.TrimSpace(*patch.JudgeBaseURL)
		judgeChanged = true
	}
	if patch.JudgeModel != nil {
		judgeCfg.Model = strings.TrimSpace(*patch.JudgeModel)
		judgeChanged = true
	}
	if patch.JudgeAPIKey != nil && strings.TrimSpace(*patch.JudgeAPIKey) != "" {
		judgeCfg.APIKey = *patch.JudgeAPIKey
		judgeChanged = true
	}
	if patch.EvalServiceURL != nil {
		remoteCfg.URL = strings.TrimSpace(*patch.EvalServiceURL)
		remoteChanged = true
	}
	if patch.EvalServiceMetrics != nil {
		var metrics []string
		for _, m := range strings.Split(*patch.EvalServiceMetrics, ",") {
			if m = strings.TrimSpace(m); m != "" {
				metrics = append(metrics, m)
			}
		}
		remoteCfg.Metrics = metrics
		remoteChanged = true
	}
	if judgeChanged || remoteChanged {
		c.worker.ConfigureJudges(judgeCfg, remoteCfg)
	}

	if c.modelRouter != nil {
		w := c.modelRouter.Weights()
		if patch.RouteWQuality != nil {
			w.Quality = *patch.RouteWQuality
		}
		if patch.RouteWCost != nil {
			w.Cost = *patch.RouteWCost
		}
		if patch.RouteWLatency != nil {
			w.Latency = *patch.RouteWLatency
		}
		if patch.RouteWQuality != nil || patch.RouteWCost != nil || patch.RouteWLatency != nil {
			c.modelRouter.SetWeights(w)
		}
		if patch.RouteWindow != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*patch.RouteWindow))
			if err != nil {
				return console.EvalConfigSnapshot{}, fmt.Errorf("route_window: %w", err)
			}
			c.modelRouter.SetWindow(d)
			_ = c.modelRouter.Refresh(context.Background())
		}
	}

	if patch.RouteGroups != nil && c.gwHandler != nil {
		groups := config.ParseRouteGroups(*patch.RouteGroups)
		c.gwHandler.SetRouteGroups(groups)
	}

	return c.buildSnapshot(), nil
}
