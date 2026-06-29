package console

import (
	"net"
	"net/http"
	"strings"

	"github.com/ffxnexus/nexus/internal/limiter"
)

// securityHeaders is a middleware that adds the recommended browser security
// headers to every response from the console. The headers mirror §4.2 of
// docs/v1.1-design.md and apply to both the SPA and the API.
//
// The CSP is intentionally permissive enough to allow:
//   - self-hosted CSS/JS (the embedded dashboard assets)
//   - the marketing site to POST login state into the console if it ever does
//   - the WSS endpoint used by the live trace stream on the same origin
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
		// CSP: allow same-origin, plus the marketing site (so the marketing
		// → console login handoff works), and the WSS endpoint on the same
		// origin. img/font data: URIs are needed for the dashboard's inline
		// icons and font fallbacks.
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"connect-src 'self' https://nexus.ffx.ai https://app.nexus.ffx.ai wss://app.nexus.ffx.ai; "+
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
// When the limit is hit we return 429 with a JSON body and the same
// security headers (those are set by the outer securityHeaders middleware).
func (s *Server) ipRateLimit(routeName string, lim *limiter.IPLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if ip != "" && !lim.Allow(routeName+":"+ip) {
				w.Header().Set("Retry-After", "60")
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "rate limit exceeded; try again later",
				})
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
