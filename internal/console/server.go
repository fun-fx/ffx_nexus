package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/limiter"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
	nexusweb "github.com/ffxnexus/nexus/web"
)

// RouteStatsSource exposes the router's current rolling per-model stats.
type RouteStatsSource interface {
	Snapshot() map[string]router.ModelStats
}

// CatalogSource exposes the gateway registry's merged model catalog so the
// console's Playground page can populate its model picker without having to
// call gateway /v1/models (which requires a virtual-key Authorization header
// that the console does not own). The interface is satisfied directly by the
// gateway's console adapter — see gateway.Handler.Catalog() — so we don't
// need to import the gateway package here.
type CatalogSource interface {
	ChatModels() []string
	EmbeddingModels() []string
	UserProviders() []gateway.ConsoleUserProvider
}

// Server exposes the dashboard API: recent traces, window stats, a live
// WebSocket feed, routing stats, and (when a store is configured)
// key/credential management.
type Server struct {
	hub               *Hub
	reader            *observability.Reader // may be nil when ClickHouse is not configured
	store             *core.Store           // may be nil when Postgres is not configured
	routes            RouteStatsSource      // may be nil when routing is disabled
	catalog           CatalogSource         // may be nil when the gateway is not co-located
	reload            func(context.Context) // may be nil when no hot-reload hook is wired
	allowSignup       bool                  // public POST /api/auth/register
	sso               *ssoClient            // OIDC client; nil when SSO is not configured
	evalConfigSrc     EvalConfigSource      // nil when eval worker is disabled
	evalConfigApply   EvalConfigApplier     // nil when eval worker is disabled
	evalProfiles      EvalProfileSource     // PR #135: profile CRUD store
	evalPlugins       EvalPluginSource      // eval-plugin store (Phase B)
	pluginCollector   PluginWebhookReceiver // eval-plugin webhook sink (Phase C)
	pluginTester      EvalPluginTester      // eval-plugin test-send (Phase D)
	pluginManualFirer PluginManualFirer     // admin-driven drain for manual-trigger plugins
	pluginKeys        EvalPluginKeys        // in-process plugin key resolver (console keys)
	benchmarks        BenchmarkRunner       // model-level benchmark runs; nil without Postgres
	// grafanaURL is the operator-supplied public Grafana base URL surfaced
	// on Spend / Quality / Traces pages so an operator can click through
	// to the matching bundled dashboard. Empty == console hides the link.
	grafanaURL string
	// qualityRouter exposes the in-memory router state to the console
	// so /api/eval/benchmarks/quality can answer "what is the
	// router actually doing right now". nil when the router is not
	// wired (e.g. local debug deployments); the route then 503s.
	qualityRouter QualityRouterQuerier
	// pushReports holds operator-reported `prime env push` outcomes.
	// In-memory and advisory only — see benchmark_push_report.go. The
	// zero value works, so NewServer leaves it alone.
	pushReports pushReportStore
	// langsmithRuleCreator is the vendor-side automator driven by
	// /api/eval/plugins/{name}/automation. nil when LangSmith is
	// not configured (the route answers 503 — the UI surfaces a
	// "not wired" explanation rather than crashing the tree).
	langsmithRuleCreator LangSmithRuleCreator
	langsmithCreatable   func(ctx context.Context, pluginName string) bool // predicate for UI gating
	loginLim             *limiter.IPLimiter                                // per-IP rate limit for /api/auth/login
	registerLim          *limiter.IPLimiter                                // per-IP rate limit for /api/auth/register
	ssoLim               *limiter.IPLimiter                                // per-IP rate limit for /api/auth/sso/*
	gatewayProxy         *httputil.ReverseProxy                            // optional /v1/* → co-located gateway
	publicGatewayURL     string                                            // optional public gateway base for UI copy
	log                  *slog.Logger
	up                   websocket.Upgrader
}

// SetAllowSignup toggles public self-service registration (member role only).
// SetBuildTag (on *Server) is kept as a small wrapper for callers
// that prefer the OO style; it forwards to the package-level
// helper used by writeJSON.
func (s *Server) SetBuildTag(tag string) { SetBuildTag(tag) }

func (s *Server) SetAllowSignup(allow bool) { s.allowSignup = allow }

