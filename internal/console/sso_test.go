package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestSSORoutesAbsentWhenNotConfigured confirms /api/auth/sso/* is not
// wired up when SetSSO has not been called (the default state). Without
// an IdP the email/password flow is the only sign-in path, and the SSO
// routes should not be enumerable by an attacker scanning for them.
func TestSSORoutesAbsentWhenNotConfigured(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	for _, path := range []string{"/api/auth/sso/login", "/api/auth/sso/callback"} {
		req := httptest.NewRequest(http.MethodGet, path+"?state=x&code=y", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without sso: want 404, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAuthConfigSSOFieldsDefaults makes sure the /api/auth/config payload
// is shaped the way the SPA expects even when SSO is disabled.
func TestAuthConfigSSOFieldsDefaults(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth config: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"sso_enabled":false`,
		`"sso_label":""`,
		`"signup_enabled":false`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("auth config missing %q, got %s", want, body)
		}
	}
}

// TestSSOStateCookieRoundTrip exercises the state cookie helpers used by
// the /login and /callback handlers. The cookie is HttpOnly + Lax and
// scoped to /api/auth/sso so JS on the SPA cannot read or replay it.
func TestSSOStateCookieRoundTrip(t *testing.T) {
	w := httptest.NewRecorder()
	setSSOStateCookie(w, "abc123")

	// Parse the Set-Cookie header that the helper wrote.
	var set string
	for _, c := range w.Result().Cookies() {
		if c.Name == ssoStateCookie {
			set = c.Name + "=" + c.Value
		}
	}
	if set == "" {
		t.Fatal("setSSOStateCookie did not emit the state cookie")
	}

	// Build a request that the helper would receive on the callback and
	// make sure it round-trips the same value.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/callback", nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookie, Value: "abc123"})
	if got := ssoStateFrom(req); got != "abc123" {
		t.Fatalf("ssoStateFrom: want abc123, got %q", got)
	}

	// clearSSOStateCookie should emit MaxAge=-1 so the browser drops it.
	w2 := httptest.NewRecorder()
	clearSSOStateCookie(w2)
	var cleared *http.Cookie
	for _, c := range w2.Result().Cookies() {
		if c.Name == ssoStateCookie {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("clearSSOStateCookie did not emit the state cookie")
	}
	if cleared.MaxAge >= 0 {
		t.Fatalf("clearSSOStateCookie: want MaxAge<0, got %d", cleared.MaxAge)
	}
}

// TestIssuerProvider verifies the issuer -> short-label normalization
// used to compute the sso_provider column. The same IdP signing in via
// different paths should produce the same provider string, otherwise a
// re-login would look like a different IdP and re-bind the user.
func TestIssuerProvider(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://kc.example.com/realms/cozy", "kc.example.com:realms/cozy"},
		{"https://kc.example.com/realms/cozy/", "kc.example.com:realms/cozy"},
		{"https://kc.example.com:8443/realms/cozy", "kc.example.com:realms/cozy"},
		{"https://kc.example.com", "kc.example.com"},
	}
	for _, c := range cases {
		got := issuerProvider(c.in)
		if got != c.want {
			t.Errorf("issuerProvider(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNewStateUniqueness checks that successive states are independent
// random values (i.e. CSRF protection is not silently broken by a
// misconfigured entropy source).
func TestNewStateUniqueness(t *testing.T) {
	a, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	b, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	if a == b {
		t.Fatal("newState returned identical values")
	}
	if u, err := url.Parse("https://x/?s=" + a); err != nil || u.Query().Get("s") == "" {
		t.Fatalf("state not URL-safe: %q (err=%v)", a, err)
	}
}
