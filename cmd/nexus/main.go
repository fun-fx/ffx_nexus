// Command nexus is the single-binary LLM gateway: an OpenAI-compatible proxy
// with built-in observability and a live dashboard API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ffxnexus/nexus/internal/balancer"
	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/ffxnexus/nexus/internal/cron"
	docsserver "github.com/ffxnexus/nexus/internal/docs"
	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
	"github.com/ffxnexus/nexus/internal/evaluators/heuristic"
	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/gateway/providers"
	"github.com/ffxnexus/nexus/internal/guardrails"
	"github.com/ffxnexus/nexus/internal/health"
	"github.com/ffxnexus/nexus/internal/leaser"
	"github.com/ffxnexus/nexus/internal/limiter"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
	"github.com/ffxnexus/nexus/internal/semcache"
)

// nexusBuildTag is the build identity stamped into the X-Nexus-Build
// response header so operators can verify the running binary matches
// what their source-control UI surfaces. The CD pipeline overrides
// this via `-ldflags "-X main.nexusBuildTag=<commit-sha>"`; locally
// built binaries keep the "dev" placeholder.
var nexusBuildTag = "dev"

// heuristicLocalEvaluator is the bridge between
// external.LocalEvaluator and internal/evaluators/heuristic. Every
// metric behind it is pure Go and runs on the worker goroutine, so a
// heuristic plugin costs no egress and no hosted compute — that is
// the property that lets it coexist with config-only evals.
//
// The HuggingFace Evaluate, LightEval and Ragas metrics used to be
// routed here through a Python subprocess. They were removed: those
// libraries need eval compute inside the Nexus pod, which is the
// dependency the plugin model exists to avoid. Use an external plugin
// for those metrics instead.
type heuristicLocalEvaluator struct{}

func (heuristicLocalEvaluator) Evaluate(
	ctx context.Context,
	metricName string,
	args map[string]any,
	t observability.Trace,
) ([]evals.Score, error) {
	switch metricName {
	case "contains", "pii", "exact_match", "rouge_l":
		return heuristic.Evaluate(ctx, metricName, args, t)
	}
	return nil, fmt.Errorf("heuristic metric %q is not registered", metricName)
}

