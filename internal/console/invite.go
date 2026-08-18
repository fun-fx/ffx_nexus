package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
)

// --- Invite flow ----------------------------------------------------
//
// The previous "Invite user" drawer was an admin-managed user-create
// with a password the admin typed then handed to the invitee by hand.
// That worked but the controls did not match the documented "admin
// invites → invitee accepts" flow (or the user-facing notion of
// "invite": a sharable, time-bound artefact that the invitee can
// accept on their own).
//
// This split introduces four endpoints:
//
//   POST   /api/invites            (admin)   issue an invite → returns
//                                               the raw token + URL once.
//   GET    /api/invites            (admin)   list pending + historical
//                                               invites for the org.
//   DELETE /api/invites/{id}       (admin)   revoke a pending invite.
//   GET    /api/invite/{token}     (public)  lookup renders the accept
//                                               page with the invitee
//                                               facing summary.
//   POST   /api/invite/{token}/accept (public) commit the password and
//                                                create the user row.
//
// The accept endpoints are anonymous — the sha256 token hash IS the
// authorisation. They are still rate-limited per-IP so a token guess
// cannot be exercised at high parallelism.

// inviteRequestShape is the input to POST /api/invites. Email is
// validated server-side so a malformed address never reaches the DB
// row even if the console skipped its own check.
type inviteRequestShape struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, caller core.User) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control plane disabled: set NEXUS_POSTGRES_URL to enable invites",
		})
		return
	}
	var req inviteRequestShape
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	email := strings.TrimSpace(req.Email)
	if _, err := mail.ParseAddress(email); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is not a valid address"})
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = core.RoleMember
	}
	if role != core.RoleAdmin && role != core.RoleMember {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be admin or member"})
		return
	}
	inv, err := s.store.CreateInvite(r.Context(), orgID(r), caller.ID, email, role, 0, s.publicBaseURL)
	if err != nil {
		if errors.Is(err, core.ErrEmailTaken) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "an invite for this email already exists in this org",
			})
			return
		}
		s.log.Error("create invite failed", "err", err, "actor", caller.ID, "email", email)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (s *Server) listInvites(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control plane disabled: set NEXUS_POSTGRES_URL to enable invites",
		})
		return
	}
	out, err := s.store.ListInvites(r.Context(), orgID(r))
	if err != nil {
		s.log.Error("list invites failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list failed"})
		return
	}
	if out == nil {
		out = []core.Invite{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request, caller core.User) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control plane disabled: set NEXUS_POSTGRES_URL to enable invites",
		})
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing invite id"})
		return
	}
	if err := s.store.RevokeInvite(r.Context(), orgID(r), caller.ID, id); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "invite not found"})
			return
		}
		s.log.Error("revoke invite failed", "err", err, "invite", id)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// invitePublicView is the shape returned by GET /api/invite/{token}.
// Only fields an invitee should see before they accept: email (so
// they confirm the right account), role (so they know what they're
// getting into), and an expiry statement.
type invitePublicView struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	OrgID     string `json:"org_id"`
	ExpiresAt string `json:"expires_at"`
	Valid     bool   `json:"valid"`
}

func (s *Server) lookupInvite(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control plane disabled: set NEXUS_POSTGRES_URL to enable invites",
		})
		return
	}
	raw := strings.TrimSpace(chi.URLParam(r, "token"))
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing invite token"})
		return
	}
	inv, err := s.store.LookupInvite(r.Context(), raw)
	if err != nil {
		if errors.Is(err, core.ErrInviteNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":  "invite not found",
				"reason": "revoked, expired, or never issued",
			})
			return
		}
		s.log.Error("lookup invite failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}
	writeJSON(w, http.StatusOK, invitePublicView{
		Email:     inv.Email,
		Role:      inv.Role,
		OrgID:     inv.OrgID,
		ExpiresAt: inv.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Valid:     inv.RevokedAt == nil && inv.AcceptedAt == nil,
	})
}

// inviteAcceptShape is the input to POST /api/invite/{token}/accept.
// One new password; the server stamps the user-create + invite-accept
// audit rows in the same transaction so the on-call cannot find an
// emitted-but-broken unwind.
type inviteAcceptShape struct {
	Password string `json:"password"`
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control plane disabled: set NEXUS_POSTGRES_URL to enable invites",
		})
		return
	}
	raw := strings.TrimSpace(chi.URLParam(r, "token"))
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing invite token"})
		return
	}
	var req inviteAcceptShape
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	pw := strings.TrimSpace(req.Password)
	if pw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password is required"})
		return
	}
	if len(pw) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 8 characters"})
		return
	}
	u, _, err := s.store.AcceptInvite(r.Context(), raw, pw)
	if err != nil {
		if errors.Is(err, core.ErrInviteNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":  "invite not found",
				"reason": "revoked, expired, or never issued",
			})
			return
		}
		if errors.Is(err, core.ErrEmailTaken) {
			// Edge case: an admin manually created the matching user
			// while the invite was pending. Surface as a hard fail so
			// the invitee can ask the admin to either revoke one
			// path consciously — never silently overwrite a real
			// password hash.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "an account with this email already exists",
			})
			return
		}
		s.log.Error("accept invite failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "accept failed"})
		return
	}
	writeJSON(w, http.StatusOK, u)
}