// SetSSO configures the OIDC client used by /api/auth/sso/*. A nil or
// disabled config is a no-op; the field stays nil and the SSO routes
// 404, which keeps deployments without an IdP completely unaffected.
func (s *Server) SetSSO(ctx context.Context, cfg config.SSOConfig) {
	if !cfg.Enabled() {
		s.log.Info("SSO not configured; /api/auth/sso/* routes disabled")
		return
	}
	client, err := newSSOClient(ctx, cfg)
	if err != nil {
		s.log.Error("SSO init failed; /api/auth/sso/* routes disabled", "err", err)
		return
	}
	s.sso = client
	s.log.Info("SSO enabled", "issuer", cfg.Issuer, "client_id", cfg.ClientID, "label", cfg.LabelOrDefault())
}

// SSOEnabled reports whether /api/auth/sso/* is wired up. The console
// uses this to decide whether to render the SSO sign-in button.
func (s *Server) SSOEnabled() bool { return s.sso != nil }

// SSOLabel is the UI label for the SSO button (e.g. "Keycloak").
func (s *Server) SSOLabel() string {
	if s.sso == nil {
		return ""
	}
	return s.sso.cfg.LabelOrDefault()
}

// SetRouteStats attaches a routing stats source for the /api/routing endpoint.
func (s *Server) SetRouteStats(src RouteStatsSource) { s.routes = src }

// SetCatalog attaches a catalog source for the /api/me/playground/catalog
// endpoint. The Playground page consumes this so it can list stock + user
// providers without needing a virtual-key Authorization header.
func (s *Server) SetCatalog(src CatalogSource) { s.catalog = src }

// SetCredentialReloader registers a callback invoked after credential changes
// (rotate/delete) so the gateway can refresh its in-memory providers without a
// restart. Optional; when unset, credential changes apply on next restart.
func (s *Server) SetCredentialReloader(fn func(context.Context)) { s.reload = fn }

// SetEvalConfig wires eval/routing runtime config for GET/PATCH /api/eval/config.
func (s *Server) SetEvalConfig(src EvalConfigSource, apply EvalConfigApplier) {
	s.evalConfigSrc = src
	s.evalConfigApply = apply
}

// SetPublicGrafanaURL surfaces the operator-supplied Grafana base URL on
// /api/ui/observability so the console's *Spend* / *Quality* / *Traces*
// pages can render an "Open in Grafana" link instead of forcing the
// operator to remember the entrypoint. Empty == the field is omitted and
// the UI hides the link entirely.
func (s *Server) SetPublicGrafanaURL(url string) {
	s.grafanaURL = strings.TrimRight(url, "/")
}

// SetEvalProfiles attaches the profile store used by PR #135 per-eval
// endpoints. The console reads/writes through this dependency; the
// runtime controller pushes profile snapshots into the worker on
// changes via a separate channel.
func (s *Server) SetEvalProfiles(src EvalProfileSource) {
	s.evalProfiles = src
}

// SetEvalPlugins attaches the plugin store used by the admin REST
// routes under /api/eval/plugins. nil means the feature is disabled
// (single-tenant builds without Postgres) and the routes respond 503.
func (s *Server) SetEvalPlugins(src EvalPluginSource) {
	s.evalPlugins = src
}

// SetPluginCollector wires the inbound webhook receiver. Webhook
// routing keys on plugin metadata.name so a single shared endpoint
// covers all installed plugins.
func (s *Server) SetPluginCollector(c PluginWebhookReceiver) {
	s.pluginCollector = c
}

// SetPluginTester wires the "test-send" handler. nil means the
// route answers 503 — set this from main.go once the dispatcher is
// up and ready.
func (s *Server) SetPluginTester(t EvalPluginTester) {
	s.pluginTester = t
}

// SetPluginManualFirer wires the manual-trigger admin REST route.
// When a plugin's spec.send.trigger is `manual`, the dispatcher does
// not forward inline traces; instead the operator invokes this
// surface to drain whatever buffer exists (typically empty) plus
// trigger the configured vendor path immediately.
func (s *Server) SetPluginManualFirer(f PluginManualFirer) {
	s.pluginManualFirer = f
}

// SetPluginKeys wires the resolver that the Plugin Keys panel talks
// to. nil means the GET/PUT/DELETE /api/eval/plugins/{name}/keys
// routes answer 503 — set this from main.go before the panel is open
// to operators in production.
func (s *Server) SetPluginKeys(k EvalPluginKeys) {
	s.pluginKeys = k
}

