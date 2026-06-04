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
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/gateway/providers"
	"github.com/ffxnexus/nexus/internal/limiter"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
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
			schema, _ := nexus.Migrations.ReadFile("migrations/postgres/001_init.sql")
			if err := st.Migrate(ctx, string(schema)); err != nil {
				log.Error("postgres migrate failed", "err", err)
			}
			store = st
			auth = makeAuthenticator(st)
			log.Info("control plane enabled (virtual key auth + credential store)",
				"credential_encryption", st.HasCipher())
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
			schema, _ := nexus.Migrations.ReadFile("migrations/clickhouse/001_init.sql")
			if err := rec.Migrate(ctx, string(schema)); err != nil {
				log.Error("clickhouse migrate failed", "err", err)
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
		evalWorker = evals.NewWorker(evals.Options{
			Heuristics:      heuristics,
			Judges:          judges,
			Sink:            evals.NewCHSink(chRec.Conn()),
			JudgeSampleRate: cfg.EvalSampleRate,
		}, log)
		recorders = append(recorders, evalWorker)
		log.Info("eval worker enabled (async quality evaluation)")
	} else {
		log.Info("clickhouse not configured; eval worker disabled")
	}

	recorder := observability.NewMultiRecorder(recorders...)

	// Gateway server.
	gwHandler := gateway.NewHandler(reg, recorder, lim, log)

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
		log.Info("quality-aware routing enabled", "groups", len(groups), "alias", "auto")
	} else {
		log.Info("clickhouse not configured; quality-aware routing disabled")
	}
	gwSrv := &http.Server{
		Addr:    cfg.GatewayAddr,
		Handler: gateway.NewMux(gwHandler, auth, lim, log),
	}

	// Console server.
	consoleSrvHandler := console.NewServer(hub, reader, store, log)
	if modelRouter != nil {
		consoleSrvHandler.SetRouteStats(modelRouter)
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
	wg.Wait()
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
		return gateway.AuthResult{
			OrgID:         vk.OrgID,
			VKeyID:        vk.ID,
			AllowedModels: vk.AllowedModels,
			RPMLimit:      vk.RPMLimit,
			MonthlyBudget: vk.MonthlyBudget,
		}, nil
	}
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