func main() {
	// Subcommand dispatch. `nexus` with no arguments runs the server, which
	// keeps every existing image ENTRYPOINT working unchanged; `nexus migrate`
	// applies schema changes and exits, which is what the Helm pre-upgrade hook
	// Job runs so a failed migration stops the rollout instead of surfacing as
	// user-visible 500s afterwards.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "migrate":
			os.Exit(runMigrateCommand(os.Args[2:]))
		case "serve":
			// Explicit spelling of the default, so a Kubernetes manifest can
			// state its intent rather than relying on an empty args list.
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q; known commands: serve, migrate\n", os.Args[1])
			os.Exit(2)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	// Readiness gate. Conditions are registered below as each dependency is
	// resolved; an unmigrated schema or a missing required datastore withholds
	// traffic rather than serving errors. Liveness deliberately stays a plain
	// "the process is up" check, because restarting a pod does not apply a
	// migration and a dependency blip must not cause a restart storm.
	ready := health.New()

	// Outbound HTTP policy, installed before anything that can make a request.
	//
	// The order matters: components built later in this function capture their
	// HTTP client at construction time, so a guard installed after them would
	// leave those paths on the strict fallback policy and quietly break an
	// operator's in-cluster collector.
	installEgressGuard(cfg, log)

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
			// Schema handling. Migrations are no longer a hardcoded list run on
			// every boot: they are discovered from the embedded filesystem,
			// ordered, recorded in a ledger, and by default applied by a
			// separate `nexus migrate` step (the Helm pre-upgrade hook Job) so a
			// failure stops the rollout. At boot we only VERIFY, and withhold
			// readiness when the schema is behind the binary — the previous
			// behaviour logged the error and served traffic anyway.
			applySchemaAtBoot(ctx, st.Pool(), cfg, ready, log)

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
			// Same treatment as Postgres. Note the ClickHouse list previously
			// omitted the benchmark_runs migration entirely (it collided on
			// ordinal 007 with 007_session_id.sql), so benchmark history was
			// never persisted; discovery-based loading fixes that class of bug
			// by construction.
			applyClickHouseSchemaAtBoot(ctx, rec.Conn(), cfg, ready, log)

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
credResolver = gateway.NewCredentialResolver(&storeCredentialSource{st: store}, 60*time.Second, cfg.EgressTenantAllowedCIDRs)
			gwHandler.SetCredentialResolution(credResolver, keyMode)
			log.Info("per-request credential resolution enabled (BYOK)", "mode", cfg.KeyMode)
		}
	}

	gwSrv := &http.Server{
		Addr: cfg.GatewayAddr,
		// V5 per-vkey concurrency cap. nil -> disabled (zero-dep mode).
		Handler: gateway.NewMux(gwHandler, auth, lim, limiter.NewConcurrencyCap(cfg.MaxConcurrentPerKey), ready, log),
	}

	// Console server.
	consoleSrvHandler := console.NewServer(hub, reader, store, log)
	// Stamp every JSON response from this binary with the build
	// identity passed by the build pipeline. Operators inspect
	// the X-Nexus-Build response header in their browser devtools
	// when an upstream proxy rewrites the body — missing header
	// means the gateway/CDN is replacing Nexus's response.
	consoleSrvHandler.SetBuildTag(nexusBuildTag)
	consoleSrvHandler.SetAllowSignup(cfg.AllowSignup)
	consoleSrvHandler.SetPublicDocs(cfg.PublicDocs)

	// Browser-security wiring, deliberately unconditional.
	//
	// SetCSPOrigins is also called further down, but only inside the block that
	// runs when the eval runtime is wired. The same list now drives the CORS
	// allowlist, the state-changing Origin check and the WebSocket upgrade
	// check, so leaving it behind that condition would mean a deployment with
	// evals disabled silently fell back to same-origin-only and the operator's
	// separate console origin stopped working — a confusing failure with no
	// obvious link to NEXUS_EVAL_ENABLED. Order matters: dev mode first, since
	// it changes how the origin list is interpreted.
	consoleSrvHandler.SetDevMode(cfg.DevMode)
	consoleSrvHandler.SetCSPOrigins(cfg.PublicWebOrigins)
	consoleSrvHandler.SetSecureCookies(cfg.SecureCookies)

	if cfg.DevMode {
		log.Warn("development mode is ON — browser protections are relaxed",
			"reason", "NEXUS_DEV_MODE=true",
			"effect", "loopback HTTP origins accepted; session cookies not marked Secure",
			"action", "unset NEXUS_DEV_MODE for any deployment reachable by a browser you do not control")
	}
	if !cfg.SecureCookies && !cfg.DevMode {
		log.Warn("session cookies are NOT marked Secure",
			"reason", "NEXUS_SECURE_COOKIES=false",
			"effect", "a browser will send a live console session over plain HTTP")
	}
	if len(cfg.PublicWebOrigins) == 0 {
		// Not an error: a console served from the same origin as its API needs
		// no allowlist at all, which is the common single-host deployment.
		log.Info("no cross-origin web origins configured; console accepts same-origin browser requests only",
			"set", "NEXUS_PUBLIC_WEB_ORIGINS to permit a console served from another origin")
	}
	if cfg.PublicDocs {
		// Logged so an unauthenticated docs tree is always traceable to a
		// deliberate configuration change rather than discovered during a
		// customer's penetration test.
		log.Warn("serving /api/docs without authentication",
			"reason", "NEXUS_PUBLIC_DOCS=true",
			"note", "the docs bundle enumerates this installation's endpoints and configuration flags")
	}
	consoleSrvHandler.SetGatewayProxy(cfg.GatewayAddr)
	consoleSrvHandler.SetReadiness(ready)
	consoleSrvHandler.SetPublicGatewayURL(cfg.PublicGatewayURL)
	// Link-only Grafana wiring: composes the deep links served by
	// GET /api/ui/observability. No Grafana client, no health check, no
	// server-side request — so Grafana being down or misconfigured can
	// never reach the gateway's request path.
	consoleSrvHandler.SetPublicGrafanaURL(cfg.PublicGrafanaURL)
	// PublicBaseURL is what the admin-facing invite URL is rooted
	// on (it lives next to the console). EmailPublicBaseURL is the
	// host embedded inside the outgoing email body — operators
	// flip them apart when the public envelope host differs from
	// the operator-facing console host (e.g. nexus.ffx.ai
	// console vs api.ffx.ai accept endpoint).
	consoleSrvHandler.SetPublicBaseURL(publicBaseForInvites(cfg))
	if cfg.EmailEnabled() {
		consoleSrvHandler.SetResendClient(
			console.NewResendClient(cfg.ResendAPIKey, cfg.EmailEnvelope(), cfg.ResendRequestTimeout))
	}
	// OIDC SSO: discovery runs against cfg.SSO.Issuer at boot. Failures
	// here only log a warning; the console still serves password login
	// and the SSO routes simply return 404.
	consoleSrvHandler.SetSSO(ctx, cfg.SSO)
	if modelRouter != nil {
		consoleSrvHandler.SetRouteStats(modelRouter)
		// Read the runtime router's bench-blend state so the
		// operator's quality view never drifts from what the
		// gateway actually does on each decision. The console's
		// QualityRouterQuerier is the narrow contract; the
		// adapter in cmd/nexus/quality_querier.go bridges it.
		consoleSrvHandler.SetQualityRouter(NewRouterQualityQuerier(modelRouter))
	}
	consoleSrvHandler.SetCatalog(gwHandler.Catalog())
	if evalWorker != nil {
		// PR #136: profile store + secret resolver. We always wire a
		// profile store (in-memory when no Postgres / ClickHouse) so
		// the CRUD endpoints answer 200 with an empty list when the
		// deployment doesn't persist profiles. The resolver maps to
		// core.Store when Postgres is up; otherwise nil (heuristics
		// and the env-var default profiles still run).
		var profileStore evals.ProfileStore = evals.NewMemoryStore(nil)
		var resolver *evals.Resolver
		// A plugin install must outlive the pod that accepted it, so
		// the plugin store is Postgres wherever a control plane exists
		// and only degrades to memory in the zero-dependency build.
		var pluginStore evalplugin.PluginStore
		var keyVault pluginKeyVault
		if store != nil {
			profileStore = newProfileStoreFromCore(store)
			resolver = evals.NewResolver(evals.NewStoreSecretLookup(store, "default"))
			pluginStore = evalplugin.NewPostgresStore(store.Pool())
			keyVault = store
		}
		erc := newEvalRuntimeController(cfg, evalWorker, modelRouter, gwHandler, stack.ScoreStore, stack.TraceStore, stack.RoutingStatsStore, profileStore, resolver, pluginStore)
		consoleSrvHandler.SetEvalConfig(erc, erc)
		erc.SeedProfilesFromConfig(ctx)
		// CSP allow-list: comma-separated web origins the console may connect to
		// (marketing site, separate live-trace wss host, etc.). Encoded into the
		// CSP `connect-src` directive by securityHeaders, replacing the prior
		// `https://*.ffx.ai` hardcode so multi-tenant Helm deploys do not need
		// a code change to permit their own origin.
		consoleSrvHandler.SetCSPOrigins(cfg.PublicWebOrigins)
		if resolver != nil {
			evalWorker.SetSecretResolver(resolver.Resolve)
		}
		consoleSrvHandler.SetEvalProfiles(erc)

		// Bind the docs root once the rest of the console is wired.
		// cfg.DocsDir is empty by default; the docs package falls back
		// to ./docs relative to the binary (which matches `go run`).
		// Any failure surfaces here loudest: missing directory on a
		// cluster deploy shows up in pod logs at boot rather than as
		// a blank page that looks identical to a healthy /docs view.
		docsBound := false
		if cfg.DocsDir != "" {
			if err := docsserver.SetSourceDir(cfg.DocsDir); err != nil {
				slog.Error("docs: NEXUS_DOCS_DIR is set but not walkable",
					"configured", cfg.DocsDir, "error", err)
			} else {
				slog.Info("docs: serving from override", "dir", cfg.DocsDir)
				docsBound = true
			}
		}
		if !docsBound {
			if err := docsserver.Err(); err != nil {
				slog.Error("docs: no usable docs directory; /api/docs will return an empty index",
					"default_root", docsserver.DefaultRoot, "error", err)
			} else {
				slog.Info("docs: serving from default location", "dir", docsserver.DefaultRoot)
			}
		}

		// Eval-plugin store + dispatcher + collector (Phases B/C).
		// The registry absorbs Helm-mounted ConfigMap plugins at
		// /etc/nexus/eval-plugins then layers DB-stored per-org
		// overrides on top (Helm wins).
		pluginReg := evalplugin.NewRegistry()
		// Strict-mode warnings (spec.flags: [strict]) route here.
		// Each suspect top-level spec key is logged once at boot
		// (ConfigMap plugins) and at every admin Save/Patch.
		evalplugin.SetStrictFieldSink(func(plugin, field string) {
			log.Warn("strict plugin spec has unknown field",
				"plugin", plugin, "field", "spec."+field)
		})
		if dir := cfg.PluginDir; dir != "" {
			if err := pluginReg.LoadFromDir(dir); err != nil {
				log.Warn("load plugin configmaps failed", "dir", dir, "err", err)
			}
		}
		if err := pluginReg.LoadFromStore(ctx, erc.PluginStore(), ""); err != nil {
			log.Warn("load plugins from db failed", "err", err)
		}
		if n := len(pluginReg.All()); n > 0 {
			log.Info("eval plugins loaded", "count", n, "durable", pluginStore != nil)
		}
		// Dispatcher / collector do real production work and need the
		// regular 30s window so a slow vendor doesn't truncate a
		// trace packet. Only the Test button uses a short-timeout
		// client (see httpClientForPluginsTest below) so its probe
		// finishes well inside the tunnel's 60-second response
		// deadline — otherwise an outage at the probe endpoint
		// surfaces as Cloudflare's 502 HTML page rather than a
		// typed {ok:false,…} JSON result.
		dispatcher := external.NewDispatcher(pluginReg, httpClientForPlugins())
		collector := external.NewCollector(pluginReg, stack.SinkForPlugins(), httpClientForPlugins())
		registerPluginAdapters(dispatcher, collector)
		// Credentials come from the in-product console-key UX (the
		// Plugin Keys panel), cached in memory and persisted encrypted
		// in eval_plugin_keys. The chart no longer projects vendor
		// Secrets into the pod, so envSecretResolver is deprecated;
		// consoleKeyResolver is the sole active surface. See
		// plugin_keys.go and plugin_secrets.go.
		pluginSecrets := newConsoleKeyResolver(keyVault)
		if n, err := pluginSecrets.Hydrate(ctx); err != nil {
			log.Warn("load stored plugin keys failed", "err", err)
		} else if n > 0 {
			log.Info("plugin keys restored", "plugins", n)
		}
		dispatcher.SetSecretResolver(pluginSecrets)
		collector.SetSecretResolver(pluginSecrets)
		collector.SetLogger(log)
		// Attribution for inbound vendor scores. A webhook POST from
		// LangSmith carries no Nexus session, so the trace id it echoes is
		// the only tenant signal; without this the collector can only fall
		// back to the plugin's own org, and a Helm-installed (cluster-wide)
		// plugin has none. Scores it cannot attribute are written as
		// evals.UnattributedOrgID rather than guessed into the default org.
		if stack.TraceOrgs != nil {
			collector.SetTraceOrgResolver(stack.TraceOrgs)
		} else {
			log.Info("eval plugin score attribution is limited",
				"reason", "no trace store configured",
				"effect", "scores from cluster-wide plugins are recorded as unattributed",
				"action", "install eval plugins per organisation, or configure ClickHouse")
		}
		// Mirror every persisted eval score to an OTLP /v1/logs
		// collector via OTLPEvaluationEventEnvelope. Noop by default
		// unless NEXUS_OTLP_LOGS_ENDPOINT (or NEXUS_OTLP_ENDPOINT
		// with /v1/logs appended) is set. Sink failures here do not
		// lose scores — evals.Sink.WriteScores returns first; only
		// after a successful write do we fan out the OTLP event.
		if logsURL := otlpLogsURLFromEnv(); logsURL != "" {
			logsSink := evals.NewHTTPLogSink(logsURL, httpClientForPlugins(), log)
			collector.SetEvaluationLogSink(logsSink)
			defer logsSink.Close()
			log.Info("eval OTLP event mirror enabled", "endpoint", logsURL)
		}
		multiEval := external.NewMultiEvaluator(pluginReg, dispatcher)
		// ServiceHeuristic plugins short-circuit Dispatch and run
		// in-process through the heuristic package. This is the
		// only place that imports internal/evaluators/heuristic,
		// keeping the multi.go evaluator interface free of any
		// concrete metric implementation.
		multiEval.SetLocalEvaluator(heuristicLocalEvaluator{})
		// Dispatch failures are best-effort but must not be invisible:
		// a wrong key or endpoint otherwise looks identical to a vendor
		// that simply has no results yet.
		multiEval.SetLogger(log)
		// Scheduler handles `scheduled` and `manual` Send.Trigger
		// values so MultiEvaluator no longer acts as if every plugin
		// were `on_trace`. The prior bug — the dispatcher ignoring
		// the trigger field — meant operators who selected
		// `scheduled` or `manual` for cost reasons still shipped every
		// trace to the vendor instantly.
		schedulerCfg := external.SchedulerConfig{
			MaxBufferPerPlugin: 4096,
			SweepInterval:      15 * time.Second,
		}
		scheduler := external.NewScheduler(dispatcher.Dispatch, schedulerCfg)
		scheduler.AttachLogger(log)
		multiEval.SetScheduler(scheduler)
		scheduler.Start(ctx, pluginReg)
		defer scheduler.Stop()
		evalWorker.SetPluginEvaluator(multiEval)
		// Manual-fire admin REST: backs the "Run now" button on
		// manual-trigger plugins. The shim looks up the plugin in
		// the registry by metadata.name because the scheduler
		// can't tell which name corresponds to which registered
		// plugin (its schedule map only carries the ones with a
		// goroutine, which excludes manual plugins).
		consoleSrvHandler.SetPluginManualFirer(&schedulerShim{
			s:   scheduler,
			reg: pluginReg,
		})
		// LangSmith automation: pressing "Create automation rule"
		// in a plugin row asks Nexus to call /api/v1/runs/rules on
		// the operator's behalf. We wire this only when NEXUS_PUBLIC_BASE_URL
		// is set — without it, Nexus has no externally-routable
		// URL to advertise, so any vendor-side webhook would 404 at
		// first eval.
		go func() {
			if err := collector.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("plugin collector stopped", "err", err)
			}
		}()
		erc.AttachPluginRegistry(pluginReg)
		consoleSrvHandler.SetEvalPlugins(pluginSourceAdapter{
			reg: pluginReg, store: erc.PluginStore(), log: log,
		})
		// Backstop for the registry: admin writes update it in place, and
		// a write that fails to land leaves no trace beyond a vendor
		// dashboard that stays empty. Re-deriving it from the store on a
		// timer bounds that to one interval and gives a second replica a
		// path to plugins installed through the first.
		go runPluginRegistryReconcile(ctx, pluginReg, erc.PluginStore(),
			pluginReconcileInterval, log)
		consoleSrvHandler.SetPluginCollector(collector)
		// The source adapter gives the Tester a way to resolve plugin
		// row ids (UUIDs) in addition to metadata.name. Built ad-hoc
		// here so we don't take a second dump of the same dependency
		// into the test handler closure.
		srcAdapter := &pluginSourceAdapter{reg: pluginReg, store: erc.PluginStore(), log: log}
		consoleSrvHandler.SetPluginTester(
			newTester(pluginReg, dispatcher, collector).
				withSource(srcAdapter).
				withSecrets(pluginSecrets))
		consoleSrvHandler.SetPluginKeys(pluginSecrets)

		// LangSmith automation: pressing "Create automation rule"
		// in a plugin row asks Nexus to call /api/v1/runs/rules on
		// the operator's behalf. We wire this only when NEXUS_PUBLIC_BASE_URL
		// is set — without it, Nexus has no externally-routable URL
		// to advertise, so any vendor-side webhook would 404 at
		// first eval.
		if strings.TrimSpace(cfg.PublicBaseURL) != "" {
			automator := &langsmithRuleAutomator{
				cfg: LangsmithAutomatorConfig{
					BaseURL:    cfg.PublicBaseURL,
					HeaderName: "X-Nexus-Plugin",
				},
				log:     log,
				plugins: srcAdapter,
				keys: keyVaultSecretsResolver{
					get: func(plugin string) (map[string]string, bool) {
						return pluginSecrets.Get(plugin)
					},
				},
			}
			consoleSrvHandler.SetLangSmithRuleCreator(automator)
			log.Info("langsmith automation wired", "public_base_url", cfg.PublicBaseURL)
		} else {
			log.Info("langsmith automation not wired (NEXUS_PUBLIC_BASE_URL unset)")
		}

		// Model benchmarks. These need Postgres for the run records and
		// the same vault for the provider token; nothing from the eval
		// worker itself, but they are wired here because this is where
		// the key resolver exists. Without Postgres the console routes
		// answer 503 with that explanation.
		if store != nil {
			benchRunner := benchmark.NewRunner(store, store,
				benchmarkTokens{keys: pluginSecrets}, cfg.PublicGatewayURL, log)
			consoleSrvHandler.SetBenchmarks(benchRunner)
			// A run outlives any request, so settlement has to be
			// driven from the background rather than from whoever
			// happens to have the page open.
			go benchRunner.Poll(ctx, benchmarkPollInterval)
			log.Info("model benchmarks enabled",
				"provider", benchmark.ProviderPrime,
				"gateway_routing", benchRunner.GatewayRoutingAvailable())

			// Champion drift: detect large relative changes between
			// adjacent settled runs and any model whose row is past
			// the freshness threshold. Settle-time detection runs
			// from the poll loop; staleness runs at boot.
			driftSink := benchmark.NewAuditSink(store)
			driftSrc := driftStore{store: store}
			watcher := benchmark.NewDriftWatcher(
				benchmark.DefaultDriftAlertSpec,
				driftSink, log)
			benchRunner.SetDriftWatcher(watcher, driftSrc)
			lineup := modelRouter.KnownModels()
			watcher.ObserveStaleness(ctx, driftSrc, lineup)
			log.Info("benchmark drift alerts enabled",
				"relative_threshold", benchmark.DefaultDriftAlertSpec.RelativeChangeThreshold,
				"freshness_threshold", benchmark.DefaultDriftAlertSpec.FreshnessThreshold)

// Scheduled benchmark re-fires. Shares the underlying
		// Runner so that launch, vkey minting and rollback on
		// partial launch failure all live in one place; the cron
		// package only owns the "when" and the bookkeeping.
		//
		// Phase D-1: in NEXUS_ROLE=worker the benchmark scheduler
		// IS the workload; the gateway pods run cron-less. In
		// NEXUS_ROLE=gateway the scheduler is intentionally
		// absent so the request hot path stays free of cron
		// goroutines. In NEXUS_ROLE=all-in-one both halves run,
		// but the lease gate (SchedulerRoleEnabled) still
		// enforces single-leader across replicas.
		var sched *cron.Runner
		if cfg.Mode == "gateway" {
			log.Info("benchmark scheduler intentionally absent (gateway mode)")
		} else {
			sched = cron.New(schedStore{store: store}, makeScheduleLander(benchRunner), log)
			if cfg.SchedulerRoleEnabled || cfg.Mode == "worker" {
				// Wire the Phase D-1 lease gate. The leaser
				// uses the same pgxpool as the rest of the
				// application; per-pod owner IDs are derived
				// from hostname + a process-local salt so a
				// restarted pod does not pick up a stale
				// lease by accident.
				//
				// All-in-one mode also enables the gate; in a
				// single-replica install the gate is a no-op
				// (one pod, no contention), but using the same
				// primitive everywhere means the rollout to
				// multi-pod cannot desync the lock semantics.
				leaserMgr := leaser.NewManager(store.Pool(), log)
				gate := cron.LeaderGateFromManager(leaserMgr, schedulerOwnerID(), log)
				sched.SetLeader(gate)
				// Per-schedule gate: even with the role-level
				// gate, defensively guard each fire() so the
				// handover window cannot race a schedule row
				// write. Schedule key takes two int32 hashes of
				// the schedule id (leaser.KeyForSchedule) so
				// unrelated schedules do not serialise on a
				// global mutex.
				sched.SetScheduleGate(cron.ScheduleGateFromManager(leaserMgr, schedulerOwnerID()))
				log.Info("benchmark scheduler leader-gated and per-schedule locked via Postgres lease", "mode", cfg.Mode)
			} else if cfg.Mode == "all-in-one" {
				log.Info("benchmark scheduler running without lease gate (all-in-one legacy)")
			}
			go sched.Run(ctx)
			log.Info("benchmark scheduler enabled", "mode", cfg.Mode)
		}
	}
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
	switch cfg.Mode {
	case "gateway":
		wg.Add(2)
		go serve(&wg, gwSrv, "gateway", cfg.GatewayAddr, log)
		go serve(&wg, consoleSrv, "console", cfg.ConsoleAddr, log)
		log.Info("mode=gateway - HTTP listeners up, scheduler absent")
	case "worker":
		// No HTTP listener on the worker pod. The data
		// plane is the cron scheduler. The Worker pod
		// exposes the metrics+readyz surface so the Helm
		// chart's metrics Service can scrape /metrics and
		// so an operator can probe /readyz (=== "we hold
		// the Postgres lease").
		//
		// Phase D-1 spec: "Worker는 트래픽을 받지 않으므로
		// Service를 만들지 마라. 다만 메트릭 수집과 헬스
		// 체크는 필요하다. 메트릭 포트만 노출하고". The
		// /metrics and /readyz handlers are the only HTTP
		// routes on this listener. There is no /v1/* route
		// on worker pods, so a misconfigured ClusterIP
		// / Ingress pointing at the worker pod gets a 404,
		// not a tenant's prompts.
healthMux := http.NewServeMux()
			healthMux.HandleFunc("/readyz", ready.Handler())
			healthMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
				// Phase D-1: Prometheus scraping on the
				// worker pod exposes only scheduler-side
				// counters. Until we wire the runtime
				// metrics registry explicitly, this
				// endpoint is a 200 no-op so Prometheus
				// does not stalemate on a missing scrape
				// target.
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("# worker metrics placeholder\n"))
			})
		workerSrv := &http.Server{
			Addr:    cfg.MetricsAddr,
			Handler: healthMux,
		}
		wg.Add(1)
		go serve(&wg, workerSrv, "worker-health", cfg.MetricsAddr, log)
		log.Info("mode=worker - scheduler up, /readyz+metrics on metrics addr only", "addr", cfg.MetricsAddr)
	default:
		// all-in-one (default): both halves are present so
		// local dev and single-container installs Just Work.
		wg.Add(2)
		go serve(&wg, gwSrv, "gateway", cfg.GatewayAddr, log)
		go serve(&wg, consoleSrv, "console", cfg.ConsoleAddr, log)
		log.Info("mode=all-in-one - HTTP + scheduler in one process")
	}

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

