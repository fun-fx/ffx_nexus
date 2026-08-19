package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An origin allowlist is only worth what its matcher is worth. These are the
// bypasses that get tried against one: suffix and prefix confusion, case and
// port variation, the literal "null" origin, and non-HTTP schemes.
//
// Every case here is a *negative* test against the exact allowlist
// "https://console.customer.example". A regression that makes any of them pass
// hands a credentialed cross-origin session to an attacker-controlled page.

const allowedConsoleOrigin = "https://console.customer.example"

func TestOriginMatchIsExactSchemeHostAndPort(t *testing.T) {
	p := newOriginPolicy([]string{allowedConsoleOrigin}, false)

	rejected := map[string]string{
		// The classic: the allowed host is a prefix of the attacker's.
		"https://console.customer.example.evil.example": "suffix-appended domain",
		"https://console.customer.example.evil":         "suffix-appended TLD",
		// The allowed host appears as a subdomain of the attacker's zone.
		"https://evil.example/?x=https://console.customer.example": "allowed origin in the path/query",
		"https://console.customer.example.":                        "trailing-dot FQDN form",
		// Prefix confusion in the other direction.
		"https://xconsole.customer.example": "host prefixed",
		"https://customer.example":          "parent domain",
		"https://console.customer.exampleX": "host suffixed",
		// Port variation: a different port is a different origin.
		"https://console.customer.example:8443": "non-default port",
		"https://console.customer.example:8080": "non-default port",
		// Scheme downgrade: http is not https, even for an allowed host.
		"http://console.customer.example": "scheme downgraded to http",
		// Credentials embedded in the authority.
		"https://evil.example@console.customer.example": "userinfo-confused authority",
		// Non-HTTP schemes cannot be an allowed web origin.
		"file://console.customer.example": "file scheme",
		"data:text/html,<script>":         "data scheme",
		"javascript:alert(1)":             "javascript scheme",
		// The opaque origin a sandboxed iframe or a redirected request sends.
		"null": "literal null origin",
		"":     "absent origin",
		// A wildcard is configuration, never a request value.
		"*": "wildcard as a request origin",
	}

	for origin, why := range rejected {
		if p.allows(origin) {
			t.Errorf("allowed %q (%s) against allowlist %q", origin, why, allowedConsoleOrigin)
		}
	}
}

// Case-insensitivity is required, not optional: scheme and host are
// case-insensitive per RFC 3986, and a browser may normalise differently than
// the operator typed. Rejecting these would lock out a legitimate console.
func TestOriginMatchIsCaseInsensitiveOnSchemeAndHost(t *testing.T) {
	p := newOriginPolicy([]string{allowedConsoleOrigin}, false)
	for _, origin := range []string{
		"https://CONSOLE.CUSTOMER.EXAMPLE",
		"HTTPS://console.customer.example",
		"HtTpS://Console.Customer.Example",
	} {
		if !p.allows(origin) {
			t.Errorf("rejected %q; scheme and host are case-insensitive", origin)
		}
	}
}

// The default port is not part of an origin. A browser never sends ":443" in an
// Origin header, so an operator who configures the explicit form would otherwise
// be locked out by a 403 that blames the variable they just set.
func TestDefaultPortsAreCanonicalisedBothWays(t *testing.T) {
	// Configured with the port, requested without.
	p := newOriginPolicy([]string{"https://console.customer.example:443"}, false)
	if !p.allows("https://console.customer.example") {
		t.Error("an allowlist entry written with the default :443 must match the browser's portless Origin")
	}
	// Configured without, requested with.
	p = newOriginPolicy([]string{allowedConsoleOrigin}, false)
	if !p.allows("https://console.customer.example:443") {
		t.Error("an Origin carrying the default :443 must match a portless allowlist entry")
	}
	// And the same for plain http on :80.
	p = newOriginPolicy([]string{"http://console.internal:80"}, true)
	if !p.allows("http://console.internal") {
		t.Error("http default port :80 must canonicalise away")
	}
	// A non-default port must still be significant.
	p = newOriginPolicy([]string{allowedConsoleOrigin}, false)
	if p.allows("https://console.customer.example:8443") {
		t.Error("a non-default port must not be canonicalised away — that is the port-confusion bypass")
	}
}

