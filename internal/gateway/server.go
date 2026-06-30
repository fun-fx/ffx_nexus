package gateway

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewMux builds the gateway HTTP handler with the middleware plugin chain.
// The chain order mirrors a Bifrost-style pipeline: request id -> recover ->
// logging -> auth -> handler. Inline guardrails slot in after auth.
//
// auth and lim may be nil to run in zero-dependency mode (no enforcement).
func NewMux(h *Handler, auth VKeyAuthenticator, lim Limiter, log *slog.Logger) http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(Recover(log))
	r.Use(Logging(log))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Group(func(r chi.Router) {
		r.Use(Auth(auth))
		r.Use(Enforce(lim))
		r.Post("/v1/chat/completions", h.ChatCompletions)
		r.Post("/v1/responses", h.Responses)
		r.Post("/v1/embeddings", h.Embeddings)
		r.Get("/v1/models", h.Models)
	})

	return r
}
