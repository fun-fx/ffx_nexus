package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
)

type stubGatewayConfig struct {
	snap GatewayConfigSnapshot
}

func (s *stubGatewayConfig) Snapshot() GatewayConfigSnapshot {
	return s.snap
}

func (s *stubGatewayConfig) Apply(patch GatewayConfigPatch) (GatewayConfigSnapshot, error) {
	if g := patch.Guardrails; g != nil && g.Enabled != nil {
		s.snap.Guardrails.Enabled = *g.Enabled
	}
	if sc := patch.SemanticCache; sc != nil && sc.Enabled != nil {
		s.snap.SemanticCache.Enabled = *sc.Enabled
	}
	return s.snap, nil
}

func TestGetGatewayConfig(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetGatewayConfig(&stubGatewayConfig{
		snap: GatewayConfigSnapshot{},
	}, &stubGatewayConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/config", nil)
	rec := httptest.NewRecorder()
	srv.getGatewayConfig(rec, req, core.User{ID: "u1", Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchGatewayConfig(t *testing.T) {
	stub := &stubGatewayConfig{}
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetGatewayConfig(stub, stub)
	body, _ := json.Marshal(map[string]any{
		"guardrails": map[string]any{"enabled": true},
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/gateway/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.patchGatewayConfig(rec, req, core.User{ID: "u1", Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !stub.snap.Guardrails.Enabled {
		t.Fatal("expected guardrails enabled")
	}
}

func TestPatchGatewayConfigInvalidThreshold(t *testing.T) {
	stub := &stubGatewayConfig{}
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetGatewayConfig(stub, stub)
	bad := 1.5
	body, _ := json.Marshal(GatewayConfigPatch{
		SemanticCache: &struct {
			Enabled    *bool    `json:"enabled"`
			TTL        *string  `json:"ttl"`
			Threshold  *float64 `json:"threshold"`
			MaxEntries *int     `json:"max_entries"`
		}{Threshold: &bad},
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/gateway/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.patchGatewayConfig(rec, req, core.User{ID: "u1", Role: core.RoleAdmin})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}
