package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/mcp"
)

// MCPServerSource is the admin CRUD surface for MCP servers.
type MCPServerSource interface {
	List(ctx context.Context, orgID string) ([]mcp.ServerRecord, error)
	Get(ctx context.Context, id string) (*mcp.ServerRecord, error)
	Save(ctx context.Context, r *mcp.ServerRecord) error
	Delete(ctx context.Context, id string) error
}

// MCPRuntime manages live MCP connections.
type MCPRuntime interface {
	ListAllServers(orgID string) []mcp.ServerStatus
	Upsert(ctx context.Context, rec mcp.ServerRecord) error
	Remove(id string)
	Reconnect(ctx context.Context, id string) error
	TestConnection(ctx context.Context, id string) (int, error)
}

type mcpServerBody struct {
	Name     string `json:"name"`
	SpecYAML string `json:"spec_yaml"`
	Enabled  bool   `json:"enabled"`
}

func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp disabled"})
		return
	}
	records, err := s.mcpStore.List(r.Context(), orgID(r))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	statuses := s.mcpRuntimeStatuses(orgID(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"servers":  records,
		"statuses": statuses,
	})
}

func (s *Server) createMCPServer(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp disabled"})
		return
	}
	var body mcpServerBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if _, err := mcp.DecodeSpec([]byte(body.SpecYAML)); err != nil {
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	rec := &mcp.ServerRecord{
		OrgID:    normalizeMCPOrg(orgID(r)),
		Name:     body.Name,
		SpecYAML: body.SpecYAML,
		Enabled:  body.Enabled,
	}
	if err := s.mcpStore.Save(r.Context(), rec); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	if s.mcpRuntime != nil {
		_ = s.mcpRuntime.Upsert(r.Context(), *rec)
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("mcp.server.create"), rec.ID, rec.Name)
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) getMCPServer(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp disabled"})
		return
	}
	rec, err := s.mcpServerByIDForOrg(r.Context(), orgID(r), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, mcp.ErrServerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) patchMCPServer(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.mcpServerByIDForOrg(r.Context(), orgID(r), id)
	if err != nil {
		if errors.Is(err, mcp.ErrServerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	var body mcpServerBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(body.Name) != "" {
		rec.Name = body.Name
	}
	if strings.TrimSpace(body.SpecYAML) != "" {
		if _, err := mcp.DecodeSpec([]byte(body.SpecYAML)); err != nil {
			s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
			return
		}
		rec.SpecYAML = body.SpecYAML
	}
	rec.Enabled = body.Enabled
	if err := s.mcpStore.Save(r.Context(), rec); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	if s.mcpRuntime != nil {
		_ = s.mcpRuntime.Upsert(r.Context(), *rec)
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("mcp.server.update"), rec.ID, rec.Name)
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) deleteMCPServer(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.mcpServerByIDForOrg(r.Context(), orgID(r), id)
	if err != nil {
		if errors.Is(err, mcp.ErrServerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	if err := s.mcpStore.Delete(r.Context(), id); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	if s.mcpRuntime != nil {
		s.mcpRuntime.Remove(id)
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("mcp.server.delete"), rec.ID, rec.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reconnectMCPServer(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpRuntime == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp runtime disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := s.mcpServerByIDForOrg(r.Context(), orgID(r), id); err != nil {
		if errors.Is(err, mcp.ErrServerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	if err := s.mcpRuntime.Reconnect(r.Context(), id); err != nil {
		s.fail(w, r, http.StatusBadGateway, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("mcp.server.reconnect"), id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) testMCPServer(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpRuntime == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp runtime disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := s.mcpServerByIDForOrg(r.Context(), orgID(r), id); err != nil {
		if errors.Is(err, mcp.ErrServerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	start := time.Now()
	count, err := s.mcpRuntime.TestConnection(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, Result{OK: false, Message: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("mcp.server.test"), id, "")
	writeJSON(w, http.StatusOK, Result{
		OK:        true,
		Message:   fmt.Sprintf("tools/list ok (%d tools)", count),
		LatencyMs: time.Since(start).Milliseconds(),
	})
}

func (s *Server) mcpServerByIDForOrg(ctx context.Context, org, id string) (*mcp.ServerRecord, error) {
	rec, err := s.mcpStore.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.OrgID != "" && normalizeMCPOrg(rec.OrgID) != normalizeMCPOrg(org) {
		return nil, mcp.ErrServerNotFound
	}
	return rec, nil
}

func (s *Server) mcpRuntimeStatuses(org string) []mcp.ServerStatus {
	if s.mcpRuntime == nil {
		return nil
	}
	return s.mcpRuntime.ListAllServers(normalizeMCPOrg(org))
}

func normalizeMCPOrg(org string) string {
	if strings.TrimSpace(org) == "" || org == core.DefaultOrgID {
		return ""
	}
	return org
}
