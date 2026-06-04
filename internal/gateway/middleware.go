package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ctxKey string

const (
	ctxKeyRequestID     ctxKey = "request_id"
	ctxKeyOrgID         ctxKey = "org_id"
	ctxKeyVKeyID        ctxKey = "virtual_key_id"
	ctxKeyAllowedModels ctxKey = "allowed_models"
	ctxKeyRPMLimit      ctxKey = "rpm_limit"
	ctxKeyMonthlyBudget ctxKey = "monthly_budget"
)

// AuthResult is what a key authenticator returns for a valid virtual key.
type AuthResult struct {
	OrgID         string
	VKeyID        string
	AllowedModels []string // empty = all models allowed
	RPMLimit      int      // requests/min, 0 = unlimited
	MonthlyBudget float64  // USD/month, 0 = unlimited
}

// VKeyAuthenticator validates a presented virtual key. Returning an error means
// the key is invalid/unknown. A nil authenticator disables enforcement
// (zero-dependency mode): requests pass through as the default org.
type VKeyAuthenticator func(ctx context.Context, plaintext string) (AuthResult, error)

// Limiter enforces per-key request rate and monthly budget. A nil Limiter
// disables enforcement.
type Limiter interface {
	Allow(ctx context.Context, keyID string, rpmLimit int) (bool, error)
	MonthlySpend(ctx context.Context, keyID string) (float64, error)
	AddSpend(ctx context.Context, keyID string, costUSD float64) error
}

// RequestID assigns a unique id to each request for tracing/correlation.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recover converts panics into 500 responses so one bad request can't crash the
// process.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered", "err", rec, "path", r.URL.Path)
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging emits a structured access log per request.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"dur_ms", time.Since(start).Milliseconds(),
				"request_id", r.Context().Value(ctxKeyRequestID),
			)
		})
	}
}

// Auth validates the bearer token (virtual key) using the given authenticator.
// When auth is nil the gateway runs in zero-dependency mode: requests pass
// through as the default org without enforcement. When auth is set, a valid
// "Authorization: Bearer <virtual key>" is required.
func Auth(auth VKeyAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth == nil {
				ctx := context.WithValue(r.Context(), ctxKeyOrgID, "default")
				ctx = context.WithValue(ctx, ctxKeyVKeyID, "")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			token := bearerToken(r)
			if token == "" {
				writeError(w, http.StatusUnauthorized, "authentication_error", "missing or malformed Authorization header")
				return
			}
			res, err := auth(r.Context(), token)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid virtual key")
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyOrgID, res.OrgID)
			ctx = context.WithValue(ctx, ctxKeyVKeyID, res.VKeyID)
			ctx = context.WithValue(ctx, ctxKeyAllowedModels, res.AllowedModels)
			ctx = context.WithValue(ctx, ctxKeyRPMLimit, res.RPMLimit)
			ctx = context.WithValue(ctx, ctxKeyMonthlyBudget, res.MonthlyBudget)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Enforce applies per-key RPM rate limits and monthly budget caps. It runs
// after Auth. A nil limiter (or unauthenticated request) disables enforcement.
func Enforce(lim Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			vkeyID, _ := r.Context().Value(ctxKeyVKeyID).(string)
			if lim == nil || vkeyID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Monthly budget check (402 when exhausted).
			if budget, _ := r.Context().Value(ctxKeyMonthlyBudget).(float64); budget > 0 {
				spent, err := lim.MonthlySpend(r.Context(), vkeyID)
				if err == nil && spent >= budget {
					writeError(w, http.StatusPaymentRequired, "budget_exceeded",
						"monthly budget exhausted for this virtual key")
					return
				}
			}

			// RPM rate limit (429 when over).
			rpm, _ := r.Context().Value(ctxKeyRPMLimit).(int)
			allowed, err := lim.Allow(r.Context(), vkeyID, rpm)
			if err == nil && !allowed {
				w.Header().Set("Retry-After", "60")
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					"requests-per-minute limit exceeded for this virtual key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from an Authorization: Bearer header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush proxies to the underlying ResponseWriter so SSE streaming works through
// the middleware chain.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
