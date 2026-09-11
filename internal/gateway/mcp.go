package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/mcp"
	"github.com/ffxnexus/nexus/internal/resp"
)

// MCPManager is the gateway-facing MCP runtime surface.
type MCPManager interface {
	ListServers(orgID, virtualKeyID string) []mcp.ServerStatus
	ListTools(ctx context.Context, orgID, serverID, virtualKeyID string) ([]mcp.Tool, error)
	CallTool(ctx context.Context, orgID, serverID string, cc mcp.CallContext, req mcp.CallRequest) (*mcp.CallResult, error)
}

// SetMCPManager wires the MCP proxy runtime.
func (h *Handler) SetMCPManager(m MCPManager) {
	if h != nil {
		h.mcp = m
	}
}

// MCPServers lists MCP servers visible to the caller's virtual key.
func (h *Handler) MCPServers(w http.ResponseWriter, r *http.Request) {
	if h.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "mcp_disabled", "MCP is not configured")
		return
	}
	orgID := OrgIDFrom(r.Context())
	vkeyID, _ := r.Context().Value(ctxKeyVKeyID).(string)
	servers := h.mcp.ListServers(orgID, vkeyID)
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// MCPListTools proxies tools/list for one server.
func (h *Handler) MCPListTools(w http.ResponseWriter, r *http.Request) {
	if h.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "mcp_disabled", "MCP is not configured")
		return
	}
	serverID := chi.URLParam(r, "id")
	orgID := OrgIDFrom(r.Context())
	vkeyID, _ := r.Context().Value(ctxKeyVKeyID).(string)
	tools, err := h.mcp.ListTools(r.Context(), orgID, serverID, vkeyID)
	if err != nil {
		h.writeMCPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

// MCPCallTool proxies tools/call for one server.
func (h *Handler) MCPCallTool(w http.ResponseWriter, r *http.Request) {
	if h.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "mcp_disabled", "MCP is not configured")
		return
	}
	serverID := chi.URLParam(r, "id")
	var req mcp.CallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, string(apierr.CodeInvalidRequest), "invalid JSON body")
		return
	}
	cc := mcp.CallContext{
		OrgID:        OrgIDFrom(r.Context()),
		UserID:       UserIDFrom(r.Context()),
		VirtualKeyID: vkeyIDFrom(r.Context()),
		RequestID:    requestIDFrom(r.Context()),
	}
	result, err := h.mcp.CallTool(r.Context(), cc.OrgID, serverID, cc, req)
	if err != nil {
		if result != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":      err.Error(),
				"result":     result,
				"latency_ms": result.LatencyMs,
			})
			return
		}
		h.writeMCPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) writeMCPError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == mcp.ErrServerNotFound:
		writeError(w, r, http.StatusNotFound, "mcp_not_found", err.Error())
	case err == mcp.ErrToolNotAllowed:
		writeError(w, r, http.StatusForbidden, string(apierr.CodeForbidden), err.Error())
	case strings.Contains(err.Error(), "virtual key not allowed"):
		writeError(w, r, http.StatusForbidden, string(apierr.CodeForbidden), err.Error())
	default:
		writeError(w, r, http.StatusBadGateway, "mcp_error", err.Error())
	}
}

func vkeyIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyVKeyID).(string)
	return v
}

func requestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(resp.RequestIDKey()).(string); ok && id != "" {
		return id
	}
	return uuid.NewString()
}
