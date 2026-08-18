package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evalplugin"
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
	pluginStore       evalplugin.PluginStore
	workerPlugins     *evalplugin.Registry
	secretResolver    *evals.Resolver // PR #136: org/user/inline credential lookup
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
	pluginStore evalplugin.PluginStore,
) *evalRuntimeController {
	if pluginStore == nil {
		pluginStore = evalplugin.NewMemoryStore(nil)
	}
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
		pluginStore:       pluginStore,
	}
}

// PluginStore exposes the controller's plugin store so the console
// admin REST can CRUD plugins against the same backing store the
// registry reads on boot.
//
// With Postgres the store is evalplugin.PostgresStore, so installs
// survive a restart. In single-binary deployments (no Postgres) it is
// the in-process MemoryStore; rows are lost on restart but the
// cluster-wide Helm-installed plugins still load from
// /etc/nexus/eval-plugins/.
func (c *evalRuntimeController) PluginStore() evalplugin.PluginStore {
	return c.pluginStore
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
// On plugin-only runs with
// NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT=true the four well-known
// default rows — default-pii, default-completeness, default-judge,
// default-remote — are hard-deleted from the store on each boot so
// the cluster converges on a plugin-only profile set without manual
// console cleanup. The two-flag gate (EvalPluginOnly = seed skip,
// PurgeLegacyProfilesOnBoot = legacy row deletion) keeps them as
// separate intentional actions — flipping only EvalPluginOnly
// leaves historical rows intact.
func (c *evalRuntimeController) SeedProfilesFromConfig(ctx context.Context) ([]evals.EvalProfile, error) {
	if c.profileStore == nil {
		return nil, nil
	}
	if c.cfg.EvalPluginOnly && c.cfg.PurgeLegacyProfilesOnBoot {
		if err := c.purgeLegacySeededProfiles(ctx); err != nil {
			return nil, err
		}
	}
	existing, err := c.profileStore.List(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if c.worker != nil {
		c.worker.ReplaceProfiles(existing)
	}
	if len(existing) > 0 {
		return existing, nil
	}
	seeded := envVarSeedProfiles(c.cfg)
	for i := range seeded {
		if err := c.profileStore.Save(ctx, &seeded[i]); err != nil {
			return nil, err
		}
	}
	if c.worker != nil {
		c.worker.ReplaceProfiles(seeded)
	}
	return seeded, nil
}

// purgeLegacySeededProfiles removes the four well-known seed rows
// — default-pii, default-completeness, default-judge, default-remote
// — from the profile store. Called by SeedProfilesFromConfig when
// both NEXUS_EVAL_PLUGIN_ONLY and
// NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT are set.
//
// Operators that flip the destructive flag on under the assumption
// that an upstream plugin covers PII / completeness detection never
// need to clean the rows up themselves — every subsequent boot
// calls this until the rows are gone. The deletes are idempotent at
// the store layer (Delete returns nil on missing) so re-running on
// a fresh store is also safe.
//
// Each id must match the seed string exactly. Operators that
// copy-renamed a default-* row don't get a stray-naming-convention
// surprise when they flip the flag on.
func (c *evalRuntimeController) purgeLegacySeededProfiles(ctx context.Context) error {
	wells := []string{"default-pii", "default-completeness", "default-judge", "default-remote"}
	for _, id := range wells {
		if err := c.profileStore.Delete(ctx, id); err != nil {
			return fmt.Errorf("purge legacy profile %q: %w", id, err)
		}
	}
	return nil
}

// envVarSeedProfiles builds the default profile set from NEXUS_EVAL_*
// env vars. Kept separate from SeedProfilesFromConfig so a unit test
// can exercise the construction without a real Store.
//
// The mapping is intentionally permissive: missing optional fields
// short-circuit their profile rather than fail boot. Operators who
// want every profile can layer overrides on top via the console.
//
// NEXUS_EVAL_PLUGIN_ONLY suppresses the heuristic PII / Completeness
// auto-seed entirely — for installations that want *only* external
// plugin-driven eval. The flag is non-destructive: existing rows in
// the store are not deleted; admins delete them through the console
// if needed.
func envVarSeedProfiles(cfg config.Config) []evals.EvalProfile {
	out := make([]evals.EvalProfile, 0, 2)

	if !cfg.EvalPluginOnly && cfg.JudgeBaseURL != "" && cfg.JudgeModel != "" {
		out = append(out, evals.EvalProfile{
			ID:    "default-judge",
			Name:  "Default LLM judge (env-var, legacy)",
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

	if !cfg.EvalPluginOnly && cfg.EvalServiceURL != "" && len(cfg.EvalServiceMetrics) > 0 {
		out = append(out, evals.EvalProfile{
			ID:    "default-remote",
			Name:  "Default sidecar eval (env-var, legacy)",
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

	// Heuristic profiles ship by default in v0.7+. They are 0-cost
	// (in-process regex) and provide baseline coverage for every
	// fresh install. Plugin evaluators add LLM-as-judge / framework
	// integration on top. NEXUS_EVAL_PLUGIN_ONLY=true opts out of
	// this auto-seed so an installation using only external plugins
	// (Langfuse, LangSmith, Confident AI, Arize Phoenix) doesn't
	// double-charge traces with redundant in-process scoring.
	if cfg.EvalPluginOnly {
		return out
	}
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
func (c *evalRuntimeController) ListEvalProfiles(ctx context.Context, orgID, ownerUserID string) ([]evals.EvalProfile, error) {
	if c.profileStore == nil {
		return nil, nil
	}
	return c.profileStore.List(ctx, orgID, ownerUserID)
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
	// A judge or sidecar profile scores nothing under plugin-only, and
	// storing one would leave an enabled row in the console that never
	// produces a score — the failure mode this whole change exists to
	// remove. Refuse it at the write instead.
	if p != nil && c.worker != nil && c.worker.PluginOnly() {
		switch p.Kind {
		case evals.ProfileSLMJudge, evals.ProfileRemoteEval:
			return fmt.Errorf(
				"eval plugin-only mode is on: %q profiles need eval compute in the cluster "+
					"(use an eval plugin, or unset NEXUS_EVAL_PLUGIN_ONLY)", p.Kind)
		}
	}
	if err := c.profileStore.Save(ctx, p); err != nil {
		return err
	}
	if c.worker != nil {
		all, err := c.profileStore.List(ctx, "", "")
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
		all, err := c.profileStore.List(ctx, "", "")
		if err == nil {
			c.worker.ReplaceProfiles(all)
		}
	}
	return nil
}

// ListEvalPlugins implements console.EvalPluginSource. The console
// passes the org_id from the session; cluster-wide rows (org_id="")
// are visible to every caller, per-org rows only to their own org.
func (c *evalRuntimeController) ListEvalPlugins(ctx context.Context, orgID string) ([]evalplugin.PluginRecord, error) {
	if c.pluginStore == nil {
		return nil, nil
	}
	return c.pluginStore.List(ctx, orgID)
}

// GetEvalPlugin implements console.EvalPluginSource.
func (c *evalRuntimeController) GetEvalPlugin(ctx context.Context, id string) (*evalplugin.PluginRecord, error) {
	if c.pluginStore == nil {
		return nil, evalplugin.ErrPluginNotFound
	}
	return c.pluginStore.Get(ctx, id)
}

// LookupEvalPlugin resolves a plugin record by metadata.name,
// preferring cluster-wide rows so admins can target Helm-installed
// plugins without remembering their DB id. The merged Registry is
// consulted first; the DB store is the fallback for per-org rows.
func (c *evalRuntimeController) LookupEvalPlugin(ctx context.Context, name string) (*evalplugin.PluginRecord, error) {
	if name == "" {
		return nil, evalplugin.ErrPluginNotFound
	}
	c.mu.Lock()
	reg := c.workerPlugins
	c.mu.Unlock()
	if reg != nil {
		if rec, ok := reg.Lookup(name); ok {
			return &evalplugin.PluginRecord{
				Name:     rec.Plugin.Metadata.Name,
				SpecYAML: pluginToYAML(rec.Plugin),
				Enabled:  rec.Enabled,
			}, nil
		}
	}
	if c.pluginStore == nil {
		return nil, evalplugin.ErrPluginNotFound
	}
	all, err := c.pluginStore.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, rec := range all {
		if rec.Name == name {
			return &rec, nil
		}
	}
	return nil, evalplugin.ErrPluginNotFound
}

// pluginToYAML is a stub used by LookupEvalPlugin so admins see the
// same spec content they shipped. The full round-trip via the YAML
// manifest happens at Save time.
func pluginToYAML(p *evalplugin.Plugin) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// SaveEvalPlugin implements console.EvalPluginSource. After save we
// re-load the registry so the runtime dispatcher sees the new plugin
// without a process restart.
func (c *evalRuntimeController) SaveEvalPlugin(ctx context.Context, rec *evalplugin.PluginRecord) error {
	if c.pluginStore == nil {
		return nil
	}
	if err := c.pluginStore.Save(ctx, rec); err != nil {
		return err
	}
	c.mu.Lock()
	plugins := c.workerPlugins
	c.mu.Unlock()
	if plugins != nil {
		_ = plugins.LoadFromStore(ctx, c.pluginStore, rec.OrgID)
	}
	return nil
}

// DeleteEvalPlugin implements console.EvalPluginSource.
func (c *evalRuntimeController) DeleteEvalPlugin(ctx context.Context, id, orgID string) error {
	if c.pluginStore == nil {
		return nil
	}
	if err := c.pluginStore.Delete(ctx, id); err != nil {
		return err
	}
	c.mu.Lock()
	plugins := c.workerPlugins
	c.mu.Unlock()
	if plugins != nil {
		_ = plugins.LoadFromStore(ctx, c.pluginStore, orgID)
	}
	return nil
}

// AttachPluginRegistry lets main.go wire the worker's plugin
// registry into the controller; PATCH/POST/DELETE on the admin
// routes push a refresh through here so the dispatcher sees the
// new state without a process restart.
func (c *evalRuntimeController) AttachPluginRegistry(reg *evalplugin.Registry) {
	c.mu.Lock()
	c.workerPlugins = reg
	c.mu.Unlock()
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
		snap.Eval.SampleRate = st.SampleRate
		snap.Eval.Workers = st.Workers

		// v0.6.9: every heuristic ("is X currently scoring?") is sourced
		// from the profile snapshot — not from Worker.piiEnabled /
		// Worker.completenessEnabled, which were removed in this PR. The
		// env-derived wiring (BaseURL / Model / URL / Metrics) still
		// comes from RuntimeState so the operator can see what's deployed.
		ps := c.worker.ProfileStatus()
		snap.Eval.Judge.BaseURL = st.JudgeBaseURL
		snap.Eval.Judge.Model = st.JudgeModel
		snap.Eval.Judge.APIKeySet = st.JudgeAPIKeySet
		snap.Eval.Judge.Enabled = ps.SLMJudgeEnabled
		snap.Eval.Remote.URL = st.RemoteURL
		snap.Eval.Remote.Metrics = st.RemoteMetrics
		snap.Eval.Remote.Timeout = formatDuration(st.RemoteTimeout)
		snap.Eval.Remote.Enabled = ps.RemoteEvalEnabled
		snap.Eval.PIIEnabled = ps.PIIEnabled
		snap.Eval.CompletenessEnabled = ps.CompletenessEnabled
	}

	// Surface which seeding policy this pod is running with so the
	// console can render a banner ("plugin-only mode") without
	// having to keep its own copy of the env var.
	snap.PluginOnly = c.cfg.EvalPluginOnly
	snap.PurgeLegacyProfilesOnBoot = c.cfg.PurgeLegacyProfilesOnBoot

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

		// Bench blend is purely env-driven today; the operator
		// sees the live configuration rather than an inferred
		// value. BenchEnabled flips on only when either backend
		// carrying the benchmark_runs table is connected and
		// the operator has not defaulted the weight to zero —
		// both conditions are necessary. Postgres and ClickHouse
		// are both supported as of the (status=completed,
		// avg_score is not null) snapshot tables each backend
		// grows independently. The check intentionally excludes
		// `live_only` (the recording backend without a stats
		// store) because that path has no benchmark rows yet.
		snap.Routing.BenchWeight = c.cfg.RouteWBench
		snap.Routing.BenchDecay = formatDuration(c.cfg.RouteBenchHalfLife)
		snap.Routing.BenchEnabled = (c.routingStatsStore == "postgres" ||
			c.routingStatsStore == "clickhouse") && c.cfg.RouteWBench > 0
	}
	if c.gwHandler != nil {
		snap.Routing.Groups = c.gwHandler.RouteGroups()
		snap.Routing.GroupsSpec = config.FormatRouteGroups(snap.Routing.Groups)
	}

	snap.RestartRequired = []string{
		"eval_workers (NEXUS_EVAL_WORKERS)",
		"route_refresh (NEXUS_ROUTE_REFRESH)",
		"route_load_balance (NEXUS_ROUTE_LOAD_BALANCE)",
		"route_w_bench (NEXUS_ROUTE_W_BENCH)",
		"route_bench_half_life (NEXUS_ROUTE_BENCH_HALF_LIFE)",
	}
	return snap
}

func (c *evalRuntimeController) Apply(patch console.EvalConfigPatch) (console.EvalConfigSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.worker == nil {
		return console.EvalConfigSnapshot{}, fmt.Errorf("eval worker not running")
	}

	// v0.6.9: PII / Completeness toggles ARE NOT handled here. They live in
	// the profile store and the console hits PATCH /api/eval/profiles/<id>
	// for those flips. `{eval:{pii_enabled:...}}` field patches arriving
	// here are dropped on the floor; the upstream console router already
	// rejects them, so this is defense-in-depth for direct API callers.
	// Only judge-side wiring (BaseURL / Model / API key / SampleRate /
	// sidecar URL / metrics) flows through here.
	// Under plugin-only the judge and the sidecar are not wired, so a
	// patch that sets either would be accepted and then do nothing.
	// Fail it loudly instead: an operator who wants that compute back
	// has to turn the flag off, which is a deployment decision rather
	// than a console toggle.
	if c.worker.PluginOnly() && patchTouchesEvalCompute(patch) {
		return console.EvalConfigSnapshot{}, fmt.Errorf(
			"eval plugin-only mode is on: judge and eval-service settings are not configurable " +
				"(unset NEXUS_EVAL_PLUGIN_ONLY to run eval compute in the cluster)")
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

// patchTouchesEvalCompute reports whether the patch would wire the LLM
// judge or the Python eval sidecar. The API key counts: it only matters
// alongside a judge, and accepting it under plugin-only would store a
// credential for a path that never runs.
func patchTouchesEvalCompute(patch console.EvalConfigPatch) bool {
	return patch.JudgeBaseURL != nil ||
		patch.JudgeModel != nil ||
		patch.JudgeAPIKey != nil ||
		patch.EvalServiceURL != nil ||
		patch.EvalServiceMetrics != nil
}
