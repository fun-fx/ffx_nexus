package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evals"
)

// EvalProfileSource is the dependency the console needs to read & write
// EvalProfile rows. The runtime controller in cmd/nexus wires a
// MemoryStore / Postgres / ClickHouse-backed implementation here. nil
// means the feature is disabled (old build, single-tenant unit test).
type EvalProfileSource interface {
	ListEvalProfiles(ctx context.Context, ownerUserID string) ([]evals.EvalProfile, error)
	GetEvalProfile(ctx context.Context, id string) (*evals.EvalProfile, error)
	SaveEvalProfile(ctx context.Context, p *evals.EvalProfile) error
	DeleteEvalProfile(ctx context.Context, id string) error
}

// listEvalProfiles returns the profiles the caller can see. Mirrors
// PR #133's callerCanSee() semantics: org profiles visible to every
// member, user profiles to their owner, admins see everything.
func (s *Server) listEvalProfiles(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval profiles disabled"})
		return
	}
	all, err := s.evalProfiles.ListEvalProfiles(r.Context(), u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]evals.EvalProfile, 0, len(all))
	for _, p := range all {
		if profileCallerCanSee(p, u) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// createEvalProfile persists a new profile. Admin-only — the route
// already required admin, so we just validate and Save.
func (s *Server) createEvalProfile(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval profiles disabled"})
		return
	}
	var p evals.EvalProfile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	// User-scoped pathways: when a non-admin tries to create a user
	// profile, force the owner_user_id to themselves so two users
	// cannot impersonate one another. Admin can create user profiles
	// on behalf of any user (audit trail catches the action).
	if p.Scope == evals.ScopeUser && p.OwnerUserID == "" {
		if u.Role != "admin" {
			p.OwnerUserID = u.ID
		}
	}
	if err := p.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.evalProfiles.SaveEvalProfile(r.Context(), &p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.profile.create", p.ID, p.Name)
	writeJSON(w, http.StatusCreated, p)
}

// patchEvalProfile applies a partial update. Caller must own the row
// (or be admin).
func (s *Server) patchEvalProfile(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval profiles disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := s.evalProfiles.GetEvalProfile(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if !profileCallerCanWrite(*existing, u) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not your eval profile"})
		return
	}
	var patch evals.ProfilePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := applyProfilePatch(existing, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := existing.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.evalProfiles.SaveEvalProfile(r.Context(), existing); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.profile.update", existing.ID, existing.Name)
	writeJSON(w, http.StatusOK, existing)
}

// deleteEvalProfile is reservation-safe: any caller with delete rights
// can remove the row. Audit log records who fired off the action.
func (s *Server) deleteEvalProfile(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval profiles disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := s.evalProfiles.GetEvalProfile(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if !profileCallerCanWrite(*existing, u) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not your eval profile"})
		return
	}
	if err := s.evalProfiles.DeleteEvalProfile(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.profile.delete", id, existing.Name)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// applyProfilePatch merges the optional fields into the existing row.
// Only fields that are present in the patch are updated, so partial
// PATCHes stay minimal and audit logs stay readable.
func applyProfilePatch(p *evals.EvalProfile, patch *evals.ProfilePatch) error {
	if patch.Name != nil {
		p.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Kind != nil {
		p.Kind = *patch.Kind
	}
	if patch.Scope != nil {
		p.Scope = *patch.Scope
	}
	if patch.OwnerUser != nil {
		p.OwnerUserID = *patch.OwnerUser
	}
	if patch.Endpoint != nil {
		p.Endpoint = *patch.Endpoint
	}
	if patch.Metrics != nil {
		p.Metrics = append([]string(nil), (*patch.Metrics)...)
	}
	if patch.Threshold != nil {
		p.Threshold = *patch.Threshold
	}
	if patch.SampleRate != nil {
		p.SampleRate = *patch.SampleRate
	}
	if patch.Enabled != nil {
		p.Enabled = *patch.Enabled
	}
	return nil
}

// profileCallerCanSee matches the same logic PR #133 uses for router
// providers: org profiles visible to everyone, user profiles only to
// their owner, admins always visible.
func profileCallerCanSee(p evals.EvalProfile, caller core.User) bool {
	switch p.Scope {
	case evals.ScopeOrg, "":
		return true
	case evals.ScopeUser:
		if caller.Role == "admin" {
			return true
		}
		return p.OwnerUserID != "" && p.OwnerUserID == caller.ID
	default:
		return true
	}
}

// profileCallerCanWrite narrows visibility for writers: even a public
// org profile requires admin to edit (we don't want members editing
// shared infrastructure), user profiles are editable by their owner.
func profileCallerCanWrite(p evals.EvalProfile, caller core.User) bool {
	if caller.Role == "admin" {
		return true
	}
	if p.Scope == evals.ScopeUser && p.OwnerUserID == caller.ID {
		return true
	}
	return false
}