// NewServer builds the console server. reader and store may be nil.
func NewServer(hub *Hub, reader *observability.Reader, store *core.Store, log *slog.Logger) *Server {
	return &Server{
		hub:    hub,
		reader: reader,
		store:  store,
		log:    log,
		// Per design doc §4.2.5: 30 req/min/IP on anonymous auth routes.
		loginLim:    limiter.NewIPLimiter(30, time.Minute),
		registerLim: limiter.NewIPLimiter(30, time.Minute),
		ssoLim:      limiter.NewIPLimiter(30, time.Minute),
		up: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// Mux returns the console HTTP handler.
func (s *Server) Mux() http.Handler {
	r := chi.NewRouter()
	r.Use(s.securityHeaders)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	}))
	r.Use(s.withUser)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Anonymous auth routes get per-IP rate limiting (design doc §4.2.5).
	// Limiters are per-route so an attacker cannot drain login by hammering
	// /api/auth/register.
	authRL := func(routeName string, lim *limiter.IPLimiter) func(http.Handler) http.Handler {
		return s.ipRateLimit(routeName, lim)
	}

	r.Route("/api", func(r chi.Router) {
		r.Get("/traces", s.requireUser(s.recentTraces))
		r.Get("/turns", s.requireUser(s.recentTurns))
		r.Get("/stats", s.requireUser(s.stats))
		r.Get("/stats/providers", s.requireUser(s.providerStats))
		r.Get("/routing", s.routing)
		r.Get("/evals", s.evals)
		r.Get("/eval/config", s.requireAdmin(s.getEvalConfig))
		r.Patch("/eval/config", s.requireAdmin(s.patchEvalConfig))
		r.Get("/live", s.requireUser(s.live))

		// Session auth + self-service (requires Postgres).
		r.Get("/auth/config", s.authConfig)
		r.With(authRL("login", s.loginLim)).Post("/auth/login", s.login)
		r.With(authRL("register", s.registerLim)).Post("/auth/register", s.register)
		r.Post("/auth/logout", s.logout)
		r.With(authRL("sso-login", s.ssoLim)).Get("/auth/sso/login", s.ssoLogin)
		r.With(authRL("sso-callback", s.ssoLim)).Get("/auth/sso/callback", s.ssoCallback)
		r.Get("/me", s.requireUser(s.me))
		r.Patch("/me", s.requireUser(s.updateMe))
		r.Get("/me/stats", s.requireUser(s.myStats))
		r.Get("/me/traces", s.requireUser(s.myTraces))
		r.Get("/me/turns", s.requireUser(s.myTurns))
		r.Get("/me/quality", s.requireUser(s.myQuality))
		r.Get("/me/spend/summary", s.requireUser(s.mySpendSummary))
		r.Get("/me/spend/daily", s.requireUser(s.mySpendDaily))
		r.Get("/me/spend/daily/{day}/breakdown", s.requireUser(s.mySpendBreakdown))
		r.Get("/me/keys", s.requireUser(s.listMyKeys))
		r.Post("/me/keys", s.requireUser(s.createMyKey))
		r.Delete("/me/keys/{id}", s.requireUser(s.revokeMyKey))
		r.Get("/me/credentials", s.requireUser(s.listMyCredentials))
		r.Post("/me/credentials/preflight", s.requireUser(s.preflightCredential))
		r.Post("/me/credentials", s.requireUser(s.createMyCredential))
		r.Post("/me/credentials/{id}/rotate", s.requireUser(s.rotateMyCredential))
		r.Delete("/me/credentials/{id}", s.requireUser(s.deleteMyCredential))
		// Playground helper: session-authenticated browse of the gateway catalog.
		// /v1/models itself requires a virtual-key Authorization header, which
		// the console session has no view of; this endpoint lets the reader
		// enumerate stock + user-defined providers using their Nexus cookie.
		r.Get("/me/playground/catalog", s.requireUser(s.playgroundCatalog))

		// Lightweight public read used by the *Spend* / *Quality* / *Traces*
		// pages to render an "Open in Grafana" link. Public because Grafana
		// itself is on the open ingress and the URL is non-sensitive; authn
		// would just gate a CSS rule. Anonymous OK.
		r.Get("/ui/observability", s.uiObservability)

		// PR #135: per-eval profile CRUD. Visibilities mirror router
		// models (PR #133): org profiles are visible to every member,
		// user profiles to the calling user only, admins always see
		// both. Mutating POST/PATCH/DELETE on org profiles is admin-only;
		// user profiles are editable by their owner.
		r.Route("/eval/profiles", func(r chi.Router) {
			r.Get("/", s.requireUser(s.listEvalProfiles))
			r.Post("/", s.requireAdmin(s.createEvalProfile))
			r.Patch("/{id}", s.requireUser(s.patchEvalProfile))
			r.Delete("/{id}", s.requireUser(s.deleteEvalProfile))
		})

		// Eval plugins (Phase B). Admin-only writes (post/patch/delete);
		// admin-only reads because the YAML can carry sensitive URLs.
		r.Route("/eval/plugins", func(r chi.Router) {
			r.Get("/", s.requireAdmin(s.listEvalPlugins))
			r.Post("/", s.requireAdmin(s.createEvalPlugin))
			r.Patch("/{id}", s.requireAdmin(s.patchEvalPlugin))
			r.Delete("/{id}", s.requireAdmin(s.deleteEvalPlugin))
		})

		// Plugin webhooks — third-party services POST score results. The
		// signature and replay-prevention checks live in Collector;
		// this route only reads the body.
		if s.pluginCollector != nil {
			r.Post("/eval/plugins/{name}/webhook", s.pluginWebhook)
		}

		// Test-send verifies a plugin's secret ref + endpoint; admins
		// hit this before flipping the toggle to "on" so misconfigured
		// endpoints surface in the UI rather than silently dropping
		// traces.
		if s.pluginTester != nil {
			r.Post("/eval/plugins/{name}/test", s.requireAdmin(s.pluginTest))
		}

		// Manual-fire: triggers an immediate drain-and-dispatch for
		// plugins whose spec.send.trigger is `manual`. The dispatcher
		// does not forward inline traces for manual plugins, so this
		// is the only way to actually exercise such a plugin against
		// the gateway. Admin-only because the call may create a real
		// vendor run that costs money.
		if s.pluginManualFirer != nil {
			r.Post("/eval/plugins/{name}/fire", s.requireAdmin(s.pluginFireManual))
		}

		// LangSmith automation: pressing "Create automation rule" in
		// the plugin row asks Nexus to call /api/v1/runs/rules on
		// behalf of the operator, removing the one manual step the
		// LangSmith UI previously required (PR #196). Admin-only
		// because the call mints vendor-side resources under the
		// tenant's bill. The route is always registered; a nil
		// langsmithRuleCreator returns a typed 503 envelope so the
		// UI can render an explanation rather than a 404.
		r.Post("/eval/plugins/{name}/automation", s.requireAdmin(s.pluginCreateAutomationRule))

		// Plugin Keys panel: lets admins paste per-vendor API keys into
		// the console instead of dropping a Helm chart-managed Secret.
		// All three handlers (GET/PUT/DELETE) are admin-only and require
		// the resolver to be wired; failing gracefully with 503 keeps
		// the route discoverable rather than returning a generic 404.
		if s.pluginKeys != nil {
			r.Get("/eval/plugins/{name}/keys", s.requireAdmin(s.getPluginKeys))
			r.Put("/eval/plugins/{name}/keys", s.requireAdmin(s.putPluginKeys))
			r.Delete("/eval/plugins/{name}/keys", s.requireAdmin(s.deletePluginKeys))
		}

		// Benchmark runs: a model measured against a dataset by an
		// external platform. Admin-only throughout — a launch spends
		// money at the provider, and a gateway-routed one mints a key
		// that lets their sandbox call us.
		//
		// Static segments are registered before "/{id}" so a path like
		// /models is never captured as a run identifier.
		r.Route("/eval/benchmarks", func(r chi.Router) {
			r.Get("/", s.requireAdmin(s.listBenchmarks))
			r.Post("/", s.requireAdmin(s.launchBenchmark))

			// Schedules: per-tenant re-fire plans that drive the
			// cron.runner. The endpoints exist so an operator can
			// shape recurring benchmark coverage without manually
			// relaunching runs. Schedules survive a restart because
			// the table is durable; the cron goroutine picks them
			// up on its next tick.
			r.Route("/schedules", func(r chi.Router) {
				r.Get("/", s.requireAdmin(s.listBenchmarkSchedules))
				r.Post("/", s.requireAdmin(s.createBenchmarkSchedule))
				r.Get("/{id}", s.requireAdmin(s.getBenchmarkSchedule))
				r.Delete("/{id}", s.requireAdmin(s.deleteBenchmarkSchedule))
			})

			// Validate is the safest way to check that the credential the
			// operator just pasted actually gets a 2xx from the vendor
			// before they hit Launch on a real budget-bound run.
			r.Post("/validate", s.requireAdmin(s.dryRunBenchmark))
			// push-report is how the operator tells us their local
			// `prime env push` finished. Nexus cannot run that command
			// or watch it, so this is the only way the console learns a
			// slug was published. Advisory only: /validate still decides
			// whether the vendor can actually see the slug.
			r.Post("/push-report", s.requireAdmin(s.reportEnvPush))
			r.Get("/push-report", s.requireAdmin(s.listEnvPushReports))
			r.Get("/models", s.requireAdmin(s.benchmarkModels))
			r.Post("/refresh", s.requireAdmin(s.refreshBenchmarks))
			r.Get("/credential", s.requireAdmin(s.getBenchmarkCredential))
			r.Put("/credential", s.requireAdmin(s.putBenchmarkCredential))
			r.Delete("/credential", s.requireAdmin(s.deleteBenchmarkCredential))
			r.Get("/{id}", s.requireAdmin(s.getBenchmark))
			r.Delete("/{id}", s.requireAdmin(s.deleteBenchmark))
			r.Post("/{id}/cancel", s.requireAdmin(s.cancelBenchmark))
			r.Get("/{id}/logs", s.requireAdmin(s.benchmarkLogs))
		})

		// User management (admin only).
		r.Get("/users", s.requireAdmin(s.listUsers))
		r.Post("/users", s.requireAdmin(s.createUser))
		r.Delete("/users/{id}", s.requireAdmin(s.deleteUser))
		r.Get("/users/quality", s.requireAdmin(s.userQuality))
		r.Get("/users/{id}/spend/summary", s.requireAdmin(s.userSpendSummary))
		r.Get("/users/{id}/spend/daily", s.requireAdmin(s.userSpendDaily))
		r.Get("/users/{id}/spend/daily/{day}/breakdown", s.requireAdmin(s.userSpendBreakdown))
		r.Get("/audit", s.requireAdmin(s.listAudit))

		// Operator-facing quality snapshot: model lineup + freshness.
		// Returns the most recent settled benchmark score per model
		// plus the decay-applied contribution weight the router
		// is mixing in right now. The admin's "is this benchmark still
		// teaching the router anything?" question has its answer here.
		r.Get("/eval/benchmarks/quality", s.requireAdmin(s.benchmarkQuality))
		r.Get("/eval/benchmarks/leaderboard", s.requireAdmin(s.benchmarkLeaderboard))
		r.Post("/eval/benchmarks/quality", s.requireAdmin(s.benchmarkQualityGate))
		r.Get("/eval/benchmarks/{model}/history", s.requireAdmin(s.benchmarkHistory))

		// Backwards-compat alias: /api/me/quality/stats (deprecated, prefer
		// /api/me/quality) — kept for any client that has been wired against
		// the original name. No admin-only path here; me/quality is per-user.

		// Org-level key/credential management (requires Postgres).
		r.Get("/keys", s.listKeys)
		r.Post("/keys", s.requireAdmin(s.createKey))
		r.Delete("/keys/{id}", s.requireAdmin(s.revokeKey))
		r.Get("/credentials", s.listCredentials)
		r.Post("/credentials", s.requireAdmin(s.createCredential))
		r.Post("/credentials/{id}/rotate", s.requireAdmin(s.rotateCredential))
		r.Delete("/credentials/{id}", s.requireAdmin(s.deleteCredential))
	})

	// Same-origin /v1 for Playground + model discovery when the console is on
	// a public hostname separate from the dedicated gateway hostname.
	if s.gatewayProxy != nil {
		r.Handle("/v1", s.gatewayProxy)
		r.Handle("/v1/*", s.gatewayProxy)
	}

	// Serve the embedded dashboard SPA for everything else, with a fallback to
	// index.html so client-side routes resolve.
	r.Handle("/*", spaHandler(s.log))

	return r
}