// The production default. With no allowlist configured, a credentialed
// cross-origin request must be refused; only same-origin passes. This is the
// "operator forgot to set NEXUS_PUBLIC_WEB_ORIGINS" case, and it must fail
// closed rather than fall back to permissive.
func TestEmptyAllowlistRefusesEveryCrossOrigin(t *testing.T) {
	p := newOriginPolicy(nil, false)
	for _, origin := range []string{
		"https://console.customer.example",
		"https://evil.example",
		"http://localhost:3000",
		"null",
	} {
		if p.allows(origin) {
			t.Errorf("an unconfigured allowlist admitted %q", origin)
		}
	}
}

// Loopback is a development affordance and must depend on the explicit dev-mode
// flag, never on the absence of configuration.
func TestLoopbackNeedsDevModeAndIsNotImpliedByAnEmptyAllowlist(t *testing.T) {
	prod := newOriginPolicy(nil, false)
	if prod.allows("http://localhost:5173") {
		t.Error("loopback accepted without dev mode")
	}
	dev := newOriginPolicy(nil, true)
	if !dev.allows("http://localhost:5173") {
		t.Error("dev mode must accept the Vite dev server origin")
	}
	// Even in dev mode, a lookalike host is not loopback.
	for _, origin := range []string{
		"http://localhost.evil.example",
		"http://127.0.0.1.evil.example",
		"https://localhost:5173", // https loopback is not the dev-server case
	} {
		if dev.allows(origin) {
			t.Errorf("dev mode admitted lookalike host %q", origin)
		}
	}
}

// End-to-end through the real mux: the literal "null" Origin that a sandboxed
// iframe or a cross-scheme redirect produces must not be treated as "no origin"
// and waved through the state-change check.
func TestNullOriginCannotChangeState(t *testing.T) {
	srv := newTestServer()
	srv.SetCSPOrigins([]string{allowedConsoleOrigin})
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/keys", nil)
	req.Header.Set("Origin", "null")
	req.Host = "console.customer.example"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("a null-Origin POST returned %d, want 403; "+
			"'null' must not be mistaken for an absent Origin header", rec.Code)
	}
}

// The same-origin fast path must not be satisfiable by an origin that merely
// resembles the request's host.
func TestSameOriginRequiresTheHostToMatchExactly(t *testing.T) {
	cases := []struct {
		host, origin string
		want         bool
	}{
		{"console.customer.example", "https://console.customer.example", true},
		{"console.customer.example", "http://console.customer.example", true}, // TLS-terminating proxy
		{"console.customer.example", "https://console.customer.example.evil.example", false},
		{"console.customer.example", "https://evil.example", false},
		{"console.customer.example", "https://console.customer.example:8443", false},
		{"console.customer.example:8443", "https://console.customer.example", false},
		{"console.customer.example", "null", false},
		{"console.customer.example", "", false},
		{"", "https://console.customer.example", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/keys", nil)
		req.Host = tc.host
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if got := sameOrigin(req); got != tc.want {
			t.Errorf("sameOrigin(host=%q, origin=%q) = %v, want %v", tc.host, tc.origin, got, tc.want)
		}
	}
}

// WebSocket upgrades bypass CORS entirely — the handshake is a GET the browser
// performs without a preflight, and Access-Control-Allow-Origin plays no part.
// So CheckOrigin has to apply the same allowlist independently, and it has to
// reject the same bypass shapes.
func TestWebSocketOriginCheckRejectsTheSameBypasses(t *testing.T) {
	srv := newTestServer()
	srv.SetCSPOrigins([]string{allowedConsoleOrigin})

	check := func(origin, host string) bool {
		req := httptest.NewRequest(http.MethodGet, "/api/live", nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return srv.origins.permitted(req)
	}

	// Allowed: the configured console origin, and same-origin.
	if !check(allowedConsoleOrigin, "gateway.customer.example") {
		t.Error("the configured console origin must be able to open the live socket")
	}
	if !check("https://gateway.customer.example", "gateway.customer.example") {
		t.Error("same-origin must be able to open the live socket")
	}

	for _, origin := range []string{
		"https://console.customer.example.evil.example",
		"https://evil.example",
		"http://console.customer.example",
		"https://console.customer.example:8443",
		"null",
		"*",
	} {
		if check(origin, "gateway.customer.example") {
			t.Errorf("WebSocket upgrade accepted foreign origin %q", origin)
		}
	}
}
