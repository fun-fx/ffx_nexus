package console

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/observability"
)

// Spend handlers expose per-day LLM cost aggregations over gateway_traces.
//
// Two scopes, four endpoints:
//
//   GET /api/me/spend/daily?days=30
//   GET /api/me/spend/daily/{day}/breakdown
//     — requireUser: scoped to u.ID
//
//   GET /api/users/{id}/spend/daily?days=30
//   GET /api/users/{id}/spend/daily/{day}/breakdown
//     — requireAdmin: {id} resolves to a same-org user; admins can also
//       pass {id}=me to look at their own through this route.

// spendDailyDays parses ?days=N. Clamped to [1, 365]. Default 30.
func spendDailyDays(r *http.Request) int {
	q := r.URL.Query().Get("days")
	if q == "" {
		return 30
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 {
		return 30
	}
	if n > 365 {
		return 365
	}
	return n
}

// parseSpendDay parses the {day} URL parameter as a YYYY-MM-DD calendar
// day in UTC. Returns (time, true) on success; (zero, false) on error;
// callers convert the false case to HTTP 422.
func parseSpendDay(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (s *Server) mySpendDaily(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.DailySpendRow{})
		return
	}
	days := spendDailyDays(r)
	until := time.Now().UTC()
	since := until.AddDate(0, 0, -days)
	rows, err := s.reader.DailySpendByDay(r.Context(), since, until, orgID(r), u.ID)
	if err != nil {
		s.log.Error("my spend daily query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) mySpendBreakdown(w http.ResponseWriter, r *http.Request, u core.User) {
	day, ok := parseSpendDay(chi.URLParam(r, "day"))
	if !ok {
		http.Error(w, "day must be YYYY-MM-DD", http.StatusUnprocessableEntity)
		return
	}
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.DailySpendBreakdownRow{})
		return
	}
	rows, err := s.reader.DailySpendBreakdown(r.Context(), day, orgID(r), u.ID)
	if err != nil {
		s.log.Error("my spend breakdown query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// adminSpendUserID resolves the {id} URL parameter. The literal "me"
// transparently substitutes the admin's own id so the admin can inspect
// their spend using the same URL shape as a per-member lookup.
func adminSpendUserID(r *http.Request, caller core.User) string {
	id := chi.URLParam(r, "id")
	if id == "me" {
		return caller.ID
	}
	return id
}

func (s *Server) userSpendDaily(w http.ResponseWriter, r *http.Request, caller core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.DailySpendRow{})
		return
	}
	days := spendDailyDays(r)
	until := time.Now().UTC()
	since := until.AddDate(0, 0, -days)
	rows, err := s.reader.DailySpendByDay(r.Context(), since, until, orgID(r), adminSpendUserID(r, caller))
	if err != nil {
		s.log.Error("user spend daily query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) userSpendBreakdown(w http.ResponseWriter, r *http.Request, caller core.User) {
	day, ok := parseSpendDay(chi.URLParam(r, "day"))
	if !ok {
		http.Error(w, "day must be YYYY-MM-DD", http.StatusUnprocessableEntity)
		return
	}
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.DailySpendBreakdownRow{})
		return
	}
	rows, err := s.reader.DailySpendBreakdown(r.Context(), day, orgID(r), adminSpendUserID(r, caller))
	if err != nil {
		s.log.Error("user spend breakdown query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
