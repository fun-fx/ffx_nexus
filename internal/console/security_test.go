package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersOnAllResponses(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	// Hit a route that exists and one that 404s — both must carry the
	// headers, since the design doc requires them even on errors.
	for _, path := range []string{"/api/auth/config", "/api/does-not-exist"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		must := map[string]string{
			"Strict-Transport-Security": "max-age=63072000",
			"X-Frame-Options":           "DENY",
			"X-Content-Type-Options":    "nosniff",
			"Referrer-Policy":           "strict-origin-when-cross-origin",
		}
		for name, expect := range must {
			got := rec.Header().Get(name)
			if !strings.Contains(got, expect) {
				t.Errorf("%s: want %q in header, got %q", path, expect, got)
			}
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s: CSP missing default-src 'self': %q", path, csp)
		}
	}
}

func TestRateLimitAllowsBurst(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	body := strings.NewReader(`{"email":"a@b.com","password":"long-enough"}`)
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
		req.Header.Set("CF-Connecting-IP", "203.0.113.7")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		// We expect 403 (signup disabled / no store) — anything but 429 is OK
		// for the first 30 requests; the limiter must NOT reject them.
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate-limited (rec=%d)", i+1, rec.Code)
		}
	}
}

func TestRateLimitBlocksAt31(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	body := strings.NewReader(`{"email":"a@b.com","password":"long-enough"}`)
	// Exhaust the bucket.
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
		req.Header.Set("CF-Connecting-IP", "203.0.113.99")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
	}
	// 31st should be 429.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("CF-Connecting-IP", "203.0.113.99")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("31st request: want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Errorf("429 should set Retry-After header")
	}
}

func TestRateLimitPerIPIsolation(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	body := strings.NewReader(`{}`)
	// Drain IP A.
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
		req.Header.Set("CF-Connecting-IP", "198.51.100.10")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
	}
	// IP A is now blocked.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("CF-Connecting-IP", "198.51.100.10")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("IP A should be blocked; got %d", rec.Code)
	}
	// IP B should still be allowed.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("CF-Connecting-IP", "198.51.100.20")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("IP B should be unaffected; got 429")
	}
}

func TestRateLimitPerRouteIsolation(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	body := strings.NewReader(`{}`)
	// Drain the login route.
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
		req.Header.Set("CF-Connecting-IP", "198.51.100.30")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
	}
	// Same IP, but the register route should still be allowed.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", body)
	req.Header.Set("CF-Connecting-IP", "198.51.100.30")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("register route should be unaffected by login route exhaustion; got 429")
	}
}

func TestClientIPPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		remote  string
		want    string
	}{
		{"cf-header", map[string]string{"CF-Connecting-IP": "1.1.1.1"}, "127.0.0.1:0", "1.1.1.1"},
		{"xff-when-no-cf", map[string]string{"X-Forwarded-For": "2.2.2.2, 10.0.0.1"}, "127.0.0.1:0", "2.2.2.2"},
		{"remote-fallback", map[string]string{}, "192.0.2.5:54321", "192.0.2.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP: want %q, got %q", tc.want, got)
			}
		})
	}
}
