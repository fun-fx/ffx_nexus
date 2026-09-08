package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/mcp"
	"github.com/ffxnexus/nexus/internal/observability"
)

type stubMCP struct {
	servers []mcp.ServerStatus
}

func (s stubMCP) ListServers(orgID, vkeyID string) []mcp.ServerStatus { return s.servers }
func (s stubMCP) ListTools(context.Context, string, string, string) ([]mcp.Tool, error) {
	return []mcp.Tool{{Name: "ping"}}, nil
}
func (s stubMCP) CallTool(_ context.Context, _, _ string, _ mcp.CallContext, req mcp.CallRequest) (*mcp.CallResult, error) {
	return &mcp.CallResult{LatencyMs: 1, RawJSON: `{"ok":true}`, Content: req.Name}, nil
}

func TestMCPServersRequiresManager(t *testing.T) {
	h := NewHandler(NewRegistry(), observability.NoopRecorder{}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/servers", nil)
	rec := httptest.NewRecorder()
	h.MCPServers(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestMCPServersLists(t *testing.T) {
	h := NewHandler(NewRegistry(), observability.NoopRecorder{}, nil, nil)
	h.SetMCPManager(stubMCP{servers: []mcp.ServerStatus{{ServerRecord: mcp.ServerRecord{Name: "fs"}}}})
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyOrgID, "default"))
	rec := httptest.NewRecorder()
	h.MCPServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPCallTool(t *testing.T) {
	h := NewHandler(NewRegistry(), observability.NoopRecorder{}, nil, nil)
	h.SetMCPManager(stubMCP{})
	body, _ := json.Marshal(map[string]any{"name": "ping", "arguments": map[string]any{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/servers/s1/tools/call", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyOrgID, "default"))
	rec := httptest.NewRecorder()
	h.MCPCallTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
