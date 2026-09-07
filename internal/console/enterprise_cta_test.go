package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func authConfigBody(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/config: %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode auth config: %v", err)
	}
	return out
}

// The default is what every self-hosted install gets. A console running
// inside a customer's company must not advertise our sales link because
// nobody remembered to turn it off.
func TestAuthConfigOmitsEnterpriseCtaByDefault(t *testing.T) {
	body := authConfigBody(t, newTestServer())
	if _, ok := body["enterprise_cta_url"]; ok {
		t.Fatalf("auth config advertises a CTA nobody configured: %v", body["enterprise_cta_url"])
	}
}

func TestAuthConfigExposesConfiguredEnterpriseCta(t *testing.T) {
	srv := newTestServer()
	srv.SetEnterpriseCtaURL("https://nexus.ffx.ai/demo")

	if got := authConfigBody(t, srv)["enterprise_cta_url"]; got != "https://nexus.ffx.ai/demo" {
		t.Fatalf("enterprise_cta_url = %v, want https://nexus.ffx.ai/demo", got)
	}
}

// The value lands in an <a href> on the one page every user sees before
// authenticating, so a javascript: URL here is stored XSS.
func TestSetEnterpriseCtaURLRejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(document.cookie)",
		"data:text/html,<script>alert(1)</script>",
		"mailto:ops@example.com",
		"//evil.example.com",
		"not a url at all",
	} {
		srv := newTestServer()
		srv.SetEnterpriseCtaURL(raw)
		if _, ok := authConfigBody(t, srv)["enterprise_cta_url"]; ok {
			t.Fatalf("accepted %q as a CTA URL", raw)
		}
	}
}

func TestSetEnterpriseCtaURLTrimsAndClears(t *testing.T) {
	srv := newTestServer()
	srv.SetEnterpriseCtaURL("  https://example.com/demo  ")
	if got := authConfigBody(t, srv)["enterprise_cta_url"]; got != "https://example.com/demo" {
		t.Fatalf("enterprise_cta_url = %v, want the trimmed URL", got)
	}

	srv.SetEnterpriseCtaURL("")
	if _, ok := authConfigBody(t, srv)["enterprise_cta_url"]; ok {
		t.Fatal("clearing the CTA left it advertised")
	}
}
