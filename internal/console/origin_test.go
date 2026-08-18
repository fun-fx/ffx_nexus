package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console previously ran `AllowedOrigins: ["*"]` together with
// `AllowCredentials: true`. chi/cors resolves the wildcard by echoing the
// request's own Origin back alongside Access-Control-Allow-Credentials: true, so
// every site on the internet was an allowed credentialed origin. Any page a
// logged-in admin visited could read their keys, credentials, users and audit
// log, and issue writes, using the cookie the browser attached automatically.
func TestCORSDoesNotEchoArbitraryOrigins(t *testing.T) {
	srv := newTestServer()
	srv.SetCSPOrigins([]string{"https://console.customer.example"})
	mux := srv.Mux()

	hostile := "https://evil.example"
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Origin", hostile)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for a non-allowlisted origin; "+
			"the browser would let %s read the response", got, hostile)
	}
	if strings.EqualFold(rec.Header().Get("Access-Control-Allow-Credentials"), "true") &&
		rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("credentialed cross-origin access granted to a non-allowlisted origin")
	}
}

func TestCORSAllowsTheConfiguredOrigin(t *testing.T) {
	const allowed = "https://console.customer.example"
	srv := newTestServer()
	srv.SetCSPOrigins([]string{allowed})
	mux := srv.Mux()

	// Preflight, which is what a browser sends before a credentialed PATCH.
	req := httptest.NewRequest(http.MethodOptions, "/api/me", nil)
	req.Header.Set("Origin", allowed)
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowed {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q — the operator's own console is locked out", got, allowed)
	}
	if !strings.EqualFold(rec.Header().Get("Access-Control-Allow-Credentials"), "true") {
		t.Error("credentials not allowed for the configured origin; session cookies would not be sent")
	}
}

// A literal "*" in the operator's allowlist must not be honoured. Accepting it
// would let a single copy-pasted value reopen the hole.
func TestWildcardOriginIsRejectedAsConfiguration(t *testing.T) {
	p := newOriginPolicy([]string{"*"}, false)
	for _, origin := range []string{"https://evil.example", "http://localhost:3000", "*"} {
		if p.allows(origin) {
			t.Errorf("a configured %q allowed origin %q", "*", origin)
		}
	}
}

// CORS governs what a script may READ. A cross-site form or image can still
// ISSUE a credentialed POST with no preflight at all, and for revoke-a-key or
// delete-a-user the attacker does not need to read the response. So
// state-changing requests are checked server-side.
func TestCrossOriginStateChangingRequestsAreRefused(t *testing.T) {
	srv := newTestServer()
	srv.SetCSPOrigins([]string{"https://console.customer.example"})
	mux := srv.Mux()

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/auth/login", strings.NewReader(`{}`))
			req.Header.Set("Origin", "https://evil.example")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("%s from a hostile origin returned %d, want 403: %s",
					method, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// Reads are not blocked by the Origin check — CORS already stops the response
// from being read cross-origin, and blocking them here would break same-origin
// navigation in ways that are hard to debug.
func TestCrossOriginReadsAreNotBlockedByTheOriginCheck(t *testing.T) {
	srv := newTestServer()
	srv.SetCSPOrigins([]string{"https://console.customer.example"})
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Error("GET was refused by the Origin check; CORS is the control for reads")
	}
}

// Same-origin writes must work with no allowlist configured at all, which is the
// ordinary single-host deployment.
func TestSameOriginStateChangingRequestsArePermitted(t *testing.T) {
	mux := newTestServer().Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
	req.Host = "console.customer.example"
	req.Header.Set("Origin", "https://console.customer.example")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Errorf("same-origin POST refused: %s", strings.TrimSpace(rec.Body.String()))
	}
}

// Non-browser clients (curl, CI, customer automation) send no Origin. They are
// not subject to CSRF because there is no ambient cookie for a hostile page to
// borrow, and rejecting them would break every API client while stopping nothing:
// an attacker's page cannot suppress the Origin header.
func TestRequestsWithoutAnOriginHeaderAreAllowed(t *testing.T) {
	mux := newTestServer().Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Errorf("a request with no Origin was refused; API clients would break: %s",
			strings.TrimSpace(rec.Body.String()))
	}
}

// Loopback HTTP origins are accepted only in dev mode.
func TestLoopbackOriginsRequireDevMode(t *testing.T) {
	prod := newOriginPolicy(nil, false)
	dev := newOriginPolicy(nil, true)

	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:5173"} {
		if prod.allows(origin) {
			t.Errorf("production policy accepted loopback origin %q", origin)
		}
		if !dev.allows(origin) {
			t.Errorf("dev policy rejected loopback origin %q", origin)
		}
	}
}

// "http://localhost.evil.example" has the prefix but is not loopback. Matching
// by prefix rather than by hostname would hand dev-mode trust to an attacker who
// controls a subdomain-shaped name.
func TestDevModeDoesNotTrustLookalikeLoopbackHosts(t *testing.T) {
	dev := newOriginPolicy(nil, true)
	for _, origin := range []string{
		"http://localhost.evil.example",
		"http://127.0.0.1.evil.example",
		"https://localhost-evil.example",
		"http://notlocalhost",
	} {
		if dev.allows(origin) {
			t.Errorf("dev policy accepted lookalike origin %q", origin)
		}
	}
}

