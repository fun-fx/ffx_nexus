package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Verifies that Server.SetCSPOrigins wires an operator-supplied allow-list
// into the securityHeaders middleware's Content-Security-Policy header, so
// Helm deploys of other companies no longer have to scrub ffx.ai literals
// from the binary to admit their own marketing / SPA origin.
//
// The middleware is in security.go; we exercise it through the Server
// constructor path used elsewhere in the test suite (NewServer) and
// observe the rendered CSP.
func TestSecurity_CSPOriginsHoist(t *testing.T) {
	cases := []struct {
		name    string
		origins []string
		want    string
		notWant string
	}{
		{
			name:    "empty list degrades to self-only",
			origins: nil,
			want:    "connect-src 'self';",
			notWant: "ffx.ai",
		},
		{
			name:    "single origin renders as connect-src entry",
			origins: []string{"https://marketing.example.com"},
			// The middleware also emits a wss:// variant per https:// origin
			// so the live-trace WebSocket on the same host sails through.
			want:    "https://marketing.example.com wss://marketing.example.com",
			notWant: "ffx.ai",
		},
		{
			name:    "https origin auto-generates wss variant",
			origins: []string{"https://trace.example.com"},
			want:    "wss://trace.example.com",
			notWant: "ffx.ai",
		},
		{
			name: "trim whitespace and trailing slash",
			origins: []string{
				"  https://marketing.example.com/  ",
				"https://trace.example.com",
				"   ",
			},
			want: "marketing.example.com",
		},
		{
			name:    "multiple origins all appear in CSP (with wss variants)",
			origins: []string{"https://a.example.com", "https://b.example.com"},
			// wss variants altered test: order must include both
			// https and wss for each. Substring match keeps the
			// expectation concise.
			want: "https://a.example.com wss://a.example.com https://b.example.com wss://b.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			srv.SetCSPOrigins(tc.origins)

			h := srv.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest("GET", "/healthz", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get("Content-Security-Policy")
			if got == "" {
				t.Fatalf("expected non-empty CSP header")
			}
			if !containsSubstring(got, tc.want) {
				t.Errorf("CSP missing %q\n  got: %s", tc.want, got)
			}
			if tc.notWant != "" && containsSubstring(got, tc.notWant) {
				t.Errorf("CSP still contains %q — hardcoded domain was not removed\n  got: %s", tc.notWant, got)
			}
		})
	}
}

// containsSubstring is a tiny strings.Contains wrapper to keep the test
// compact, named distinctly to avoid colliding with the helper in
// trace_query_test.go (same package).
func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
