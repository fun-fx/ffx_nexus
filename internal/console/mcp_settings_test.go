package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/mcp"
)

type stubMCPOrgSettings struct {
	settings mcp.OrgSettings
}

func (s *stubMCPOrgSettings) Get(_ context.Context, orgID string) (mcp.OrgSettings, error) {
	if s.settings.OrgID == "" {
		s.settings.OrgID = orgID
	}
	return s.settings, nil
}

func (s *stubMCPOrgSettings) Upsert(_ context.Context, orgID string, timeoutMs *int, sticky *bool) (mcp.OrgSettings, error) {
	if s.settings.OrgID == "" {
		s.settings.OrgID = orgID
	}
	if timeoutMs != nil {
		s.settings.DefaultTimeoutMs = *timeoutMs
	}
	if sticky != nil {
		s.settings.DefaultStickyHTTP = *sticky
	}
	return s.settings, nil
}

func TestGetMCPSettings(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetPublicGatewayURL("https://gw.example")
	srv.SetMCPOrgSettings(&stubMCPOrgSettings{
		settings: mcp.OrgSettings{DefaultTimeoutMs: 45000, DefaultStickyHTTP: false},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/settings", nil)
	rec := httptest.NewRecorder()
	srv.getMCPSettings(rec, req, core.User{ID: "u1", Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var snap MCPSettingsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.DefaultTimeoutMs != 45000 {
		t.Fatalf("timeout = %d", snap.DefaultTimeoutMs)
	}
	if snap.GatewayBaseURL != "https://gw.example" {
		t.Fatalf("gateway = %q", snap.GatewayBaseURL)
	}
	if snap.OAuthSessionsEnabled {
		t.Fatal("oauth should be false")
	}
}

func TestPatchMCPSettings(t *testing.T) {
	store := &stubMCPOrgSettings{}
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetMCPOrgSettings(store)
	body, _ := json.Marshal(map[string]any{"default_timeout_ms": 120000})
	req := httptest.NewRequest(http.MethodPatch, "/api/mcp/settings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.patchMCPSettings(rec, req, core.User{ID: "u1", Role: core.RoleAdmin, OrgID: "default"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.settings.DefaultTimeoutMs != 120000 {
		t.Fatalf("stored timeout = %d", store.settings.DefaultTimeoutMs)
	}
}