// spaHandler serves the embedded dashboard build. Requests for missing paths
// fall back to index.html (single-page-app routing).
func spaHandler(log *slog.Logger) http.Handler {
	sub, err := nexusweb.Dist()
	if err != nil {
		log.Error("dashboard assets unavailable", "err", err)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "dashboard not built", http.StatusNotImplemented)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, statErr := fs.Stat(sub, p); statErr != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) recentTraces(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, observability.TracePage{Items: []observability.TraceSummary{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, since, filter, err := parseTraceQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	page, err := s.reader.TracePage(r.Context(), before, since, limit, uid, filter)
	if err != nil {
		s.log.Error("recent traces query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if u.Role == core.RoleAdmin {
		s.enrichTraceUserEmails(r.Context(), orgID(r), page.Items)
	}
	writeJSON(w, http.StatusOK, page)
}

// recentTurns backs the overview's grouped list: one row per agent turn
// instead of one per upstream call. The window defaults to the trailing
// hour so the page has a bounded GROUP BY to chew on; expanding a row
// drills back into /api/traces?turn=<id> for the individual calls.
func (s *Server) recentTurns(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.TurnSummary{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	turns, err := s.reader.TurnPage(r.Context(), time.Time{}, turnWindowStart(r), limit, uid)
	if err != nil {
		s.log.Error("recent turns query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if u.Role == core.RoleAdmin {
		s.enrichTurnUserEmails(r.Context(), orgID(r), turns)
	}
	if turns == nil {
		turns = []observability.TurnSummary{}
	}
	writeJSON(w, http.StatusOK, turns)
}

// turnWindowStart resolves the `window` duration param (default 1h) into an
// absolute lower bound. Turns are grouped, not cursor-paged, so a window is
// the only bound available — an unbounded GROUP BY over the full 90-day TTL
// would scan the whole table to render ten rows.
func turnWindowStart(r *http.Request) time.Time {
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil && d > 0 {
			window = d
		}
	}
	return time.Now().Add(-window)
}

// enrichTurnUserEmails is enrichTraceUserEmails for grouped rows. ClickHouse
// stores only user_id; emails live in Postgres.
func (s *Server) enrichTurnUserEmails(ctx context.Context, org string, turns []observability.TurnSummary) {
	if s.store == nil || len(turns) == 0 {
		return
	}
	users, err := s.store.ListUsers(ctx, org)
	if err != nil {
		s.log.Warn("turn user email lookup failed", "err", err)
		return
	}
	byID := make(map[string]string, len(users))
	for _, u := range users {
		byID[u.ID] = u.Email
	}
	for i := range turns {
		if turns[i].UserID != "" {
			turns[i].UserEmail = byID[turns[i].UserID]
		}
	}
}

// parseTraceQuery extracts the trace-listing query parameters off the
// `/api/traces` and `/api/me/traces` endpoints. All fields are optional:
//
//   - `before` / `since`: RFC3339 timestamps forming a half-open window
//     `[since, before)` — must be `before > since` when both set.
//   - `status`: "ok" (<400) / "err" (>=400) / empty (any).
//   - `provider`: exact match against `provider_name`.
//   - `turn`: exact match against `turn_id`. Sent by the overview when a
//     grouped row is expanded to list the individual calls of that turn.
//   - `q`: free-text fuzzy match against `request_model | provider_name |
//     user_email | guardrail_action`. `%` and `_` are NOT yet escaped here —
//     the reader does that server-side so the SQL text stays clean.
//
// Invalid values return a 4xx-friendly error.
func parseTraceQuery(r *http.Request) (before, since time.Time, filter observability.TraceFilter, err error) {
	q := r.URL.Query()

	if v := q.Get("before"); v != "" {
		t, perr := parseRFC3339(v)
		if perr != nil {
			return time.Time{}, time.Time{}, filter, fmt.Errorf("invalid `before` timestamp: %w", perr)
		}
		before = t
	}
	if v := q.Get("since"); v != "" {
		t, perr := parseRFC3339(v)
		if perr != nil {
			return time.Time{}, time.Time{}, filter, fmt.Errorf("invalid `since` timestamp: %w", perr)
		}
		since = t
	}
	if !before.IsZero() && !since.IsZero() && !before.After(since) {
		return time.Time{}, time.Time{}, filter, fmt.Errorf("`before` must be after `since`")
	}

	filter.Status = q.Get("status")
	switch filter.Status {
	case "", "ok", "err":
		// accepted
	default:
		return time.Time{}, time.Time{}, filter, fmt.Errorf("invalid `status`: want ok|err|<empty>, got %q", filter.Status)
	}
	filter.Provider = q.Get("provider")
	filter.Q = q.Get("q")
	filter.Turn = q.Get("turn")
	return before, since, filter, nil
}

// parseRFC3339 accepts both RFC3339 ("...Z") and RFC3339Nano ("...123456789Z")
// forms because the console may emit nanoseconds when an upstream returned
// a high-precision timestamp. ClickHouse's DateTime64(3) column rejects
// nanosecond precision so the reader converts to UTC second precision
// inside the query; accepting both here keeps the API forgiving.
func parseRFC3339(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

// enrichTraceUserEmails attaches caller emails to trace rows for the admin
// overview. ClickHouse stores only user_id; emails live in Postgres.
func (s *Server) enrichTraceUserEmails(ctx context.Context, org string, traces []observability.TraceSummary) {
	if s.store == nil || len(traces) == 0 {
		return
	}
	users, err := s.store.ListUsers(ctx, org)
	if err != nil {
		s.log.Warn("trace user email lookup failed", "err", err)
		return
	}
	byID := make(map[string]string, len(users))
	for _, u := range users {
		byID[u.ID] = u.Email
	}
	for i := range traces {
		if traces[i].UserID != "" {
			traces[i].UserEmail = byID[traces[i].UserID]
		}
	}
}

// providerStats returns per-provider aggregates for the spend-by-provider
// widget. Cached in-process for 30 s so the Overview page can re-render on a
// developer dashboard refresh without punching ClickHouse on every poll; the
// underlying SELECT is a single GROUP BY on a partitioned column and stays
// under one second on the prod gateway_traces table, but repetition at the
// UI refresh rate would still be wasteful once we hit dozens of concurrent
// dashboard tabs.
//
// The cache key is (window, userID-scope) so scoped queries don't poison each
// other's buckets.
type providerStatsCacheEntry struct {
	value     []observability.ProviderStat
	expiresAt time.Time
}

var (
	providerStatsCacheMu sync.RWMutex
	providerStatsCache   = map[string]providerStatsCacheEntry{}
	providerStatsTTL     = 30 * time.Second
)

func providerStatsCacheGet(key string) ([]observability.ProviderStat, bool) {
	providerStatsCacheMu.RLock()
	defer providerStatsCacheMu.RUnlock()
	e, ok := providerStatsCache[key]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

func providerStatsCacheSet(key string, v []observability.ProviderStat) {
	providerStatsCacheMu.Lock()
	defer providerStatsCacheMu.Unlock()
	providerStatsCache[key] = providerStatsCacheEntry{value: v, expiresAt: time.Now().Add(providerStatsTTL)}
}

func (s *Server) providerStats(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.ProviderStat{})
		return
	}
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			window = d
		}
	}
	scope := "admin"
	if u.Role != core.RoleAdmin {
		scope = "user:" + u.ID
	}
	cacheKey := scope + "|" + window.String()

	if cached, ok := providerStatsCacheGet(cacheKey); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	out, err := s.reader.ProviderStats(r.Context(), window, uid, 20)
	if err != nil {
		s.log.Error("provider stats query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []observability.ProviderStat{}
	}
	providerStatsCacheSet(cacheKey, out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, observability.Stats{})
		return
	}
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			window = d
		}
	}
	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	st, err := s.reader.WindowStats(r.Context(), window, uid)
	if err != nil {
		s.log.Error("stats query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// routing returns the router's current per-model quality/cost/latency stats,
// sorted by quality descending, so the console can show why models are chosen.
func (s *Server) routing(w http.ResponseWriter, _ *http.Request) {
	if s.routes == nil {
		writeJSON(w, http.StatusOK, []router.ModelStats{})
		return
	}
	snap := s.routes.Snapshot()
	out := make([]router.ModelStats, 0, len(snap))
	for _, v := range snap {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Quality > out[j].Quality })
	writeJSON(w, http.StatusOK, out)
}

// evals returns per-(evaluator, metric) aggregates of async eval scores over the
// requested window so the console can show quality/safety trends.
func (s *Server) evals(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.EvalMetric{})
		return
	}
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			window = d
		}
	}
	out, err := s.reader.EvalSummary(r.Context(), window)
	if err != nil {
		s.log.Error("eval summary query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) live(w http.ResponseWriter, r *http.Request, u core.User) {
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	ch := s.hub.subscribe(uid)
	defer s.hub.unsubscribe(ch)

	// Reader goroutine to detect client disconnect.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				_ = conn.Close()
				return
			}
		}
	}()

	for t := range ch {
		if err := conn.WriteJSON(t); err != nil {
			return
		}
	}
}

