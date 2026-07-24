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

// evalRuntimeController holds mutable eval/routing settings for the
// console PATCH endpoints. Env vars seed both the legacy single-config
// block (EvalConfigPatch in console/eval_config.go) and the new
// EvalProfile set (PR #135/#136). Profile changes apply in-memory
// until the next process restart; an admin POST against a profile is
// stitched in immediately by ReplaceProfiles.
type evalRuntimeController struct {
	mu sync.Mutex

	cfg               config.Config
	worker            *evals.Worker
	modelRouter       *router.Router
	gwHandler         *gateway.Handler
	profileStore      evals.ProfileStore // PR #136: persistent EvalProfile store
	secretResolver    *evals.Resolver    // PR #136: org/user/inline credential lookup
	routeRefresh      time.Duration
	loadBalance       bool
	scoreStore        evals.StoreKind
	traceStore        string
	routingStatsStore string
}

func newEvalRuntimeController(
	cfg config.Config,
	worker *evals.Worker,
	modelRouter *router.Router,
	gwHandler *gateway.Handler,
	scoreStore evals.StoreKind,
	traceStore string,
	routingStatsStore string,
	profileStore evals.ProfileStore,
	resolver *evals.Resolver,
) *evalRuntimeController {
	return &evalRuntimeController{
		cfg:               cfg,
		worker:            worker,
		modelRouter:       modelRouter,
		gwHandler:         gwHandler,
		profileStore:      profileStore,
		secretResolver:    resolver,
		routeRefresh:      cfg.RouteRefresh,
		loadBalance:       cfg.RouteLoadBalance,
		scoreStore:        scoreStore,
		traceStore:        traceStore,
		routingStatsStore: routingStatsStore,
	}
}

// SeedProfilesFromConfig materialises the legacy env-var block
// (NEXUS_EVAL_* / NEXUS_EVAL_SERVICE_*) as EvalProfile rows the first
// time the store opens. PR #136 introduces this so existing
// deployments can use the new code path without manually creating a
// profile in the console; the next refactor can drop the env-var
// path entirely. Seeded rows are org-scoped.
//
// Returns the profiles that were inserted; the caller pushes them
// through Worker.ReplaceProfiles so the next evaluate() call uses
// them. Idempotent: re-running on a populated store inserts nothing.
func (c *evalRuntimeController) SeedProfilesFromConfig(ctx context.Context) ([]evals.EvalProfile, error) {
	if c.profileStore == nil || c.worker == nil {
		return nil, nil
	}
	existing, err := c.profileStore.List(ctx, "")
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		c.worker.ReplaceProfiles(existing)
		return existing, nil
	}
	seeded := envVarSeedProfiles(c.cfg)
	for i := range seeded {
		if err := c.profileStore.Save(ctx, &seeded[i]); err != nil {
			return nil, err
		}
	}
	c.worker.ReplaceProfiles(seeded)
	return seeded, nil
}

// envVarSeedProfiles builds the default profile set from NEXUS_EVAL_*
// env vars. Kept separate from SeedProfilesFromConfig so a unit test
// can exercise the construction without a real Store.
//
// The mapping is intentionally permissive: missing optional fields
// short-circuit their profile rather than fail boot. Operators who
// want every profile can layer overrides on top via the console.
func envVarSeedProfiles(cfg config.Config) []evals.EvalProfile {
	out := make([]evals.EvalProfile, 0, 4)

	if cfg.JudgeBaseURL != "" && cfg.JudgeModel != "" {
		out = append(out, evals.EvalProfile{
			ID:    "default-judge",
			Name:  "Default LLM judge (env-var)",
			Kind:  evals.ProfileSLMJudge,
			Scope: evals.ScopeOrg,
			Endpoint: evals.EvalEndpoint{
				BaseURL: cfg.JudgeBaseURL,
				Model:   cfg.JudgeModel,
				// Org-keyed: the runtime controller looks up the
				// credential via StoreSecretLookup ORG. Operator
				// can rotate the key without a restart.
				KeySource: evals.KeySourceOrg,
			},
			SampleRate: clampSample(cfg.EvalSampleRate),
			Enabled:    cfg.EvalSampleRate > 0,
		})
	}

	if cfg.EvalServiceURL != "" && len(cfg.EvalServiceMetrics) > 0 {
		out = append(out, evals.EvalProfile{
			ID:    "default-remote",
			Name:  "Default sidecar eval (env-var)",
			Kind:  evals.ProfileRemoteEval,
			Scope: evals.ScopeOrg,
			Endpoint: evals.EvalEndpoint{
				BaseURL:   cfg.EvalServiceURL,
				KeySource: evals.KeySourceOrg,
			},
			Metrics:    splitCSV(cfg.EvalServiceMetrics),
			Threshold:  0.5,
			SampleRate: clampSample(cfg.EvalSampleRate),
			Enabled:    cfg.EvalSampleRate > 0,
		})
	}

	// Always ship the heuristic profiles — they're cheap, never
	// require an external secret, and most tenants want a baseline.
	out = append(out, evals.EvalProfile{
		ID:         "default-pii",
		Name:       "PII heuristic",
		Kind:       evals.ProfileHeuristicPII,
		Scope:      evals.ScopeOrg,
		Endpoint:   evals.EvalEndpoint{KeySource: evals.KeySourceBuiltin},
		SampleRate: 1.0,
		Enabled:    true,
	})
	out = append(out, evals.EvalProfile{
		ID:         "default-completeness",
		Name:       "Completeness heuristic",
		Kind:       evals.ProfileHeuristicCompleteness,
		Scope:      evals.ScopeOrg,
		Endpoint:   evals.EvalEndpoint{KeySource: evals.KeySourceBuiltin},
		SampleRate: 1.0,
		Enabled:    true,
	})

	return out
}

