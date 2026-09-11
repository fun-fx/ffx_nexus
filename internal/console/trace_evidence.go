package console

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/observability"
)

func (s *Server) getTraceEvidence(w http.ResponseWriter, r *http.Request, u core.User) {
	ev := s.loadTraceEvidence(w, r, u)
	if ev == nil {
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) exportTraceEvidence(w http.ResponseWriter, r *http.Request, u core.User) {
	ev := s.loadTraceEvidence(w, r, u)
	if ev == nil {
		return
	}
	id := chi.URLParam(r, "id")
	w.Header().Set("Content-Disposition", `attachment; filename="nexus-trace-`+id+`.json"`)
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) loadTraceEvidence(w http.ResponseWriter, r *http.Request, u core.User) *observability.TraceEvidence {
	if s.reader == nil {
		http.Error(w, "trace store not configured", http.StatusNotFound)
		return nil
	}
	id := chi.URLParam(r, "id")
	uid := ""
	if u.Role != core.RoleAdmin {
		uid = u.ID
	}
	ev, err := s.reader.TraceEvidence(r.Context(), orgID(r), uid, id)
	if err != nil {
		s.log.Error("trace evidence query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return nil
	}
	if ev == nil {
		http.Error(w, "trace not found", http.StatusNotFound)
		return nil
	}
	if u.Role == core.RoleAdmin {
		sum := []observability.TraceSummary{ev.TraceSummary}
		s.enrichTraceUserEmails(r.Context(), orgID(r), sum)
		ev.UserEmail = sum[0].UserEmail
	}
	return ev
}