// audit is a thin convenience wrapper that defers to the store's audit
// log when one is available. Failures are swallowed (audit is best-effort
// by design; see core.Store.Audit). actorID is the user_id of the caller;
// pass "" for system actions.
func (s *Server) audit(ctx context.Context, actorID, orgID, action, targetID, detail string) {
	if s.store == nil {
		return
	}
	s.store.Audit(ctx, actorID, orgID, action, targetID, detail)
}

// playgroundCatalog exposes the merged model catalog for any
// session-authenticated user (member or admin). Returned shape mirrors the
// shape GatewayModelCatalog expects on the client.
//
// Visibility rules (informally known as "team router / personal router"
// gating, see the user's feedback that arrived 2026-07-24):
//   - Every chat/embed model id from the registry is returned for everyone
//     — that path is taken by stock provider models registered with
//     ScopePublic, which the catalog reports through ChatModels() /
//     EmbeddingModels() (their fillter happens upstream in the gateway).
//   - The "user" provider list is filtered per caller:
//   - ScopePublic  → visible to everyone
//   - ScopeOrg     → visible only to members of the same org_id
//   - ScopeUser    → visible only to the OwnerID matching the caller's
//     user id; admins always see them so they can audit
//     accidental leaks / noisy neighbours.
//
// The frontend Playground page uses the per-provider scope to colour
// "Personal" vs "Team" vs "Public" badges (frontend PR #133).
func (s *Server) playgroundCatalog(w http.ResponseWriter, _ *http.Request, caller core.User) {
	if s.catalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"chat":  []string{},
			"embed": []string{},
			"user":  []any{},
		})
		return
	}
	allUsers := s.catalog.UserProviders()
	userOut := make([]map[string]any, 0, len(allUsers))
	for _, u := range allUsers {
		if !callerCanSee(u, caller) {
			continue
		}
		userOut = append(userOut, map[string]any{
			"provider": u.Provider,
			"models":   u.Models,
			"scope":    string(u.Scope),
			"owner_id": u.OwnerID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chat":  s.catalog.ChatModels(),
		"embed": s.catalog.EmbeddingModels(),
		"user":  userOut,
	})
}

