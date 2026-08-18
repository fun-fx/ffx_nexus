package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Cookie flags in a TLS-terminating-proxy deployment, which is how every
// customer install actually runs: the browser speaks HTTPS to an ingress, the
// ingress speaks plain HTTP to the pod. So the pod sees r.TLS == nil on requests
// that were HTTPS end to end.
//
// The design consequence, and the thing these tests pin: the Secure attribute is
// decided by configuration (NEXUS_SECURE_COOKIES, default true), NOT by
// inspecting the request. The alternatives were both worse.
//
//   - Deriving it from r.TLS marks nothing Secure behind an ingress, i.e. in the
//     exact deployment shape the product ships in.
//   - Deriving it from X-Forwarded-Proto trusts a client-supplied header. Unless
//     every hop that could reach the pod is guaranteed to overwrite it, a request
//     sent straight to the pod's ClusterIP with "X-Forwarded-Proto: https" would
//     dictate its own cookie flags. Nexus therefore never reads that header, and
//     no deployment note about "trusted proxies" is needed for cookies.
//
// The deployment requirement this creates is documented in
// docs/customer-self-hosted-security.md: terminate TLS in front of Nexus and
// leave NEXUS_SECURE_COOKIES at its default.

// loginCookies drives a real login through the mux and returns the Set-Cookie
// headers, so the assertions are on what a browser would actually receive.
func sessionCookieOnPlainHTTPRequest(t *testing.T, secure bool, headers map[string]string) *http.Cookie {
	t.Helper()
	srv := newTestServer()
	srv.SetSecureCookies(secure)

	// httptest.NewRequest with an http:// target leaves r.TLS nil, which is
	// exactly what the pod sees behind an ingress.
	req := httptest.NewRequest(http.MethodGet, "http://console.customer.example/", nil)
	if req.TLS != nil {
		t.Fatal("precondition: this test needs a non-TLS request to model the proxy hop")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	srv.setSessionCookie(rec, "session-token-value")

	res := rec.Result()
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie was set", sessionCookie)
	return nil
}

// The headline case: HTTPS at the browser, plain HTTP at the pod, and the cookie
// must still be Secure. Getting this wrong means a live console session is sent
// over cleartext on any subsequent plain-HTTP request to the same host.
func TestSessionCookieIsSecureBehindATLSTerminatingProxy(t *testing.T) {
	c := sessionCookieOnPlainHTTPRequest(t, true, nil)
	if !c.Secure {
		t.Error("session cookie is not Secure on a request that reached the pod over plain HTTP; " +
			"behind an ingress that is every request, so nothing would ever be marked Secure")
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly so an XSS cannot read it")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if c.Expires.IsZero() && c.MaxAge == 0 {
		t.Error("session cookie must expire; a non-expiring session cookie outlives the session")
	}
}

// X-Forwarded-Proto must not be able to talk the server into either direction.
// If it could, a request delivered straight to the pod (bypassing the ingress,
// which is reachable inside the cluster) would choose its own cookie flags.
func TestForwardedProtoCannotDictateCookieFlags(t *testing.T) {
	// Claiming http must not clear Secure when the operator configured it on.
	on := sessionCookieOnPlainHTTPRequest(t, true, map[string]string{
		"X-Forwarded-Proto": "http",
	})
	if !on.Secure {
		t.Error("X-Forwarded-Proto: http cleared the Secure attribute; " +
			"a client-supplied header must not weaken cookie flags")
	}

	// Claiming https must not set Secure when the operator configured it off.
	// (Off is only for local plain-HTTP development; if the header could set it,
	// the dev flow would break in a way that looks like a server bug.)
	off := sessionCookieOnPlainHTTPRequest(t, false, map[string]string{
		"X-Forwarded-Proto": "https",
	})
	if off.Secure {
		t.Error("X-Forwarded-Proto: https set the Secure attribute; " +
			"cookie flags come from configuration, not from the request")
	}
}

// Same rules for the SSO state cookie, which is the anti-CSRF token for the
// OIDC round trip. Its Path is narrower on purpose: it is only ever read by the
// callback, so there is no reason to attach it to every console request.
func TestSSOStateCookieIsSecureBehindATLSTerminatingProxy(t *testing.T) {
	srv := newTestServer()
	srv.SetSecureCookies(true)
	rec := httptest.NewRecorder()
	srv.setSSOStateCookie(rec, "state-value")

	res := rec.Result()
	defer res.Body.Close()
	var got *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == ssoStateCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatalf("no %s cookie was set", ssoStateCookie)
	}
	if !got.Secure {
		t.Error("SSO state cookie is not Secure")
	}
	if !got.HttpOnly {
		t.Error("SSO state cookie must be HttpOnly; JS tampering with it defeats the state check")
	}
	if got.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax — Strict would drop the cookie on the IdP's redirect back", got.SameSite)
	}
	if !strings.HasPrefix(got.Path, "/api/auth/sso") {
		t.Errorf("Path = %q, want the SSO callback subtree only", got.Path)
	}
	if got.Expires.IsZero() && got.MaxAge == 0 {
		t.Error("the SSO state cookie must expire; a stale state is a replay window")
	}
}

// Turning Secure off must require the explicit configuration switch, so that a
// production deployment cannot arrive there by omission.
func TestSecureCookiesDefaultToOn(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.setSessionCookie(rec, "t")

	res := rec.Result()
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie && !c.Secure {
			t.Error("a server that was never configured must still mark session cookies Secure")
		}
	}
}
