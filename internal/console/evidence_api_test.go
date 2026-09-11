package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/gateway"
)

type stubCaps struct{ caps []gateway.ProviderCapability }

func (s stubCaps) ProviderCapabilities() []gateway.ProviderCapability { return s.caps }

func TestGatewayCapabilitiesEmpty(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/capabilities", nil)
	rec := httptest.NewRecorder()
	srv.gatewayCapabilities(rec, req, core.User{ID: "u1", Role: core.RoleMember})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Providers []gateway.ProviderCapability `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Providers == nil {
		t.Fatal("providers must be an array")
	}
}

func TestGatewayCapabilitiesListsProviders(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetCapabilitySource(stubCaps{caps: []gateway.ProviderCapability{{Name: "openai", Chat: true, Stream: true, Health: "registered"}}})
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/capabilities", nil)
	rec := httptest.NewRecorder()
	srv.gatewayCapabilities(rec, req, core.User{ID: "u1", Role: core.RoleMember})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !json.Valid(rec.Body.Bytes()) || rec.Body.String() == "" {
		t.Fatal(rec.Body.String())
	}
}

func TestInstallReadinessJSON(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetInstallReadiness(ReadinessFunc(func() InstallReadiness {
		return NewInstallReadiness("dev-test", "in_cluster_only", false, true, false)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/ops/readiness", nil)
	rec := httptest.NewRecorder()
	srv.installReadiness(rec, req, core.User{ID: "u1", Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rep InstallReadiness
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.FailClosed || rep.D2b.ProviderEgressMode != "in_cluster_only" {
		t.Fatalf("%+v", rep)
	}
}

func TestTraceEvidenceWithoutStore(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/traces/abc", nil)
	rec := httptest.NewRecorder()
	srv.getTraceEvidence(rec, req, core.User{ID: "u1", Role: core.RoleMember})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}
