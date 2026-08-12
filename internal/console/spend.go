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
// Three endpoints per scope (self + admin):
//
//   GET /api/me/spend/summary?days=30
//   GET /api/me/spend/daily?days=30
//   GET /api/me/spend/daily/{day}/breakdown
//     — requireUser: scoped to u.ID
//
//   GET /api/users/{id}/spend/summary?days=30
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

// /api/me/spend/summary?days=N — current window + previous-window totals
// for the Spend-page hero card. The two window rollups ride one
// ClickHouse query (cur + prev CTEs), so the page's hero loads in lock
// step with the daily list rather than issuing two round-trips.
func (s *Server) mySpendSummary(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, emptySummary(spendDailyDays(r)))
		return
	}
	days := spendDailyDays(r)
	out, err := s.reader.DailySpendSummary(r.Context(), days, orgID(r), u.ID)
	if err != nil {
		s.log.Error("my spend summary query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// /api/users/{id}/spend/summary?days=N — admin-scoped variant. Same
// shape as /api/me/spend/summary but the reportable slice is the
// resolved {id}'s gateway_traces, intercepted at adminSpendUserID so
// `id=me` reads the admin's own rollup.
func (s *Server) userSpendSummary(w http.ResponseWriter, r *http.Request, caller core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, emptySummary(spendDailyDays(r)))
		return
	}
	days := spendDailyDays(r)
	out, err := s.reader.DailySpendSummary(r.Context(), days, orgID(r), adminSpendUserID(r, caller))
	if err != nil {
		s.log.Error("user spend summary query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// emptySummary returns the zero-value spend summary so the page can
// still render its hero when the reader is unconfigured (e.g. the dev
// build without a ClickHouse sink). `days` is preserved so the
// readout still reads "Last N days" rather than vanishing entirely.
func emptySummary(days int) observability.DailySpendSummary {
	return observability.DailySpendSummary{Days: days}
}