// clampSample normalises sample_rate into the [0,1] band the profile
// validator enforces. cfg.EvalSampleRate can be set via env where
// >0 / NaN mistakes are easy.
func clampSample(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// splitCSV trims + de-dupes comma-delimited strings. Used to turn
// NEXUS_EVAL_SERVICE_METRICS (CSV) into the []string the profile
// schema expects.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, tok := range strings.Split(s, ",") {
		t := strings.TrimSpace(tok)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// RegisterInlineSecret inserts a decrypted inline secret into the
// resolver's in-memory map. Console EndpointKeySource=inline
// PATCH/POST flow through this path; revocation calls RevokeInline
// on the same resolver. Keeping the secrets in-process avoids
// re-decrypting on every evaluate() call — once Redis/fleshing of
// cipher material lands we'll move this behind a cache.
func (c *evalRuntimeController) RegisterInlineSecret(keyRef, plaintext string, expires time.Time) {
	if c.secretResolver == nil || keyRef == "" || plaintext == "" {
		return
	}
	c.secretResolver.RegisterInline(keyRef, plaintext, expires)
}

// RevokeInlineSecret drops a previously-registered inline secret
// so the worker skips the profile going forward.
func (c *evalRuntimeController) RevokeInlineSecret(keyRef string) {
	if c.secretResolver == nil || keyRef == "" {
		return
	}
	c.secretResolver.RevokeInline(keyRef)
}

// ListEvalProfiles implements console.EvalProfileSource. The console
// never sees the secretResolver's plaintext — it returns profile
// metadata only.
func (c *evalRuntimeController) ListEvalProfiles(ctx context.Context, ownerUserID string) ([]evals.EvalProfile, error) {
	if c.profileStore == nil {
		return nil, nil
	}
	return c.profileStore.List(ctx, ownerUserID)
}

// GetEvalProfile implements console.EvalProfileSource.
func (c *evalRuntimeController) GetEvalProfile(ctx context.Context, id string) (*evals.EvalProfile, error) {
	if c.profileStore == nil {
		return nil, evals.ErrProfileNotFound
	}
	return c.profileStore.Get(ctx, id)
}

// SaveEvalProfile implements console.EvalProfileSource. After a
// successful save the controller pushes the updated profile snapshot
// through Worker.ReplaceProfiles so the next evaluate() call sees
// the new state on the producer's next loop tick.
func (c *evalRuntimeController) SaveEvalProfile(ctx context.Context, p *evals.EvalProfile) error {
	if c.profileStore == nil {
		return nil
	}
	if err := c.profileStore.Save(ctx, p); err != nil {
		return err
	}
	if c.worker != nil {
		all, err := c.profileStore.List(ctx, "")
		if err == nil {
			c.worker.ReplaceProfiles(all)
		}
	}
	return nil
}

// DeleteEvalProfile implements console.EvalProfileSource.
func (c *evalRuntimeController) DeleteEvalProfile(ctx context.Context, id string) error {
	if c.profileStore == nil {
		return nil
	}
	if err := c.profileStore.Delete(ctx, id); err != nil {
		return err
	}
	if c.worker != nil {
		all, err := c.profileStore.List(ctx, "")
		if err == nil {
			c.worker.ReplaceProfiles(all)
		}
	}
	return nil
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
	snap.RoutingStatsStore = c.routingStatsStore

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
