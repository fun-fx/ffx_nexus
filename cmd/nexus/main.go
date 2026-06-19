// Command nexus is the single-binary LLM gateway: an OpenAI-compatible proxy
// with built-in observability and a live dashboard API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/balancer"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/gateway/providers"
	"github.com/ffxnexus/nexus/internal/guardrails"
	"github.com/ffxnexus/nexus/internal/limiter"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
	"github.com/ffxnexus/nexus/internal/semcache"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Control plane (optional): Postgres-backed store for virtual keys and
	// encrypted provider credentials. Boots without it (zero-dependency mode).
	cipher, err := crypto.NewCipher(cfg.MasterKey)
	if err != nil {
		log.Error("invalid NEXUS_MASTER_KEY; credential encryption disabled", "err", err)
		cipher = nil
	}

	var store *core.Store
	var auth gateway.VKeyAuthenticator
	if cfg.PostgresURL != "" {
		st, err := core.NewStore(ctx, cfg.PostgresURL, cipher)
		if err != nil {
			log.Error("postgres connect failed; continuing without control plane", "err", err)
		} else {
			for _, path := range []string{
				"migrations/postgres/001_init.sql",
				"migrations/postgres/002_byok.sql",
				"migrations/postgres/003_sso.sql",
			} {
				schema, _ := nexus.Migrations.ReadFile(path)
				if err := st.Migrate(ctx, string(schema)); err != nil {
					log.Error("postgres migrate failed", "file", path, "err", err)
				}
			}
			store = st
			auth = makeAuthenticator(st)
			log.Info("control plane enabled (virtual key auth + credential store)",
				"credential_encryption", st.HasCipher())
			bootstrapAdmin(ctx, st, cfg, log)
		}
	} else {
		log.Info("postgres not configured; key auth disabled, provider keys from env only")
	}

	// Limiter: Redis (shared across replicas) or in-memory fallback. Only
	// enforced for authenticated requests (virtual key present).
	var redisLim *limiter.Redis
	var lim gateway.Limiter
	if cfg.RedisURL != "" {
		rl, err := limiter.NewRedis(ctx, cfg.RedisURL)
		if err != nil {
			log.Error("redis connect failed; using in-memory limiter", "err", err)
			lim = limiter.NewMemory()
		} else {
			redisLim = rl
			lim = rl
			log.Info("redis limiter enabled (shared rate limits + budgets)")
		}
	} else {
		lim = limiter.NewMemory()
		log.Info("redis not configured; using in-memory limiter (single-node)")
	}

	// Provider registry: env-configured providers plus any DB-stored credentials.
	reg := gateway.NewRegistry()
	registerProviders(reg, cfg, log)
	if store != nil {
		registerStoredCredentials(ctx, reg, store, cfg, log)
	}
	if len(reg.AllModels()) == 0 {
		log.Warn("no providers configured; set OPENAI_API_KEY / ANTHROPIC_API_KEY / GEMINI_API_KEY or add credentials via the console")
	}

	// Observability: ClickHouse persistence (optional) + live dashboard hub.
	hub := console.NewHub()
	var chRec *observability.CHRecorder
	var reader *observability.Reader
	recorders := []observability.Recorder{hub}

	if cfg.ClickHouseURL != "" {
		rec, err := observability.NewCHRecorder(ctx, cfg.ClickHouseURL, observability.CHOptions{}, log)
		if err != nil {
			log.Error("clickhouse connect failed; continuing without persistence", "err", err)
		} else {
			for _, path := range []string{
				"migrations/clickhouse/001_init.sql",
				"migrations/clickhouse/002_eval_context.sql",
				"migrations/clickhouse/003_dashboard.sql",
				"migrations/clickhouse/004_byok.sql",
				"migrations/clickhouse/005_eval_user.sql",
			} {
				schema, _ := nexus.Migrations.ReadFile(path)
				if err := rec.Migrate(ctx, string(schema)); err != nil {
					log.Error("clickhouse migrate failed", "file", path, "err", err)
				}
			}
			chRec = rec
			reader = rec.NewReader()
			recorders = append(recorders, rec)
			log.Info("clickhouse trace persistence enabled")
		}
	} else {
		log.Info("clickhouse not configured; traces are live-only (set NEXUS_CLICKHOUSE_URL to persist)")
	}

	// Eval worker (Phase 3): async, out-of-band quality evaluation. Heuristics
	// (PII/completeness) always run; the local SLM judge runs on sampled traces.
	// Scores persist to ClickHouse eval_scores and feed quality-aware routing.
	var evalWorker *evals.Worker
	if chRec != nil {
		heuristics := []evals.Evaluator{evals.PIIEvaluator{}, evals.CompletenessEvaluator{}}
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
		// External Python eval service (DeepEval/RAGAS). Sample-gated like the
		// SLM judge; failures degrade gracefully to the Go heuristics.
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
		evalWorker = evals.NewWorker(evals.Options{
			Heuristics:      heuristics,
			Judges:          judges,
			Sink:            evals.NewCHSink(chRec.Conn()),
			JudgeSampleRate: cfg.EvalSampleRate,
			Workers:         cfg.EvalWorkers,
		}, log)
		recorders = append(recorders, evalWorker)
		log.Info("eval worker enabled (async quality evaluation)")
	} else {
		log.Info("clickhouse not configured; eval worker disabled")
	}

	recorder := observability.NewMultiRecorder(recorders...)

	// Gateway server.
	gwHandler := gateway.NewHandler(reg, recorder, lim, log)

	// Inline guardrails (hot path): block disallowed prompts before the upstream
	// call and optionally redact PII from responses.
	if guard := guardrails.New(guardrails.Config{
		Enabled:            cfg.GuardrailsEnabled,
		BlockPIIInput:      cfg.GuardrailBlockPIIIn,
		RedactPIIOutput:    cfg.GuardrailRedactPIIOut,
		MaxInputChars:      cfg.GuardrailMaxInputChrs,
		DenyPatterns:       splitDenyPatterns(cfg.GuardrailDenyPatterns),
		ValidateJSONOutput: cfg.GuardrailValidateJSON,
	}); guard.Active() {
		gwHandler.SetGuard(guard)
		log.Info("inline guardrails enabled",
			"block_pii_input", cfg.GuardrailBlockPIIIn,
			"redact_pii_output", cfg.GuardrailRedactPIIOut,
			"max_input_chars", cfg.GuardrailMaxInputChrs,
			"deny_patterns", len(splitDenyPatterns(cfg.GuardrailDenyPatterns)),
			"validate_json_output", cfg.GuardrailValidateJSON)
	} else {
		log.Info("inline guardrails disabled (set NEXUS_GUARDRAILS_ENABLED=true)")
	}

	// Structured-output self-correction: retry rejected JSON responses with a
	// correction prompt before failing. Requires the schema guardrail to supply
	// the rejection signal (NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT=true).
	if cfg.SelfCorrectionEnabled && cfg.SelfCorrectionMaxRetries > 0 {
		gwHandler.SetSelfCorrection(cfg.SelfCorrectionMaxRetries)
		log.Info("structured-output self-correction enabled", "max_retries", cfg.SelfCorrectionMaxRetries)
	}

	// Quality-aware routing (Phase 4): blend rolling eval quality with cost and
	// latency to pick the best model for routing aliases ("auto" or groups).
	var modelRouter *router.Router
	if chRec != nil {
		modelRouter = router.New(
			router.NewCHStatsProvider(chRec.Conn()),
			router.Weights{Quality: cfg.RouteWQuality, Cost: cfg.RouteWCost, Latency: cfg.RouteWLatency},
			cfg.RouteWindow, cfg.RouteRefresh, log,
		)
		groups := parseRouteGroups(cfg.RouteGroups)
		gwHandler.SetRouter(modelRouter, groups)
		if cfg.RouteLoadBalance {
			gwHandler.SetLoadBalancing(balancer.NewWeightedRR())
			log.Info("route load balancing enabled (rank-weighted round-robin within quality-qualified tiers)")
		}
		log.Info("quality-aware routing enabled", "groups", len(groups), "alias", "auto")
	} else {
		log.Info("clickhouse not configured; quality-aware routing disabled")
	}

	// Semantic cache: Redis-backed, embedding-similarity response cache.
	var semCacheRedis *semcache.Redis
	if cfg.SemanticCacheEnabled {
		if cfg.RedisURL == "" {
			log.Warn("semantic cache requires NEXUS_REDIS_URL")
		} else if cfg.EmbeddingsURL == "" {
			log.Warn("semantic cache requires NEXUS_EMBEDDINGS_URL")
		} else {
			scfg := semcache.Config{
				Enabled:            true,
				TTL:                cfg.SemanticCacheTTL,
				Threshold:          cfg.SemanticCacheThreshold,
				MaxEntriesPerModel: cfg.SemanticCacheMaxEntries,
			}
			embedder := semcache.NewOpenAIEmbedder(
				cfg.EmbeddingsURL, cfg.EmbeddingsModel, cfg.EmbeddingsAPIKey, cfg.EmbeddingsTimeout,
			)
			scr, err := semcache.NewRedis(ctx, cfg.RedisURL, embedder, scfg)
			if err != nil {
				log.Error("semantic cache init failed", "err", err)
			} else {
				semCacheRedis = scr
				if svc := semcache.NewService(scr, embedder, scfg); svc != nil {
					gwHandler.SetSemanticCache(svc)
					log.Info("semantic cache enabled", "config", svc.ConfigString(), "embeddings", cfg.EmbeddingsModel)
				}
			}
		}
	}

	// BYOK: per-request upstream key resolution. In "byok"/"strict_byok" mode
	// each caller's request uses their own stored provider key (falling back to
	// shared keys in soft BYOK). A short-TTL cache avoids re-decrypting per call.
	keyMode := gateway.ParseKeyMode(cfg.KeyMode)
	var credResolver *gateway.CredentialResolver
	if keyMode != gateway.KeyModeShared {
		if store == nil || !store.HasCipher() {
			log.Warn("NEXUS_KEY_MODE requires Postgres + NEXUS_MASTER_KEY; falling back to shared keys", "mode", cfg.KeyMode)
			keyMode = gateway.KeyModeShared
		} else {
			credResolver = gateway.NewCredentialResolver(&storeCredentialSource{st: store}, 60*time.Second)
			gwHandler.SetCredentialResolution(credResolver, keyMode)
			log.Info("per-request credential resolution enabled (BYOK)", "mode", cfg.KeyMode)
		}
	}

	gwSrv := &http.Server{
		Addr:    cfg.GatewayAddr,
		Handler: gateway.NewMux(gwHandler, auth, lim, log),
	}

	// Console server.
	consoleSrvHandler := console.NewServer(hub, reader, store, log)
	consoleSrvHandler.SetAllowSignup(cfg.AllowSignup)
	if modelRouter != nil {
		consoleSrvHandler.SetRouteStats(modelRouter)
	}
	// Hot-reload providers after credential changes (e.g. rotation) so a new
	// secret takes effect without restarting the gateway.
	if store != nil && store.HasCipher() {
		consoleSrvHandler.SetCredentialReloader(func(rctx context.Context) {
			registerStoredCredentials(rctx, reg, store, cfg, log)
			credResolver.Invalidate() // safe on nil; clears per-user key cache
		})
	}
	consoleSrv := &http.Server{
		Addr:    cfg.ConsoleAddr,
		Handler: consoleSrvHandler.Mux(),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go serve(&wg, gwSrv, "gateway", cfg.GatewayAddr, log)
	go serve(&wg, consoleSrv, "console", cfg.ConsoleAddr, log)

	<-ctx.Done()
	log.Info("shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = gwSrv.Shutdown(shutCtx)
	_ = consoleSrv.Shutdown(shutCtx)
	if modelRouter != nil {
		modelRouter.Close()
	}
	if evalWorker != nil {
		_ = evalWorker.Close(shutCtx)
	}
	if chRec != nil {
		_ = chRec.Close(shutCtx)
	}
	if store != nil {
		store.Close()
	}
	if redisLim != nil {
		_ = redisLim.Close()
	}
	if semCacheRedis != nil {
		_ = semCacheRedis.Close()
	}
	wg.Wait()
}

// splitDenyPatterns parses a semicolon-separated list of regex patterns,
// trimming whitespace and dropping empty entries.
func splitDenyPatterns(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitCSV parses a comma-separated list, trimming whitespace and dropping
// empty entries.
func splitCSV(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseRouteGroups parses a spec like
// "fast=gpt-4o-mini,gemini-2.5-flash;smart=gpt-4o,claude-3-5-sonnet" into a map
// of alias -> candidate models. Malformed entries are skipped.
func parseRouteGroups(spec string) map[string][]string {
	groups := map[string][]string{}
	for _, group := range strings.Split(spec, ";") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		alias, list, ok := strings.Cut(group, "=")
		alias = strings.TrimSpace(alias)
		if !ok || alias == "" {
			continue
		}
		var models []string
		for _, m := range strings.Split(list, ",") {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		if len(models) > 0 {
			groups[alias] = models
		}
	}
	return groups
}

// makeAuthenticator adapts the store into a gateway virtual-key authenticator.
func makeAuthenticator(st *core.Store) gateway.VKeyAuthenticator {
	return func(ctx context.Context, plaintext string) (gateway.AuthResult, error) {
		vk, err := st.LookupVirtualKey(ctx, plaintext)
		if err != nil {
			return gateway.AuthResult{}, err
		}
		res := gateway.AuthResult{
			OrgID:         vk.OrgID,
			UserID:        vk.UserID,
			VKeyID:        vk.ID,
			AllowedModels: vk.AllowedModels,
			RPMLimit:      vk.RPMLimit,
			MonthlyBudget: vk.MonthlyBudget,
			MinQuality:    vk.MinQuality,
		}
		// When the owning user has turned off Nexus-side enforcement (BYOK),
		// drop the RPM/budget caps so only the provider's own limits apply.
		if !vk.EnforceLimits {
			res.RPMLimit = 0
			res.MonthlyBudget = 0
		}
		return res, nil
	}
}

// bootstrapAdmin creates an initial admin user from NEXUS_ADMIN_EMAIL /
// NEXUS_ADMIN_PASSWORD when the org has no users yet, so the console has a first
// login. No-op when the env vars are unset or users already exist.
func bootstrapAdmin(ctx context.Context, st *core.Store, cfg config.Config, log *slog.Logger) {
	if cfg.AdminEmail == "" || cfg.AdminPassword == "" {
		return
	}
	n, err := st.CountUsers(ctx, "default")
	if err != nil {
		log.Error("bootstrap admin: count users failed", "err", err)
		return
	}
	if n > 0 {
		return
	}
	if _, err := st.CreateUser(ctx, "default", cfg.AdminEmail, cfg.AdminPassword, core.RoleAdmin); err != nil {
		log.Error("bootstrap admin failed", "err", err)
		return
	}
	log.Info("bootstrap admin user created", "email", cfg.AdminEmail)
}

// storeCredentialSource adapts the control-plane store to the gateway's
// CredentialSource interface, translating core types to gateway types so the
// gateway package stays decoupled from core.
type storeCredentialSource struct{ st *core.Store }

func (s *storeCredentialSource) ResolveCredential(ctx context.Context, orgID, userID, provider string) (gateway.ResolvedCredential, bool, error) {
	cred, source, err := s.st.ResolveCredential(ctx, orgID, userID, provider)
	if errors.Is(err, core.ErrNotFound) {
		return gateway.ResolvedCredential{}, false, nil
	}
	if err != nil {
		return gateway.ResolvedCredential{}, false, err
	}
	return gateway.ResolvedCredential{
		Secret:  cred.Secret,
		BaseURL: cred.BaseURL,
		Source:  source,
		ID:      cred.ID,
	}, true, nil
}

// registerStoredCredentials registers providers from encrypted DB credentials.
// Env-configured providers already registered take precedence are not
// overwritten unless absent.
func registerStoredCredentials(ctx context.Context, reg *gateway.Registry, st *core.Store, cfg config.Config, log *slog.Logger) {
	if !st.HasCipher() {
		log.Warn("provider credentials in DB skipped: NEXUS_MASTER_KEY not set")
		return
	}
	creds, err := st.LoadEnabledCredentials(ctx, "default")
	if err != nil {
		log.Error("load stored credentials failed", "err", err)
		return
	}
	for _, c := range creds {
		switch c.Provider {
		case "openai":
			base := c.BaseURL
			if base == "" {
				base = cfg.OpenAIBaseURL
			}
			reg.Register(providers.NewOpenAI(c.Secret, base, cfg.UpstreamTimeout))
		case "anthropic":
			reg.Register(providers.NewAnthropic(c.Secret, cfg.UpstreamTimeout))
		case "gemini":
			reg.Register(providers.NewGemini(c.Secret, cfg.UpstreamTimeout))
		default:
			log.Warn("unknown provider in credential store", "provider", c.Provider)
			continue
		}
		log.Info("provider registered from credential store", "name", c.Provider, "last4", c.SecretLast4)
	}
}

func serve(wg *sync.WaitGroup, srv *http.Server, name, addr string, log *slog.Logger) {
	defer wg.Done()
	log.Info("listening", "service", name, "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "service", name, "err", err)
	}
}

func registerProviders(reg *gateway.Registry, cfg config.Config, log *slog.Logger) {
	if cfg.OpenAIAPIKey != "" {
		reg.Register(providers.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.UpstreamTimeout))
		log.Info("provider registered", "name", "openai")
	}
	if cfg.AnthropicAPIKey != "" {
		reg.Register(providers.NewAnthropic(cfg.AnthropicAPIKey, cfg.UpstreamTimeout))
		log.Info("provider registered", "name", "anthropic")
	}
	if cfg.GeminiAPIKey != "" {
		reg.Register(providers.NewGemini(cfg.GeminiAPIKey, cfg.UpstreamTimeout))
		log.Info("provider registered", "name", "gemini")
	}
}
