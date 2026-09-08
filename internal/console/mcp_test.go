package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/mcp"
)

type stubMCPStore struct{}

func (stubMCPStore) List(context.Context, string) ([]mcp.ServerRecord, error) {
	return []mcp.ServerRecord{{ID: "s1", Name: "demo", SpecYAML: "connection:\n  type: http\n  url: http://x", Enabled: true}}, nil
}
func (stubMCPStore) Get(context.Context, string) (*mcp.ServerRecord, error) {
	return &mcp.ServerRecord{ID: "s1", Name: "demo"}, nil
}
func (stubMCPStore) Save(context.Context, *mcp.ServerRecord) error { return nil }
func (stubMCPStore) Delete(context.Context, string) error          { return nil }

type stubMCPRuntime struct{}

func (stubMCPRuntime) ListAllServers(string) []mcp.ServerStatus {
	return []mcp.ServerStatus{{ServerRecord: mcp.ServerRecord{ID: "s1", Name: "demo"}, State: "healthy"}}
}
func (stubMCPRuntime) Upsert(context.Context, mcp.ServerRecord) error { return nil }
func (stubMCPRuntime) Remove(string)                                  {}
func (stubMCPRuntime) Reconnect(context.Context, string) error        { return nil }
func (stubMCPRuntime) TestConnection(context.Context, string) (int, error) {
	return 1, nil
}

func TestListMCPServers(t *testing.T) {
	srv := NewServer(NewHub(), nil, nil, nil)
	srv.SetMCPServers(stubMCPStore{}, stubMCPRuntime{}, NewMCPHub())
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/servers", nil)
	rec := httptest.NewRecorder()
	srv.listMCPServers(rec, req, core.User{ID: "u1", Role: core.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
