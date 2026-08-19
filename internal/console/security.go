package console

import (
	"net"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/limiter"
	"github.com/ffxnexus/nexus/internal/resp"
)

// recoverPanics turns a panic in any console handler into a 500 with a request
// id, instead of a closed connection with no response at all.
//
// Two reasons this is a security control and not just tidiness. A dropped
// connection is indistinguishable from an infrastructure fault, so a
// reachable-by-anyone crash in an unauthenticated handler can be mistaken for
// flaky networking and go unfixed. And a panicking handler has abandoned its
// work midway: without a definite status code, a caller cannot tell a refusal
// from a partial success.
//
// The response goes through resp.HTTP so the body shape is the public
// apierr contract ({code, message, request_id}) and the same X-Request-Id
// is stamped on the response header. The panic value and stack are NOT
// in the body; the operator's slog carries them keyed by the same
// request id so an admin can correlate a user report to a stack trace
// without the crash detail being echoed to whoever triggered it.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A client that hangs up mid-request makes net/http panic with
			// ErrAbortHandler by design; it is not a defect and the connection
			// is already gone, so re-panic and let net/http handle it quietly.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			id := requestIDFrom(r)
			if s.log != nil {
				s.log.Error("console handler panic",
					"err", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", id,
					"stack", string(debug.Stack()),
				)
			}
			// resp.HTTP guarantees the body is apierr.Body shape with
			// X-Request-Id stamped. The panic value is part of the slog
			// entry above; the body is bounded to the public contract.
			resp.HTTP(w, r, 0, apierr.CodeInternalError, id, nil, s.log)
		}()
		next.ServeHTTP(w, r)
	})
}

// requestIDFrom prefers an id the caller's proxy already assigned, so a console
// 500 can be traced through the same identifier that appears in the customer's
// ingress logs. Falls back to a fresh one when nothing upstream set it.
func requestIDFrom(r *http.Request) string {
	for _, h := range []string{"X-Request-Id", "X-Request-ID", "X-Correlation-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			// Bounded and sanitised: this value is echoed in a JSON body and
			// written to logs, so an attacker-supplied header must not be able
			// to inject newlines or grow a log line without limit.
			return sanitizeRequestID(v)
		}
	}
	return uuid.NewString()
}

func sanitizeRequestID(v string) string {
	const max = 64
	if len(v) > max {
		v = v[:max]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return -1
		}
	}, v)
}

// securityHeaders is a middleware that adds the recommended browser security
// headers to every response from the console. The headers mirror §4.2 of
// docs/v1.1-design.md and apply to both the SPA and the API.
//
// The CSP is intentionally permissive enough to allow:
//   - self-hosted CSS/JS (the embedded dashboard assets)
//   - the operator-supplied web origins (marketing → console login handoff
//     and the WSS endpoint used by the live trace stream). These come from
//     `Server.SetCSPOrigins`, which the runtime wires from
//     `Config.PublicWebOrigins` (NEXUS_PUBLIC_WEB_ORIGINS). The list is
//     tenant-supplied so Nexus does not hardcode any company's hostname —
//     an empty list degrades the policy to 'self' only, which is fine for
//     on-prem Helm deploys that do not run a separate marketing site.
//
// The headers are set before any handler runs, so even 401/403/429 responses
// carry them.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// CSP: allow same-origin, plus the operator-supplied web origins
		// (so the marketing → console login handoff works), and the WSS
		// endpoint on the same origin. img/font data: URIs are needed for
		// the dashboard's inline icons and font fallbacks.
		//
		// Origins come from SetCSPOrigins (defaults to a single empty list).
		// Both http and https variants of each origin are admitted; Secure
		// WebSocket URLs ("wss://" on the same host) follow automatically.
		extra := ""
		for _, o := range s.cspOrigins {
			if o == "" {
				continue
			}
			extra += " " + o
			if strings.HasPrefix(o, "https://") {
				extra += " " + strings.Replace(o, "https://", "wss://", 1)
			}
		}
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"connect-src 'self'"+extra+"; "+
				"style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; "+
				"img-src 'self' data:; "+
				"font-src 'self' data:; "+
				"frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ipRateLimit returns a middleware that enforces a per-IP rate limit on the
// protected routes. The bucket key is "<routeName>:<clientIP>". This means
// an attacker cannot drain all routes at once; the 30/min budget is per
// route per IP. limit/minute comes from the design doc §4.2.5.
//
// When the limit is hit we return 429 with the apierr.Body shape so a
// customer's SDK can branch on `code=rate_limited`. The security headers
// are set by the outer securityHeaders middleware, so they survive.
func (s *Server) ipRateLimit(routeName string, lim *limiter.IPLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if ip != "" && !lim.Allow(routeName+":"+ip) {
				w.Header().Set("Retry-After", "60")
				resp.HTTP(w, r, http.StatusTooManyRequests, apierr.CodeRateLimited, requestIDFrom(r), nil, s.log)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the best-effort source IP for rate-limiting purposes.
// Behind Cloudflare the canonical header is CF-Connecting-IP; behind other
// reverse proxies X-Forwarded-For may also be set. We deliberately trust
// only the first hop we recognise — Nexus must not be exposed to the public
// internet without a proxy that sets one of these.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// Take the leftmost (original client) address.
		if i := strings.Index(v, ","); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