// Operators paste URLs, not origins. A trailing slash or a path must not lock
// them out of their own console.
func TestConfiguredOriginsAreNormalized(t *testing.T) {
	p := newOriginPolicy([]string{
		"https://console.customer.example/",
		"HTTPS://Upper.Example",
		"https://withpath.example/login",
	}, false)

	for _, origin := range []string{
		"https://console.customer.example",
		"https://upper.example",
		"https://withpath.example",
	} {
		if !p.allows(origin) {
			t.Errorf("normalisation dropped %q", origin)
		}
	}
}

// Non-HTTP schemes cannot be a browser origin for these purposes; accepting them
// would widen the allowlist for no benefit.
func TestNonHTTPSchemesAreRejected(t *testing.T) {
	p := newOriginPolicy([]string{"file://", "javascript:alert(1)", "chrome-extension://abc"}, false)
	if len(p.allowed) != 0 {
		t.Errorf("non-HTTP schemes were accepted into the allowlist: %v", p.allowed)
	}
}

// WebSocket upgrades bypass CORS entirely: the browser sends the handshake with
// cookies and no preflight. CheckOrigin is therefore the only control, and it
// used to return true unconditionally — so a hostile page could stream a
// logged-in admin's live traces, prompts and responses included.
func TestWebSocketUpgradeRejectsForeignOrigins(t *testing.T) {
	srv := newTestServer()
	srv.SetCSPOrigins([]string{"https://console.customer.example"})

	check := srv.up.CheckOrigin
	if check == nil {
		t.Fatal("CheckOrigin is nil: gorilla/websocket then defaults to same-origin, but relying on that implicitly is not a decision")
	}

	hostile := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	hostile.Host = "console.customer.example"
	hostile.Header.Set("Origin", "https://evil.example")
	if check(hostile) {
		t.Error("WebSocket upgrade accepted from a hostile origin")
	}

	allowed := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	allowed.Host = "console.customer.example"
	allowed.Header.Set("Origin", "https://console.customer.example")
	if !check(allowed) {
		t.Error("WebSocket upgrade refused for the configured console origin")
	}

	// Non-browser clients omit Origin and cannot be driven by a hostile page.
	noOrigin := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	noOrigin.Host = "console.customer.example"
	if !check(noOrigin) {
		t.Error("WebSocket upgrade refused for a client that sent no Origin")
	}
}

// A Server built without SetCSPOrigins must be same-origin-only, not
// allow-everything. A nil policy would also panic in the middleware.
func TestZeroValueServerIsSameOriginOnly(t *testing.T) {
	srv := newTestServer()
	if srv.origins == nil {
		t.Fatal("origins policy is nil; the middleware would panic")
	}
	if srv.origins.allows("https://evil.example") {
		t.Error("a Server with no configured origins allowed an arbitrary origin")
	}
	if !srv.secureCookies {
		t.Error("secureCookies must default to true, so a forgotten setter is the safe setting")
	}
}

// --- cookies ---------------------------------------------------------------

// The session cookie previously had no Secure attribute, so a browser would send
// a live admin session over plain HTTP.
func TestSessionCookieAttributes(t *testing.T) {
	srv := newTestServer()
	srv.SetSecureCookies(true)

	rec := httptest.NewRecorder()
	srv.setSessionCookie(rec, "session-token")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want exactly one cookie, got %d", len(cookies))
	}
	c := cookies[0]

	if c.Name != sessionCookie {
		t.Errorf("cookie name = %q, want %q", c.Name, sessionCookie)
	}
	if !c.HttpOnly {
		t.Error("HttpOnly not set: an XSS bug in the SPA could read the session token")
	}
	if !c.Secure {
		t.Error("Secure not set: the browser would send a live session over plain HTTP")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax (blocks cross-site POSTs; Strict would break the OIDC redirect back)", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want / (the SPA and API share the origin)", c.Path)
	}
	if c.Expires.IsZero() && c.MaxAge == 0 {
		t.Error("no expiry: a session cookie must not be a permanent credential")
	}
}

func TestSessionCookieSecureCanBeDisabledForLocalHTTP(t *testing.T) {
	srv := newTestServer()
	srv.SetSecureCookies(false)

	rec := httptest.NewRecorder()
	srv.setSessionCookie(rec, "tok")

	c := rec.Result().Cookies()[0]
	if c.Secure {
		t.Error("Secure still set with secureCookies=false; local HTTP development could not log in")
	}
	// Everything else must stay put: only the TLS-dependent attribute relaxes.
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Error("disabling Secure must not relax HttpOnly or SameSite")
	}
}

// The OIDC state cookie proves the authorization code arriving at the callback
// belongs to the login this browser started, so it needs the same protection.
func TestSSOStateCookieAttributes(t *testing.T) {
	srv := newTestServer()
	srv.SetSecureCookies(true)

	rec := httptest.NewRecorder()
	srv.setSSOStateCookie(rec, "state-value")

	c := rec.Result().Cookies()[0]
	if !c.HttpOnly {
		t.Error("HttpOnly not set on the OIDC state cookie")
	}
	if !c.Secure {
		t.Error("Secure not set on the OIDC state cookie")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax; Strict withholds the cookie on the IdP's cross-site redirect back and every SSO login fails on state mismatch", c.SameSite)
	}
	if !strings.HasPrefix(c.Path, "/api/auth/sso") {
		t.Errorf("Path = %q, want the cookie scoped to the SSO endpoints", c.Path)
	}
	if c.Expires.IsZero() && c.MaxAge == 0 {
		t.Error("the state cookie must expire; it is valid only for one login attempt")
	}
}