// callerCanSee answers "should this caller be told this provider exists?"
// against the per-provider scope metadata the gateway recorded at
// registration time (PR #132).
//
//   - Public   → always shown (no per-caller state check).
//   - Org      → shown when the caller shares the provider's owning org.
//     Console users all live in a single org today (Org:
//     "default" via core.NewStore), so the org-scope filter
//     collapses to "always" while the surface area stays ready
//     for a future multi-tenancy switch.
//   - User     → shown only when the caller's user_id matches the
//     provider's OwnerID, or when the caller is an admin so
//     they retain a "see all routers" oversight.
//
// Empty OwnerID for a user-scoped provider is treated as "unbound, never
// visible" so a misconfigured credential cannot leak through the
// picker, even to admins.
func callerCanSee(p gateway.ConsoleUserProvider, caller core.User) bool {
	switch p.Scope {
	case gateway.ScopePublic, "":
		return true
	case gateway.ScopeOrg:
		return true // single-tenant console today; future-proof.
	case gateway.ScopeUser:
		if p.OwnerID == "" {
			return false
		}
		if caller.Role == "admin" {
			return true
		}
		return p.OwnerID == caller.ID
	default:
		return true
	}
}

// responseBuildTag is read by writeJSON and written into the
// X-Nexus-Build header on every JSON response. The default
// ("dev") matches the build pipeline's local default; the linker
// overrides it for release builds via -X main.nexusBuildTag=…
var responseBuildTag = "dev"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Tag every response from this binary so an intervening CDN /
	// ingress / WAF that rewrites the body is detectable from the
	// client. If the response from the operator's console doesn't
	// carry this header, the body has been replaced before it
	// reached the browser — typically an authentication proxy
	// returning a login page, or a CDN error page that is HTML.
	w.Header().Set("X-Nexus-Source", "nexus-console")
	w.Header().Set("X-Nexus-Build", responseBuildTag)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// SetBuildTag overrides the package-level responseBuildTag with
