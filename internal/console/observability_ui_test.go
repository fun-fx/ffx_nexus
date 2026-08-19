package console

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
)

// The console's fetchUIObservability() (web/src/api.ts) expects
// {"grafana":{"base","overview","spend","eval"}} or {} — these tests pin that
// contract so a rename on either side breaks here rather than silently
// reverting the sidebar link to invisible, which is how the missing backend
// went unnoticed in the first place.

func TestObservabilityUI_UnsetReportsNoGrafana(t *testing.T) {
	s := NewServer(nil, nil, nil, slog.Default())

	rec := httptest.NewRecorder()
	s.observabilityUI(rec, httptest.NewRequest(http.MethodGet, "/api/ui/observability", nil), core.User{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got uiObservability
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %q)", err, rec.Body.String())
	}
	if got.Grafana != nil {
		t.Fatalf("Grafana = %+v, want nil when NEXUS_PUBLIC_GRAFANA_URL is unset", got.Grafana)
	}
	// `omitempty` must actually drop the key so the client's optional-chaining
	// on `o.grafana?.base` sees undefined rather than a zero-valued object.
	if strings.Contains(rec.Body.String(), "grafana") {
		t.Fatalf("body %q should omit the grafana key entirely", rec.Body.String())
	}
}

func TestObservabilityUI_ComposesDeepLinks(t *testing.T) {
	s := NewServer(nil, nil, nil, slog.Default())
	// Trailing slash on purpose: link composition must not double it.
	s.SetPublicGrafanaURL("https://grafana.customer.example/")

	rec := httptest.NewRecorder()
	s.observabilityUI(rec, httptest.NewRequest(http.MethodGet, "/api/ui/observability", nil), core.User{})

	var got uiObservability
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Grafana == nil {
		t.Fatal("Grafana = nil, want populated")
	}
	for name, pair := range map[string][2]string{
		"base":     {got.Grafana.Base, "https://grafana.customer.example"},
		"overview": {got.Grafana.Overview, "https://grafana.customer.example/d/nexus-01-overview/nexus-01-overview"},
		"spend":    {got.Grafana.Spend, "https://grafana.customer.example/d/nexus-02-llm-spend/nexus-02-llm-spend"},
		"eval":     {got.Grafana.Eval, "https://grafana.customer.example/d/nexus-03-eval-quality/nexus-03-eval-quality"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
		if strings.Contains(strings.TrimPrefix(pair[0], "https://"), "//") {
			t.Errorf("%s = %q contains a doubled slash", name, pair[0])
		}
	}
}

// A non-absolute or scheme-abusing value would end up in an href rendered in
// the operator's browser, so it is dropped rather than stored.
func TestObservabilityUI_RejectsNonHTTPBase(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"grafana.customer.example",
		"//grafana.customer.example",
		"data:text/html,<script>alert(1)</script>",
	} {
		s := NewServer(nil, nil, nil, slog.Default())
		s.SetPublicGrafanaURL(raw)
		if s.publicGrafanaURL != "" {
			t.Errorf("SetPublicGrafanaURL(%q) stored %q, want it dropped", raw, s.publicGrafanaURL)
		}
	}
}

func TestObservabilityUI_AcceptsPlainHTTPForInClusterGrafana(t *testing.T) {
	// Plenty of in-cluster Grafanas are reached over http:// through an
	// internal-only ingress; refusing that would force operators to lie.
	s := NewServer(nil, nil, nil, slog.Default())
	s.SetPublicGrafanaURL("http://grafana.monitoring.svc.cluster.local:3000")
	if s.publicGrafanaURL == "" {
		t.Fatal("plain http:// base was rejected, want accepted")
	}
}

// The route must require a session: the value names internal infrastructure.
func TestObservabilityUI_RouteRequiresSession(t *testing.T) {
	s := NewServer(nil, nil, nil, slog.Default())
	s.SetPublicGrafanaURL("https://grafana.customer.example")

	rec := httptest.NewRecorder()
	s.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ui/observability", nil))

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200 for anonymous request, want 401/403/503")
	}
	if strings.Contains(rec.Body.String(), "grafana.customer.example") {
		t.Fatalf("anonymous response leaked the Grafana host: %q", rec.Body.String())
	}
}
