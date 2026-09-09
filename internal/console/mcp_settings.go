package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/mcp"
)

// MCPSettingsSnapshot is the console-facing org MCP preferences plus
// read-only gateway reference data.
type MCPSettingsSnapshot struct {
	OrgID             string `json:"org_id"`
	DefaultTimeoutMs  int    `json:"default_timeout_ms"`
	DefaultStickyHTTP bool   `json:"default_sticky_http"`
	GatewayBaseURL    string `json:"gateway_base_url"`
	OAuthSessionsEnabled bool `json:"oauth_sessions_enabled"`
	MCPRoutes         struct {
		ListServers string `json:"list_servers"`
		ListTools   string `json:"list_tools"`
		CallTool    string `json:"call_tool"`
	} `json:"mcp_routes"`
}

type mcpSettingsPatch struct {
	DefaultTimeoutMs  *int  `json:"default_timeout_ms"`
	DefaultStickyHTTP *bool `json:"default_sticky_http"`
}

func (s *Server) mcpSettingsSnapshot(orgID string) MCPSettingsSnapshot {
	gw := strings.TrimRight(s.publicGatewayURL, "/")
	if gw == "" {
		gw = "http://localhost:8080"
	}
	out := MCPSettingsSnapshot{
		OrgID:                orgID,
		DefaultTimeoutMs:     60000,
		DefaultStickyHTTP:    true,
		GatewayBaseURL:       gw,
		OAuthSessionsEnabled: false,
	}
	out.MCPRoutes.ListServers = gw + "/v1/mcp/servers"
	out.MCPRoutes.ListTools = gw + "/v1/mcp/servers/{id}/tools/list"
	out.MCPRoutes.CallTool = gw + "/v1/mcp/servers/{id}/tools/call"
	return out
}

func (s *Server) getMCPSettings(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.mcpOrgSettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp settings unavailable"})
		return
	}
	org := orgID(r)
	stored, err := s.mcpOrgSettings.Get(r.Context(), org)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "internal_error", err)
		return
	}
	snap := s.mcpSettingsSnapshot(org)
	snap.DefaultTimeoutMs = stored.DefaultTimeoutMs
	snap.DefaultStickyHTTP = stored.DefaultStickyHTTP
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) patchMCPSettings(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpOrgSettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp settings unavailable"})
		return
	}
	var patch mcpSettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	org := orgID(r)
	stored, err := s.mcpOrgSettings.Upsert(r.Context(), org, patch.DefaultTimeoutMs, patch.DefaultStickyHTTP)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err)
		return
	}
	s.audit(r.Context(), u.ID, org, core.AuditAction("mcp.settings.update"), "", "")
	snap := s.mcpSettingsSnapshot(org)
	snap.DefaultTimeoutMs = stored.DefaultTimeoutMs
	snap.DefaultStickyHTTP = stored.DefaultStickyHTTP
	writeJSON(w, http.StatusOK, snap)
}

// MCPOrgSettingsSource loads per-org MCP defaults.
type MCPOrgSettingsSource interface {
	Get(ctx context.Context, orgID string) (mcp.OrgSettings, error)
	Upsert(ctx context.Context, orgID string, timeoutMs *int, sticky *bool) (mcp.OrgSettings, error)
}