// (splitCSV lives in eval_runtime.go since PR #136.)

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
		// Decide the visibility scope before constructing the adapter so the
		// Registry can answer ConsoleCatalog.UserProviders() with a stable
		// Public/Org/User label per registered router. See PR #132.
		scopeHint := gateway.ScopeHint{Scope: gateway.ScopeOrg}
		ownerID := c.UserID
		if ownerID != "" {
			scopeHint = gateway.ScopeHint{Scope: gateway.ScopeUser, OwnerID: ownerID}
		}
		opts := providers.UserCompatOpts{OwnerID: ownerID, Scope: scopeHint.Scope}
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
			uc := providers.NewUserCompat(compat, opts)
			reg.RegisterHint(c.Provider, scopeHint, uc)
			log.Info("dynamic compat provider registered",
				"name", c.Provider, "last4", c.SecretLast4,
				"scope", string(scopeHint.Scope),
				"owner", ownerID,
				"chat_models", len(c.Models.Chat), "embed_models", len(c.Models.Embed))
			continue
		}
		log.Info("provider registered from credential store",
			"name", c.Provider, "last4", c.SecretLast4,
			"scope", string(scopeHint.Scope), "owner", ownerID)
	}
	// End of registerStoredCredentials.
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
//
// Adapters registered here are tagged ScopePublic so the ConsoleCatalog
// reports them as "Public" to every tenant, distinct from org-shared team
// routers (#132) and user BYOK routers (#PR-2).
func registerBuiltinCatalog(reg *gateway.Registry, cfg config.Config, log *slog.Logger) {
	reg.RegisterHint("openai", gateway.ScopeHint{Scope: gateway.ScopePublic}, providers.NewOpenAI("", cfg.OpenAIBaseURL, cfg.UpstreamTimeout))
	reg.RegisterHint("anthropic", gateway.ScopeHint{Scope: gateway.ScopePublic}, providers.NewAnthropic("", cfg.UpstreamTimeout))
	reg.RegisterHint("gemini", gateway.ScopeHint{Scope: gateway.ScopePublic}, providers.NewGemini("", cfg.UpstreamTimeout))
	reg.RegisterHint("groq", gateway.ScopeHint{Scope: gateway.ScopePublic}, providers.NewGroq("", cfg.UpstreamTimeout))
	reg.RegisterHint("mistral", gateway.ScopeHint{Scope: gateway.ScopePublic}, providers.NewMistral("", cfg.UpstreamTimeout))
	reg.RegisterHint("grid", gateway.ScopeHint{Scope: gateway.ScopePublic}, providers.NewGrid("", cfg.UpstreamTimeout))
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

// otlpLogsURLFromEnv returns the OTLP /v1/logs endpoint for the
// eval-events mirror, derived from NEXUS_OTLP_LOGS_ENDPOINT
// (when set explicitly) or NEXUS_OTLP_ENDPOINT (when present, by
// appending /v1/logs to the trace collector URL). Empty when
// neither is configured. Trimming handles /v1/traces already
// being a part of the trace endpoint.
func otlpLogsURLFromEnv() string {
	if v := os.Getenv("NEXUS_OTLP_LOGS_ENDPOINT"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// publicBaseForInvites returns the host root that should anchor
// the issued invite URL. EmailPublicBaseURL wins when set so the
// envelope's hostname can diverge from the console's host
// (e.g. nexus.ffx.ai). Otherwise we trust PublicBaseURL, which
// is what SetPublicBaseURL pulled from NEXUS_PUBLIC_BASE_URL in
// the operator's Helm values.
func publicBaseForInvites(cfg config.Config) string {
	if v := strings.TrimSpace(cfg.EmailPublicBaseURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
}

// schedulerOwnerID derives a Pod-stable identifier so a restarted
// Nexus process does not pick up its predecessor's lease row by
// accident. The pattern is "<hostname>-<pid>" — hostname is set by
// Kubernetes downward API and stays stable across restarts within
// the same Pod; pid changes per-process and forces a new lease.
// We deliberately use just hostname + pid (no random suffix) so a
// debugging operator can correlate "old pid 4127 stopped renewing"
// with the corresponding log line without consulting a token store.
func schedulerOwnerID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
