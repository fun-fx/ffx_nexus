package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
)

// RouteStatsSource exposes the router's current rolling per-model stats.
type RouteStatsSource interface {
	Snapshot() map[string]router.ModelStats
}

// Server exposes the dashboard API: recent traces, window stats, a live
// WebSocket feed, routing stats, and (when a store is configured)
// key/credential management.
type Server struct {
	hub    *Hub
	reader *observability.Reader // may be nil when ClickHouse is not configured
	store  *core.Store           // may be nil when Postgres is not configured
	routes RouteStatsSource      // may be nil when routing is disabled
	reload func(context.Context) // may be nil when no hot-reload hook is wired
	log    *slog.Logger
	up     websocket.Upgrader
}

// SetRouteStats attaches a routing stats source for the /api/routing endpoint.
func (s *Server) SetRouteStats(src RouteStatsSource) { s.routes = src }

// SetCredentialReloader registers a callback invoked after credential changes
// (rotate/delete) so the gateway can refresh its in-memory providers without a
// restart. Optional; when unset, credential changes apply on next restart.
func (s *Server) SetCredentialReloader(fn func(context.Context)) { s.reload = fn }

// NewServer builds the console server. reader and store may be nil.
func NewServer(hub *Hub, reader *observability.Reader, store *core.Store, log *slog.Logger) *Server {
	return &Server{
		hub:    hub,
		reader: reader,
		store:  store,
		log:    log,
		up: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// Mux returns the console HTTP handler.
func (s *Server) Mux() http.Handler {
	r := chi.NewRouter()
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
	}))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Route("/api", func(r chi.Router) {
		r.Get("/traces", s.recentTraces)
		r.Get("/stats", s.stats)
		r.Get("/routing", s.routing)
		r.Get("/live", s.live)

		// Key/credential management (requires Postgres).
		r.Get("/keys", s.listKeys)
		r.Post("/keys", s.createKey)
		r.Delete("/keys/{id}", s.revokeKey)
		r.Get("/credentials", s.listCredentials)
		r.Post("/credentials", s.createCredential)
		r.Post("/credentials/{id}/rotate", s.rotateCredential)
		r.Delete("/credentials/{id}", s.deleteCredential)
	})

	return r
}

func (s *Server) recentTraces(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	traces, err := s.reader.RecentTraces(r.Context(), limit)
	if err != nil {
		s.log.Error("recent traces query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, traces)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
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
	st, err := s.reader.WindowStats(r.Context(), window)
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

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ch := s.hub.subscribe()
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
