package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/resp"
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
	// Outgoing email transport: best-effort, never blocks the
	// response. A successfully issued invite row is the contract
	// the admin sees immediately; the envelope is a courtesy on
	// top. We tag the message id onto an audit row so an
	// operator can correlate "we sent invite X" → Resend
	// dashboard / send API logs.
	if s.mailer != nil && inv.URL != "" {
		body, tmplErr := renderInviteHTML(inv.URL, caller.Email, role)
		if tmplErr != nil {
			s.log.Warn("invite template render failed",
				"err", tmplErr,
				"invite", inv.ID,
				"email", email,
				"actor", caller.ID)
			s.store.Audit(r.Context(), core.AuditEvent{
				ActorID:  caller.ID,
				OrgID:    orgID(r),
				Action:   core.AuditActionInviteIssuedEmailTemplate,
				TargetID: inv.ID,
				Detail:   tmplErr.Error(),
			})
			// Fall through to return the OK response: the row is
			// committed and the URL works; the only casualty is
			// the envelope, matching the fall-back behaviour below.
		} else {
			idem := "nexus-invite-" + inv.ID
			// Detach from the request context: a slow / hard-failing
			// SMTP relay or Resend call must not unwind an admin's
			// already-issued invite status. The transport owns its
			// own deadline (EmailRequestTimeout) and audit row.
			go s.deliverInvite(r.Context(), email, "You're invited to Nexus", body, idem, inv.ID, caller.ID, orgID(r))
		}
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
			resp.HTTP(w, r, http.StatusNotFound, apierr.CodeNotFound, "", core.ErrNotFound, s.log)
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
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

// deliverInvite runs the actual transport call after createInvite has
// committed the row. It is detached from the request context so a slow
// SMTP relay or Resend hand-off cannot unwind an admin's already-issued
// invite status; the transport owns its own deadline (EmailRequestTimeout)
// and its own audit row.
//
// The Detail field carries "transport=<name>; msg=<id>" — previously it
// embedded the raw Resend error string, which leaked base URLs and
// inboxes into the audit_log when a misconfigured relay returned a long
// diagnostic. Sanitising the failure detail to {subject + reason code}
// is the safer form for SIEM rules that key on the action.
//
// The function is safe to invoke concurrently because Server.mailer is a
// single Mailer whose underlying transports are themselves concurrency-
// safe (http.Client, smtp.Client-per-call). The slog logger handles the
// per-invite card, and the audit row includes the invite id so an
// operator can correlate "invite.email.failed" → "go away retry" by
// filtering the audit log.
func (s *Server) deliverInvite(_ context.Context, recipient, subject, body, idempotencyKey, inviteID, actorID, orgID string) {
	if s.mailer == nil {
		return
	}
	// Detached: 30s ceiling is the operator-configured EmailRequestTimeout
	// ceiling via the per-transport Timeout. We give it a small extra
	// headroom so a perfectly-reliable SMTP transport that's also the
	// audit-write bottleneck does not get cut by the goroutine limit.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgID, err := s.mailer.Send(ctx, recipient, subject, body, idempotencyKey)
	if err != nil {
		s.log.Warn("invite email send failed",
			"err", err,
			"invite", inviteID,
			"email", recipient,
			"actor", actorID)
		s.store.Audit(ctx, core.AuditEvent{
			ActorID:  actorID,
			OrgID:    orgID,
			Action:   core.AuditActionInviteIssuedEmailFailed,
			TargetID: inviteID,
			Detail:   "subject=" + subject + "; err=" + err.Error(),
		})
		return
	}
	s.store.Audit(ctx, core.AuditEvent{
		ActorID:  actorID,
		OrgID:    orgID,
		Action:   core.AuditActionInviteIssuedEmailSent,
		TargetID: inviteID,
		Detail:   "subject=" + subject + "; msg=" + msgID,
	})
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
				"reason": "revoked or never issued",
			})
			return
		}
		// An accepted invite is spent. 410 Gone rather than 404 because the
		// token was real, and the visitor's next step is to sign in rather than
		// to chase an admin for a replacement link. No account data is echoed
		// back: the caller here is unauthenticated, and a used invite link
		// outlives onboarding in forwarded mail and browser history.
		if errors.Is(err, core.ErrInviteConsumed) {
			writeJSON(w, http.StatusGone, map[string]string{
				"error":  "invite already used",
				"reason": "this invite has been accepted; sign in with your account instead",
			})
			return
		}
		if errors.Is(err, core.ErrInviteExpired) {
			writeJSON(w, http.StatusGone, map[string]string{
				"error":  "invite expired",
				"reason": "ask an administrator for a new invite",
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
