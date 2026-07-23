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
				"migrations/postgres/004_audit_index.sql",
				"migrations/postgres/005_credential_models.sql",
				"migrations/postgres/006_eval_scores.sql",
				"migrations/postgres/007_eval_scores_model.sql",
				"migrations/postgres/008_onboarded_at.sql",
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

	// Optional background sync: keep /v1/models in lock-step with each
	// provider's live catalog so day-zero model launches do not require
	// a redeploy. The worker is per-provider and runs outside the hot
	// path; Registry.UpdateModels is the only lock taken on the request
	// goroutine side, and it holds the write lock only for the duration
	// of a slice copy.
	if cfg.DynamicModelSync {
		startDynamicSyncWorkers(ctx, reg, cfg, log)
	}

	// Observability: ClickHouse persistence (optional) + live dashboard hub.
	hub := console.NewHub()
	var chRec *observability.CHRecorder

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
				"migrations/clickhouse/006_replica_id.sql",
			} {
				schema, _ := nexus.Migrations.ReadFile(path)
				if err := rec.Migrate(ctx, string(schema)); err != nil {
					log.Error("clickhouse migrate failed", "file", path, "err", err)
				}
			}
			chRec = rec
			log.Info("clickhouse trace persistence enabled")
		}
	} else {
		log.Info("clickhouse not configured; traces are live-only (set NEXUS_CLICKHOUSE_URL to persist)")
	}

	stack := buildStack(cfg, hub, chRec, store, log)
	recorder := stack.Recorder
	reader := stack.Reader
	evalWorker := stack.EvalWorker
	modelRouter := stack.ModelRouter

	// Gateway server.
	gwHandler := gateway.NewHandler(reg, recorder, lim, log)
	gwHandler.SetReplicaID(cfg.ReplicaID)

	// --- V4 failover alert sinks ----------------------------------------
	// Wire a multi-sink notifier only when at least one URL is set so
	// the gateway doesn't pay for the worker goroutine in the common,
	// metrics-only case. Each URL is independently opt-in; both can
	// coexist (one webhook for the in-house alerting pipeline plus a
	// Slack channel for the team's awareness).
	var failoverSinks []router.Notifier
	if cfg.FailoverWebhookURL != "" {
		failoverSinks = append(failoverSinks, router.NewWebhookNotifier(cfg.FailoverWebhookURL, log))
	}
	if cfg.FailoverSlackURL != "" {
		failoverSinks = append(failoverSinks, router.NewSlackNotifier(cfg.FailoverSlackURL, log))
	}
	if mn := router.NewMultiNotifier(failoverSinks...); mn != nil {
		gwHandler.SetFailoverNotifier(mn)
		log.Info("failover alert sinks enabled",
			"webhook", cfg.FailoverWebhookURL != "",
			"slack", cfg.FailoverSlackURL != "",
			"cooldown", cfg.FailoverAlertCooldown)
	}

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

	// Quality-aware routing (Phase 4): attached when ClickHouse stats are available.
	if modelRouter != nil {
		groups := config.ParseRouteGroups(cfg.RouteGroups)
		gwHandler.SetRouter(modelRouter, groups)
		if cfg.RouteLoadBalance {
			gwHandler.SetLoadBalancing(balancer.NewWeightedRR())
			log.Info("route load balancing enabled (rank-weighted round-robin within quality-qualified tiers)")
		}
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

	// BYOK: per-request upstream key resolution. Default mode is strict_byok
	// (each caller must own a provider key for the target provider; the
	// operator never pays for user usage). Setting NEXUS_KEY_MODE=byok or
	// =shared softens this, and NEXUS_ALLOW_SHARED_KEYS=true additionally
	// re-enables env-key registration via registerProviders.
	keyMode := gateway.ParseKeyMode(cfg.KeyMode)
	var credResolver *gateway.CredentialResolver
	if keyMode != gateway.KeyModeShared {
		if store == nil || !store.HasCipher() {
			if cfg.AllowSharedKeys {
				log.Warn("NEXUS_KEY_MODE requires Postgres + NEXUS_MASTER_KEY; falling back to shared keys (NEXUS_ALLOW_SHARED_KEYS=true)",
					"mode", cfg.KeyMode)
				keyMode = gateway.KeyModeShared
			} else {
				log.Warn("NEXUS_KEY_MODE requires Postgres + NEXUS_MASTER_KEY; strict-byok disabled until storage configured",
					"mode", cfg.KeyMode)
				keyMode = gateway.KeyModeShared
			}
		} else {
			credResolver = gateway.NewCredentialResolver(&storeCredentialSource{st: store}, 60*time.Second)
			gwHandler.SetCredentialResolution(credResolver, keyMode)
			log.Info("per-request credential resolution enabled (BYOK)", "mode", cfg.KeyMode)
		}
	}

	gwSrv := &http.Server{
		Addr: cfg.GatewayAddr,
		// V5 per-vkey concurrency cap. nil -> disabled (zero-dep mode).
		Handler: gateway.NewMux(gwHandler, auth, lim, limiter.NewConcurrencyCap(cfg.MaxConcurrentPerKey), log),
	}

	// Console server.
	consoleSrvHandler := console.NewServer(hub, reader, store, log)
	consoleSrvHandler.SetAllowSignup(cfg.AllowSignup)
	consoleSrvHandler.SetGatewayProxy(cfg.GatewayAddr)
	consoleSrvHandler.SetPublicGatewayURL(cfg.PublicGatewayURL)
	// OIDC SSO: discovery runs against cfg.SSO.Issuer at boot. Failures
	// here only log a warning; the console still serves password login
	// and the SSO routes simply return 404.
	consoleSrvHandler.SetSSO(ctx, cfg.SSO)
	if modelRouter != nil {
		consoleSrvHandler.SetRouteStats(modelRouter)
	}
	consoleSrvHandler.SetCatalog(gwHandler.Catalog())
	if evalWorker != nil {
		erc := newEvalRuntimeController(cfg, evalWorker, modelRouter, gwHandler, stack.ScoreStore, stack.TraceStore, stack.RoutingStatsStore)
		consoleSrvHandler.SetEvalConfig(erc, erc)
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

	// --- Metabase BI adapter (one-shot, idempotent, never gating boot) -----
	// Mirrors the V3 OTLP contract: empty URL => constructor returns nil =>
	// MultiBootstrapper skips it. We register on a Multi so future BI tools
	// (Redash, Superset) can share the same boot slot without touching
	// main.go's command shape.
	if mbBoot := observability.NewMetabaseBootstrapper(observability.MetabaseConfig{
		URL:            cfg.MetabaseURL,
		User:           cfg.MetabaseUser,
		Password:       cfg.MetabasePassword,
		ClickHouseHTTP: cfg.MetabaseClickHouseURL,
		PostgresJDBC:   cfg.MetabasePostgresURL,
		SeedDir:        cfg.MetabaseSeedDir,
		HealthTimeout:  cfg.MetabaseHealthTimeout,
		RequestTimeout: cfg.MetabaseRequestTimeout,
	}, log); mbBoot != nil {
		mbMulti := observability.NewMultiBootstrapper(mbBoot)
		mbMulti.SetLogger(log)
		bootCtx, bootCancel := context.WithTimeout(context.Background(),
			cfg.MetabaseHealthTimeout+10*time.Second)
		if err := mbMulti.Bootstrap(bootCtx); err != nil {
			log.Warn("metabase bootstrap encountered issues (continuing)", "err", err)
		} else {
			log.Info("metabase bootstrap ok", "names", mbMulti.Names())
		}
		bootCancel()
	}

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
	// Bootstrap is a system action (no caller); empty actorID => audit_log stores
	// "system" for the resulting user.create entry.
	if _, err := st.CreateUser(ctx, "default", "", cfg.AdminEmail, cfg.AdminPassword, core.RoleAdmin); err != nil {
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
	// Console stores The Grid as "the_grid"; the gateway adapter is "grid".
	if errors.Is(err, core.ErrNotFound) && provider == "grid" {
		cred, source, err = s.st.ResolveCredential(ctx, orgID, userID, "the_grid")
	}
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
		case "groq":
			reg.Register(providers.NewGroq(c.Secret, cfg.UpstreamTimeout))
		case "mistral":
			reg.Register(providers.NewMistral(c.Secret, cfg.UpstreamTimeout))
		case "grid", "the_grid":
			reg.Register(providers.NewGrid(c.Secret, cfg.UpstreamTimeout))
		default:
			// Dynamic OpenAI-compatible credential: any owner-supplied provider
			// name falls through here. The base URL is required so the gateway
			// knows where to forward calls; only OpenAI-shaped wire formats are
			// supported, so we wrap with the OpenAICompat adapter.
			//
			// Model ids are namespaced as "user/<provider>/<model>" in the
			// registry so they cannot collide with built-in catalog ids; clients
			// call the gateway with the prefix (or the short-form
			// "<provider>/<model>" which Resolver already knows to cut on the
			// first "/").
			if c.BaseURL == "" {
				log.Warn("user-defined credential skipped: base_url required for non-builtin providers",
					"provider", c.Provider, "last4", c.SecretLast4)
				continue
			}
			// Inner adapter uses the raw model ids the owner registered; the
			// UserCompat wrapper exposes them under "user/<provider>/<model>"
			// through Models()/EmbeddingModels() so callers do not collide
			// with the built-in catalog id space at /v1/models.
			compat := providers.NewOpenAICompat(c.Provider, c.Secret, c.BaseURL,
				c.Models.Chat, c.Models.Embed, nil, nil, cfg.UpstreamTimeout)
			reg.Register(providers.NewUserCompat(compat))
			log.Info("dynamic compat provider registered", "name", c.Provider, "last4", c.SecretLast4,
				"chat_models", len(c.Models.Chat), "embed_models", len(c.Models.Embed))
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

// registerBuiltinCatalog registers every first-party adapter with an empty
// operator key so /v1/models and model routing work under strict BYOK. Per-
// request ResolveCredential injects each caller's stored secret at call time.
func registerBuiltinCatalog(reg *gateway.Registry, cfg config.Config, log *slog.Logger) {
	reg.Register(providers.NewOpenAI("", cfg.OpenAIBaseURL, cfg.UpstreamTimeout))
	reg.Register(providers.NewAnthropic("", cfg.UpstreamTimeout))
	reg.Register(providers.NewGemini("", cfg.UpstreamTimeout))
	reg.Register(providers.NewGroq("", cfg.UpstreamTimeout))
	reg.Register(providers.NewMistral("", cfg.UpstreamTimeout))
	reg.Register(providers.NewGrid("", cfg.UpstreamTimeout))
	log.Info("builtin provider catalogs registered for BYOK routing")
}

func registerProviders(reg *gateway.Registry, cfg config.Config, log *slog.Logger) {
	// Env-configured providers are only useful when the operator has opted in
	// to shared-key fallback (NEXUS_ALLOW_SHARED_KEYS=true) or when the
	// gateway is running in KeyModeShared. In v0.1.0 the default is
	// strict_byok and AllowSharedKeys is false, so the env keys below are
	// loaded into the struct for visibility but never reach the Registry.
	// We log a single warn line so operators can see exactly which env keys
	// are present but unused; setting NEXUS_ALLOW_SHARED_KEYS=true re-enables
	// registration. Catalog stubs (empty operator key) are still registered so
	// BYOK users can call any built-in model once they store a personal key.
	mode := gateway.ParseKeyMode(cfg.KeyMode)
	strictBYOK := mode != gateway.KeyModeShared && !cfg.AllowSharedKeys

	if strictBYOK {
		for _, name := range []string{"openai", "anthropic", "gemini", "groq", "mistral", "grid"} {
			if envKeySet(name, cfg) {
				log.Warn("env provider key present but unused under strict-byok default",
					"provider", name,
					"opt_in", "set NEXUS_ALLOW_SHARED_KEYS=true to enable shared fallback")
			}
		}
		registerBuiltinCatalog(reg, cfg, log)
		return
	}

	// Shared-key / escape-hatch mode: register the full catalog first so BYOK
	// users can still reach every built-in model, then overlay any operator env
	// keys on top of the matching provider adapter.
	registerBuiltinCatalog(reg, cfg, log)
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
	if cfg.GroqAPIKey != "" {
		reg.Register(providers.NewGroq(cfg.GroqAPIKey, cfg.UpstreamTimeout))
		log.Info("provider registered", "name", "groq")
	}
	if cfg.MistralAPIKey != "" {
		reg.Register(providers.NewMistral(cfg.MistralAPIKey, cfg.UpstreamTimeout))
		log.Info("provider registered", "name", "mistral")
	}
	if cfg.GridAPIKey != "" {
		reg.Register(providers.NewGrid(cfg.GridAPIKey, cfg.UpstreamTimeout))
		log.Info("provider registered", "name", "grid")
	}
}

// envKeySet reports whether the named provider has a non-empty env-configured
// key in the active config.
func envKeySet(name string, cfg config.Config) bool {
	switch name {
	case "openai":
		return cfg.OpenAIAPIKey != ""
	case "anthropic":
		return cfg.AnthropicAPIKey != ""
	case "gemini":
		return cfg.GeminiAPIKey != ""
	case "groq":
		return cfg.GroqAPIKey != ""
	case "mistral":
		return cfg.MistralAPIKey != ""
	case "grid":
		return cfg.GridAPIKey != ""
	default:
		return false
	}
}

// dynamicSyncRegistry owns the per-provider counters so /metrics (when
// enabled) can fold them into the existing Prometheus scrape. Defined as
// a package-level var so the binary LinkName keeps it out of the hot path
// entirely: when DynamicModelSync=false the registry is never allocated.
var dynamicSyncRegistry = gateway.NewDynamicSyncRegistry()

// dynamicSyncSpec binds a provider name to its fetcher, ordered for the
// boot log. Order matches the priority list operators see in the docs so a
// misconfigured provider shows up in a familiar position.
func startDynamicSyncWorkers(ctx context.Context, reg *gateway.Registry, cfg config.Config, log *slog.Logger) {
	type spec struct {
		name    string
		fetcher gateway.ModelFetcher
	}
	specs := []spec{
		{
			name:    "openai",
			fetcher: gateway.NewOpenAIModelFetcher(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.UpstreamTimeout),
		},
		{
			name:    "anthropic",
			fetcher: gateway.NewAnthropicModelFetcher(cfg.AnthropicAPIKey, "https://api.anthropic.com/v1", cfg.UpstreamTimeout),
		},
		{
			name:    "gemini",
			fetcher: gateway.NewGeminiModelFetcher(cfg.GeminiAPIKey, "https://generativelanguage.googleapis.com/v1beta", cfg.UpstreamTimeout),
		},
	}
	for _, s := range specs {
		if _, ok := reg.ProviderFor(s.name); !ok {
			// Only enabled providers (env key present and key mode allows
			// shared fallback) get a worker; silent skip on others keeps
			// the boot log quiet for the common case where one provider
			// is configured.
			continue
		}
		dp := gateway.NewDynamicProvider(s.name)
		counters := &gateway.DynamicSyncCounters{}
		dynamicSyncRegistry.Register(s.name, dp, counters)
		gateway.StartDynamicSync(ctx, reg, dp, s.fetcher, cfg.DynamicModelInterval, cfg.DynamicModelMaxRetry, counters, log)
		log.Info("dynamic model sync enabled",
			"provider", s.name,
			"interval", cfg.DynamicModelInterval,
			"max_retry", cfg.DynamicModelMaxRetry)
	}
}
