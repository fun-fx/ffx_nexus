package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/egress"
	"github.com/ffxnexus/nexus/internal/evals"
)

// EvalProfileSource is the dependency the console needs to read & write
// EvalProfile rows. The runtime controller in cmd/nexus wires a
// MemoryStore / Postgres / ClickHouse-backed implementation here. nil
// means the feature is disabled (old build, single-tenant unit test).
type EvalProfileSource interface {
	ListEvalProfiles(ctx context.Context, orgID, ownerUserID string) ([]evals.EvalProfile, error)
	GetEvalProfile(ctx context.Context, id string) (*evals.EvalProfile, error)
	SaveEvalProfile(ctx context.Context, p *evals.EvalProfile) error
	DeleteEvalProfile(ctx context.Context, id string) error
}

// profileForOrg fetches a profile and refuses it if it belongs to another org.
//
// Mirrors pluginByIDForOrg: cluster-wide rows (OrgID == "") are the operator's
// seeded configuration and remain reachable, a row owned by a different org
// reports ErrProfileNotFound rather than a 403. "Not found" is deliberate — a
// 403 saying "that is not your profile" confirms the id exists, which lets a
// caller enumerate another tenant's profile ids one guess at a time.
func (s *Server) profileForOrg(ctx context.Context, orgID, id string) (*evals.EvalProfile, error) {
	p, err := s.evalProfiles.GetEvalProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, evals.ErrProfileNotFound
	}
	if !p.VisibleToOrg(orgID) {
		return nil, evals.ErrProfileNotFound
	}
	return p, nil
}

// listEvalProfiles returns the profiles the caller can see. Mirrors
// PR #133's callerCanSee() semantics: org profiles visible to every
// member, user profiles to their owner, admins see everything — all of it
// within the caller's own tenant, which the store filter enforces first.
func (s *Server) listEvalProfiles(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalProfiles == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval profiles disabled"})
		return
	}
	all, err := s.evalProfiles.ListEvalProfiles(r.Context(), orgID(r), u.ID)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	out := make([]evals.EvalProfile, 0, len(all))
	for _, p := range all {
		// Belt and braces: the store already filtered by org, but the "admin
		// sees everything" widening below is exactly the kind of rule that
		// grows past a tenant boundary, so the boundary is re-checked here
		// where the widening happens.
		if !p.VisibleToOrg(orgID(r)) {
			continue
		}
		if profileCallerCanSee(p, u) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// checkProfileEndpoint rejects a judge or remote-eval endpoint the guard would
// refuse to dial.
//
// The guard already stops the request at connect time, so this is not the
// security boundary — it is the difference between "saved, and then silently
// never scores anything" and an error the admin sees while they still have the
// URL in front of them. Without it the failure surfaces hours later as missing
// scores, which is indistinguishable from a dozen other causes.
//
// Builtin heuristics carry no reachable endpoint, so they are skipped rather
// than made to satisfy a URL rule that does not apply to them.
func checkProfileEndpoint(ctx context.Context, p evals.EvalProfile) error {
	base := strings.TrimSpace(p.Endpoint.BaseURL)
	if base == "" || p.Endpoint.KeySource == evals.KeySourceBuiltin {
		return nil
	}
	// CheckConfiguredURL, not CheckURL: a host that does not resolve from this pod
	// right now must not block the save. Private DNS zones and names the customer
	// has not finished creating both resolve later, and the address policy is
	// enforced on every request by the dialer either way.
	if err := egress.CheckConfiguredURL(ctx, base, egress.Tenant); err != nil {
		return fmt.Errorf("endpoint.base_url is not an allowed destination: %w", err)
	}
	return nil
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
	// The org comes from the session, overwriting anything the body asked for.
	//
	// Two attacks close here. A body carrying another tenant's org id would
	// otherwise plant a profile inside that tenant — and because profiles hold
	// an endpoint and a key reference, that profile would then be applied to
	// the victim's traces and ship their prompts to an endpoint of the
	// attacker's choosing. A body carrying org_id:"" would create a
	// cluster-wide row and do the same to every tenant at once. Cluster-wide
	// profiles are reachable only through operator env/Helm seeding; no HTTP
	// request can mint one.
	p.OrgID = orgID(r)
	// An id supplied by the client is ignored on create. Save() treats a
	// populated ID as an update, so accepting it would let a POST overwrite an
	// existing profile — including one this caller was never allowed to read.
	p.ID = ""
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
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	if err := checkProfileEndpoint(r.Context(), p); err != nil {
		s.failWithMessage(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest,
			"endpoint.base_url is not an allowed destination", err)
		return
	}
	if err := s.evalProfiles.SaveEvalProfile(r.Context(), &p); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.profile.create"), p.ID, p.Name)
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
	existing, err := s.profileForOrg(r.Context(), orgID(r), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": evals.ErrProfileNotFound.Error()})
		return
	}
	if !profileCallerCanWrite(*existing, u) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not your eval profile"})
		return
	}
	owner := existing.OrgID
	var patch evals.ProfilePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := applyProfilePatch(existing, &patch); err != nil {
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	// A patch cannot move a profile between tenants, in either direction: it
	// can neither hand a row to another org nor claim a cluster-wide row for
	// the caller's own (which would strip the operator's seeded PII profile
	// from every other tenant). ProfilePatch has no org field today, so this
	// restore is a guard against one being added later without anyone
	// revisiting the boundary.
	existing.OrgID = owner
	if err := existing.Validate(); err != nil {
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	if err := checkProfileEndpoint(r.Context(), *existing); err != nil {
		s.failWithMessage(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest,
			"endpoint.base_url is not an allowed destination", err)
		return
	}
	if err := s.evalProfiles.SaveEvalProfile(r.Context(), existing); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.profile.update"), existing.ID, existing.Name)
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
	existing, err := s.profileForOrg(r.Context(), orgID(r), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": evals.ErrProfileNotFound.Error()})
		return
	}
	if !profileCallerCanWrite(*existing, u) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not your eval profile"})
		return
	}
	if err := s.evalProfiles.DeleteEvalProfile(r.Context(), id); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.profile.delete"), id, existing.Name)
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
