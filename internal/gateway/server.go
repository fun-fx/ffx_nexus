package gateway

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// ReadinessReporter supplies the /readyz handler. Declared as an interface here
// rather than importing internal/health so the gateway package keeps no
// dependency on the readiness implementation (and tests can pass nil).
type ReadinessReporter interface {
	Handler() http.HandlerFunc
}

// NewMux builds the gateway HTTP handler with the middleware plugin chain.
// The chain order mirrors a Bifrost-style pipeline: request id -> recover ->
// logging -> auth -> handler. Inline guardrails slot in after auth.
//
// auth and lim may be nil to run in zero-dependency mode (no enforcement).
// concCap may be nil to disable V5 per-vkey concurrency caps.
//
// ready may be nil, in which case /readyz mirrors /healthz. When supplied it
// reports the process's readiness conditions (schema state, required
// dependencies) so Kubernetes withholds traffic from a pod that is running but
// cannot correctly serve — previously indistinguishable from a healthy pod,
// because the only probe was a /healthz that returned "ok" as soon as the
// listener bound.
func NewMux(h *Handler, auth VKeyAuthenticator, lim Limiter, concCap CapIface, ready ReadinessReporter, log *slog.Logger) http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(Recover(log))
	r.Use(Logging(log))

	// Liveness: deliberately dependency-free. Restarting the process does not
	// apply a migration or resurrect a database, so making liveness depend on
	// either would turn a transient dependency failure into a restart storm.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Readiness: "should this pod receive traffic right now".
	if ready != nil {
		r.Get("/readyz", ready.Handler())
	} else {
		r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
	}

	r.Group(func(r chi.Router) {
		r.Use(Auth(auth))
		r.Use(Enforce(lim))
		r.Use(Concurrency(concCap))
		r.Post("/v1/chat/completions", h.ChatCompletions)
		r.Post("/v1/responses", h.Responses)
		r.Post("/v1/embeddings", h.Embeddings)
		r.Post("/v1/moderations", h.Moderations)
		r.Post("/v1/images/generations", h.Images)
		r.Get("/v1/models", h.Models)
	})

	return r
}
