package console

import (
	"net/http"
	"net/url"
	"strings"
)

// originPolicy decides which browser origins may talk to the console with
// credentials attached.
//
// It replaces `AllowedOrigins: ["*"]` combined with `AllowCredentials: true`.
// That pairing is invalid per the Fetch standard — a browser will not honour a
// literal `*` on a credentialed request — but the chi/cors middleware resolves
// `*` by echoing back whatever Origin the request carried, which is strictly
// worse than the standard behaviour: every origin on the internet became an
// allowed origin, and the response said so per-request.
//
// The practical consequence: any page a logged-in admin visited could issue
// credentialed cross-origin calls to the console and read the replies —
// enumerate keys and credentials, create invites, delete users — with the
// browser attaching the session cookie automatically.
//
// The allowlist comes from NEXUS_PUBLIC_WEB_ORIGINS, which already existed for
// the CSP connect-src directive. Reusing it means the operator declares their
// console origins once.
type originPolicy struct {
	// allowed holds normalised origins ("https://console.example.com"). Empty
	// means same-origin only, which is the correct default for a console served
	// from the same host as its API.
	allowed []string

	// devMode additionally accepts http://localhost and http://127.0.0.1 on any
	// port, for `npm run dev` against a locally built binary. Never set in a
	// customer deployment.
	devMode bool
}

func newOriginPolicy(origins []string, devMode bool) *originPolicy {
	p := &originPolicy{devMode: devMode}
	for _, o := range origins {
		if n := normalizeOrigin(o); n != "" {
			p.allowed = append(p.allowed, n)
		}
	}
	return p
}

// normalizeOrigin reduces a configured value to scheme://host[:port], lowercased.
//
// Operators paste URLs, not origins: "https://console.example.com/" and
// "https://console.example.com/login" both mean the same origin. Normalising
// here rather than demanding exactness avoids the failure where a trailing
// slash silently locks the operator out of their own console.
func normalizeOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		// A literal "*" is rejected rather than honoured. Allowing every origin
		// to send credentials is not a configuration Nexus offers, and silently
		// accepting it here would reintroduce the hole this type exists to
		// close.
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	// Reject userinfo instead of discarding it.
	//
	// url.Parse reads "https://evil.example@console.customer.example" as host
	// console.customer.example with user evil.example, so normalising by host
	// alone turned an attacker-authored string into an exact allowlist match.
	// A browser never puts userinfo in an Origin header — the serialisation is
	// scheme://host[:port] — so any value carrying it is malformed as an origin
	// and refusing it costs nothing.
	if u.User != nil {
		return ""
	}
	return scheme + "://" + canonicalHost(scheme, strings.ToLower(u.Host))
}

// canonicalHost drops a port that is the scheme's default.
//
// Per the URL standard, "https://console.example.com:443" and
// "https://console.example.com" are the same origin, and a browser never puts
// the default port in an Origin header. An operator who pastes the explicit form
// into NEXUS_PUBLIC_WEB_ORIGINS would otherwise configure an origin no browser
// can ever send, and be locked out of their own console with a 403 that names
// the variable they just set correctly-looking.
//
// Non-default ports are preserved and must match exactly: an origin on :8443 is
// a different origin from the same host on :443, and treating them as
// interchangeable is the port-confusion bypass this function must not introduce.
func canonicalHost(scheme, host string) string {
	switch {
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	}
	return host
}

// isLocalDevOrigin reports whether an origin is a loopback HTTP origin.
//
// Matched by hostname rather than by string prefix so that
// "http://localhost.attacker.example" — which has the prefix but is not
// loopback — does not pass.
func isLocalDevOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.ToLower(u.Scheme) != "http" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// allows reports whether a cross-origin credentialed request from origin is
// permitted.
func (p *originPolicy) allows(origin string) bool {
	origin = normalizeOrigin(origin)
	if origin == "" {
		return false
	}
	for _, a := range p.allowed {
		if a == origin {
			return true
		}
	}
	if p.devMode && isLocalDevOrigin(origin) {
		return true
	}
	return false
}

// sameOrigin reports whether the request's Origin matches the host it was sent
// to, which is the ordinary case for a console served alongside its API.
//
// The comparison uses the Host header, so a reverse proxy that rewrites Host
// must also rewrite Origin — standard behaviour for every ingress controller.
// X-Forwarded-Host is deliberately not consulted: it is caller-supplied, and
// trusting it would let a request assert its own same-originness.
func sameOrigin(r *http.Request) bool {
	origin := normalizeOrigin(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(r.Host))
	if host == "" {
		return false
	}
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	scheme, originHost := origin[:i], origin[i+3:]
	// The scheme is not in the Host header. Behind a TLS-terminating proxy the
	// request arrives as HTTP while the browser used HTTPS, so comparing schemes
	// would reject every legitimate request. Host equality is the part that
	// carries the security value here; scheme downgrade is what Secure cookies
	// and HSTS address.
	//
	// Both sides are canonicalised with the same scheme so ":443" on one and
	// nothing on the other are not read as different hosts. A forged pair —
	// Host: evil.example with Origin: https://evil.example — does satisfy this,
	// and that is not a weakness: a browser sets Host from the URL it is
	// fetching and an attacker's page cannot make it lie, while a non-browser
	// client that forges both has no ambient session cookie to abuse.
	return canonicalHost(scheme, originHost) == canonicalHost(scheme, host)
}

// permitted reports whether a request's Origin may act on this console with
// credentials. Same-origin always passes; cross-origin needs the allowlist.
func (p *originPolicy) permitted(r *http.Request) bool {
	if sameOrigin(r) {
		return true
	}
	return p.allows(r.Header.Get("Origin"))
}

// requiresOriginCheck reports whether a method changes state and therefore needs
// CSRF protection.
//
// GET/HEAD/OPTIONS are excluded because they must not change state; if one of
// them does, the fix is the handler, not this list. Note that a CORS allowlist
// alone does NOT prevent CSRF: a cross-site form or an <img> can issue a
// credentialed POST without any preflight, and the browser will deliver it. The
// attacker cannot read the response, but for delete-a-user or revoke-a-key the
// response is not the point.
func requiresOriginCheck(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// enforceOrigin rejects state-changing requests that carry a disallowed Origin.
//
// A missing Origin header is allowed through. Browsers attach Origin to every
// POST/PATCH/PUT/DELETE, so its absence means a non-browser client — curl, a CI
// job, a customer's automation — which is not subject to CSRF because there is
// no ambient cookie for an attacker to borrow. Rejecting on absence would break
// every API client while adding nothing: an attacker's page cannot suppress the
// header, so "no Origin" is not a bypass.
func (s *Server) enforceOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresOriginCheck(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if s.origins.permitted(r) {
			next.ServeHTTP(w, r)
			return
		}
		if s.log != nil {
			s.log.Warn("rejected cross-origin state-changing request",
				"origin", origin,
				"method", r.Method,
				"path", r.URL.Path,
				"host", r.Host)
		}
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":  "cross-origin request refused",
			"reason": "this origin is not in NEXUS_PUBLIC_WEB_ORIGINS",
		})
	})
}