// a value supplied by the caller (cmd/nexus/main.go reads it from
// the linker-injected -X main.nexusBuildTag=…). Empty input is
// ignored so the dev default remains stable.
func SetBuildTag(tag string) {
	if tag == "" {
		return
	}
	responseBuildTag = tag
}

// uiObservability returns the small bundle of operator-facing URLs the
// console sidebar needs to render "Open in Grafana" / "Open in Metabase"
// style shortcuts. Empty fields are omitted entirely so the front-end
// `ui-observability-link` component renders nothing instead of a broken
// href. The endpoint is intentionally anonymous because the URLs are
// non-sensitive (operator-set, public ingress, never carry tenant data).
// Mirrors the runtime env vars NEXUS_PUBLIC_GRAFANA_URL and (added in a
// future PR) NEXUS_PUBLIC_METABASE_URL; only the Grafana side is wired
// up here because Metabase's bundled dashboards ship under Metabase's
// own URL prefix and embed cleanly without an explicit link handler.
func (s *Server) uiObservability(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{}
	if s.grafanaURL != "" {
		// Three bundled reference dashboards — keep the UIDs in sync
		// with the GrafanaDashboard CRs in deploy/helm/nexus/.
		resp["grafana"] = map[string]string{
			"base":     s.grafanaURL,
			"overview": s.grafanaURL + "/d/nexus-01-overview/nexus-01-overview",
			"spend":    s.grafanaURL + "/d/nexus-02-llm-spend/nexus-02-llm-spend",
			"eval":     s.grafanaURL + "/d/nexus-03-eval-quality/nexus-03-eval-quality",
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
