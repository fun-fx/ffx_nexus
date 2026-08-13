package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/core"
)

// ScheduleHandlers wire the admin REST endpoints under
// /api/eval/benchmarks/schedules to the core/cron stack.
//
// Two design choices worth flagging:
//
//  1. Schedule bodies reuse the same shape LaunchRequest takes minus
//     the per-launch derived fields. Diverge would have meant two
//     unrelated form surfaces on the console and a chance for them
//     to drift; reuse keeps validation in one place (Launch.Validate)
//     while letting cron decorate the run id and the linkage column.
//
//  2. We deliberately do not implement PATCH. Edit cadence and the
//     run shape by deleting and re-creating; the row count is small
//     and an in-place edit can fire between the operator saving and
//     the runner reading the stale row. A delete-then-create round
//     is observable in the audit log, which is the property we want.

// scheduleRequest is the JSON shape accepted on POST.
type scheduleRequest struct {
	Name           string   `json:"name"`
	Environments   []string `json:"environments"`
	Model          string   `json:"model"`
	NumExamples    int      `json:"num_examples"`
	Rollouts       int      `json:"rollouts"`
	ViaGateway     *bool    `json:"via_gateway"`
	CadenceSeconds int      `json:"cadence_seconds"`
	Enabled        *bool    `json:"enabled"`
}

const (
	scheduleMinCadence = 60            // 1 minute — synchronous ticks run every 30s
	scheduleMaxCadence = 7 * 24 * 3600 // 7 days — anything longer is more usefully manual
)

func (s *Server) createBenchmarkSchedule(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "benchmarks not configured"})
		return
	}
	var body scheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(body.Model) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if len(body.Environments) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one environment is required"})
		return
	}
	if body.CadenceSeconds < scheduleMinCadence || body.CadenceSeconds > scheduleMaxCadence {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "cadence_seconds must be between 60 (1 minute) and 604800 (7 days)",
		})
		return
	}
	viaGateway := true
	if body.ViaGateway != nil {
		viaGateway = *body.ViaGateway
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	row, err := s.benchmarks.CreateSchedule(r.Context(), core.BenchmarkSchedule{
		ID:             uuid.NewString(),
		OrgID:          u.OrgID,
		Name:           strings.TrimSpace(body.Name),
		Environments:   body.Environments,
		Model:          body.Model,
		NumExamples:    body.NumExamples,
		Rollouts:       body.Rollouts,
		ViaGateway:     viaGateway,
		CadenceSeconds: body.CadenceSeconds,
		Enabled:        enabled,
		CreatedBy:      u.Email,
		NextLaunchAt:   time.Now().UTC().Add(time.Duration(body.CadenceSeconds) * time.Second),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) listBenchmarkSchedules(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "benchmarks not configured"})
		return
	}
	rows, err := s.benchmarks.ListSchedules(r.Context(), u.OrgID, 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": rows})
}

func (s *Server) getBenchmarkSchedule(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "benchmarks not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	row, err := s.benchmarks.GetSchedule(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "schedule not found"})
		return
	}
	if u.OrgID != "" && row.OrgID != "" && u.OrgID != row.OrgID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "schedule belongs to a different org"})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) deleteBenchmarkSchedule(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "benchmarks not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	row, err := s.benchmarks.GetSchedule(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "schedule not found"})
		return
	}
	if u.OrgID != "" && row.OrgID != "" && u.OrgID != row.OrgID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "schedule belongs to a different org"})
		return
	}
	if err := s.benchmarks.DeleteSchedule(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// schedulePauseResume is the explicit pause/resume action: a POST with
// {"enabled": false} pauses the row, a POST with {"enabled": true}
// resumes it.
//
// Cadence and the run shape cannot be edited in place; this endpoint
// only flips the on/off bit. Resume re-stamps NextLaunchAt to "now +
// cadence" so a long-paused row does not trap the runner at the front
// of its scan queue the moment the toggle goes back to true.
type scheduleEnableBody struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) pauseBenchmarkSchedule(w http.ResponseWriter, r *http.Request, u core.User) {
	s.setBenchmarkScheduleEnabled(w, r, u, false)
}

func (s *Server) resumeBenchmarkSchedule(w http.ResponseWriter, r *http.Request, u core.User) {
	s.setBenchmarkScheduleEnabled(w, r, u, true)
}

func (s *Server) setBenchmarkScheduleEnabled(
	w http.ResponseWriter, r *http.Request, u core.User, enabled bool,
) {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "benchmarks not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	row, err := s.benchmarks.GetSchedule(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "schedule not found"})
		return
	}
	if u.OrgID != "" && row.OrgID != "" && u.OrgID != row.OrgID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "schedule belongs to a different org"})
		return
	}
	// Body is accepted but optional — an empty POST with the URL alone
	// is enough to act. The body's "enabled" field, when present, must
	// agree with the route; otherwise we 400 the call.
	var body scheduleEnableBody
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if body.Enabled != enabled {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "body.enabled must match the route",
			})
			return
		}
	}
	// Resume re-stamps to "now + cadence" so the row pauses its wait
	// from the moment of resume rather than firing the date that was
	// already due. Pause keeps the existing NextLaunchAt so the row
	// can be resumed "as if no time had passed" by re-using it.
	next := time.Time{}
	if enabled && row.CadenceSeconds > 0 {
		next = time.Now().UTC().Add(time.Duration(row.CadenceSeconds) * time.Second)
	}
	if err := s.benchmarks.SetScheduleEnabled(r.Context(), id, enabled, next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	row, err = s.benchmarks.GetSchedule(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}
