package console

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/observability"
)

func (s *Server) listMCPLogs(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp logs require clickhouse"})
		return
	}
	q := parseMCPLogQuery(r)
	userScope := ""
	if u.Role != core.RoleAdmin {
		userScope = u.ID
	}
	page, err := s.reader.MCPLogPage(r.Context(), orgID(r), userScope, q.Limit, q.Before, q.Since, q.Filter)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) getMCPLog(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp logs require clickhouse"})
		return
	}
	detail, err := s.reader.MCPLogByID(r.Context(), orgID(r), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	if detail == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if u.Role != core.RoleAdmin && detail.UserID != "" && detail.UserID != u.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) mcpLogFilterData(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp logs require clickhouse"})
		return
	}
	data, err := s.reader.MCPLogFilterData(r.Context(), orgID(r))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) mcpLogStats(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "mcp logs require clickhouse"})
		return
	}
	q := parseMCPLogQuery(r)
	userScope := ""
	if u.Role != core.RoleAdmin {
		userScope = u.ID
	}
	stats, err := s.reader.MCPLogStatsWindow(r.Context(), orgID(r), userScope, q.Since, q.Before)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) liveMCP(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.mcpHub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "live mcp feed disabled"})
		return
	}
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	userScope := ""
	if u.Role != core.RoleAdmin {
		userScope = u.ID
	}
	ch := s.mcpHub.subscribe(userScope)
	defer s.mcpHub.unsubscribe(ch)
	done := make(chan struct{})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(done)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case log, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(map[string]any{"type": "mcp_log", "log": log})
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}
}

type mcpLogQuery struct {
	Limit  int
	Since  time.Time
	Before time.Time
	Filter observability.MCPLogFilter
}

func parseMCPLogQuery(r *http.Request) mcpLogQuery {
	q := mcpLogQuery{Limit: 100, Before: time.Now()}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			q.Since = t
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			q.Before = t
		}
	}
	q.Filter = observability.MCPLogFilter{
		ToolName:     r.URL.Query().Get("tool_name"),
		ServerLabel:  r.URL.Query().Get("server_label"),
		Status:       r.URL.Query().Get("status"),
		VirtualKeyID: r.URL.Query().Get("virtual_key_id"),
		LLMTraceID:   r.URL.Query().Get("llm_trace_id"),
		Q:            r.URL.Query().Get("q"),
	}
	return q
}
