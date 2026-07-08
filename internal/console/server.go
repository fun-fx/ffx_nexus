package console

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/limiter"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
	nexusweb "github.com/ffxnexus/nexus/web"
)

// RouteStatsSource exposes the router's current rolling per-model stats.
type RouteStatsSource interface {
	Snapshot() map[string]router.ModelStats
}

// Server exposes the dashboard API: recent traces, window stats, a live
// WebSocket feed, routing stats, and (when a store is configured)
// key/credential management.
type Server struct {
	hub             *Hub
	reader          *observability.Reader // may be nil when ClickHouse is not configured
	store           *core.Store           // may be nil when Postgres is not configured
	routes          RouteStatsSource      // may be nil when routing is disabled
	reload          func(context.Context) // may be nil when no hot-reload hook is wired
	allowSignup     bool                  // public POST /api/auth/register
	sso             *ssoClient            // OIDC client; nil when SSO is not configured
	evalConfigSrc   EvalConfigSource  // nil when eval worker is disabled
	evalConfigApply EvalConfigApplier // nil when eval worker is disabled
	loginLim        *limiter.IPLimiter    // per-IP rate limit for /api/auth/login
	registerLim     *limiter.IPLimiter    // per-IP rate limit for /api/auth/register
	ssoLim          *limiter.IPLimiter    // per-IP rate limit for /api/auth/sso/*
	log             *slog.Logger
	up              websocket.Upgrader
}

// SetAllowSignup toggles public self-service registration (member role only).
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

// SetCredentialReloader registers a callback invoked after credential changes
// (rotate/delete) so the gateway can refresh its in-memory providers without a
// restart. Optional; when unset, credential changes apply on next restart.
func (s *Server) SetCredentialReloader(fn func(context.Context)) { s.reload = fn }

// SetEvalConfig wires eval/routing runtime config for GET/PATCH /api/eval/config.
func (s *Server) SetEvalConfig(src EvalConfigSource, apply EvalConfigApplier) {
	s.evalConfigSrc = src
	s.evalConfigApply = apply
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
		r.Get("/stats", s.requireUser(s.stats))
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
		r.Get("/me/quality", s.requireUser(s.myQuality))
		r.Get("/me/keys", s.requireUser(s.listMyKeys))
		r.Post("/me/keys", s.requireUser(s.createMyKey))
		r.Delete("/me/keys/{id}", s.requireUser(s.revokeMyKey))
		r.Get("/me/credentials", s.requireUser(s.listMyCredentials))
		r.Post("/me/credentials", s.requireUser(s.createMyCredential))
		r.Post("/me/credentials/{id}/rotate", s.requireUser(s.rotateMyCredential))
		r.Delete("/me/credentials/{id}", s.requireUser(s.deleteMyCredential))

		// User management (admin only).
		r.Get("/users", s.requireAdmin(s.listUsers))
		r.Post("/users", s.requireAdmin(s.createUser))
		r.Delete("/users/{id}", s.requireAdmin(s.deleteUser))
		r.Get("/users/quality", s.requireAdmin(s.userQuality))
		r.Get("/audit", s.requireAdmin(s.listAudit))

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
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	traces, err := s.reader.RecentTraces(r.Context(), limit, uid)
	if err != nil {
		s.log.Error("recent traces query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if u.Role == core.RoleAdmin {
		s.enrichTraceUserEmails(r.Context(), orgID(r), traces)
	}
	writeJSON(w, http.StatusOK, traces)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
