package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// bcryptGenerate is a thin layer over bcrypt.GenerateFromPassword at the
// default cost so the invite accept path shares the same hash format as
// CreateUser — pre-flight comparisons therefore work across both flows.
func bcryptGenerate(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// buildInviteURL composes a shareable URL out of publicBaseURL + the raw
// token. Accepts both `/` and empty as "no path" so callers can hand in
// either `https://nexus.ffx.ai` or `https://nexus.ffx.ai/`.
func buildInviteURL(publicBaseURL, raw string) string {
	base := strings.TrimRight(publicBaseURL, "/")
	if base == "" {
		base = "/"
	}
	return base + "/invite/" + raw
}

// Invite is the public-facing shape of a pending or historical invite. The
// raw token is NEVER persisted — only the sha256 hash lives in the DB; the
// raw token is returned exactly once, at create time, so the admin can hand
// it off to the invitee out of band.
type Invite struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"org_id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"` // "admin" | "member"
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	AcceptedBy *string    `json:"accepted_by,omitempty"`
	URL        string     `json:"url,omitempty"` // only populated on create response
}

// InviteIssued is the shape returned when an admin issues one — it carries
// the raw token (and the shareable URL) so the admin can copy/paste it into
// a Slack DM, ticket comment, or ticket system. After the create call
// returns, the raw token cannot be shown again; subsequent list/get calls
// strip it.
type InviteIssued struct {
	Invite
	Token string `json:"token"`
}

// ErrInviteNotFound is returned when the public accept path can't locate
// the record (revoked, expired, never issued, consumed twice).
var ErrInviteNotFound = errors.New("core: invite not found or no longer valid")

// DefaultInviteTTL is how long an issued invite stays open. Long enough to
// survive an out-of-band handoff that takes a few business days; short
// enough that abandoned invites don't linger.
const DefaultInviteTTL = 7 * 24 * time.Hour

// generateRawToken returns 32 hex bytes (256 bits) of CSPRNG randomness.
// Suffix-loaded keys would beep on entropy farms so we lean on crypto/rand.
func generateRawToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// hashInviteToken returns the canonical sha256 hex of the raw invite
// token so a leaked DB dump cannot impersonate an invitee.
func hashInviteToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateInvite issues a new invite for email+role within orgID. The
// returned InviteIssued includes the raw token string — call sites MUST
// surface it (URL or copy buffer) immediately because it's unrecoverable
// after the function returns.
func (s *Store) CreateInvite(ctx context.Context, orgID, actorID, email, role string, ttl time.Duration, publicBaseURL string) (InviteIssued, error) {
	if orgID == "" {
		orgID = "default"
	}
	if role == "" {
		role = RoleMember
	}
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	email = normaliseEmail(email)
	if email == "" {
		return InviteIssued{}, fmt.Errorf("email is required")
	}
	raw, err := generateRawToken()
	if err != nil {
		return InviteIssued{}, fmt.Errorf("token: %w", err)
	}
	id := uuid.NewString()
	hash := hashInviteToken(raw)
	now := time.Now().UTC()
	inv := InviteIssued{
		Invite: Invite{
			ID:        id,
			OrgID:     orgID,
			Email:     email,
			Role:      role,
			CreatedBy: actorID,
			CreatedAt: now,
			ExpiresAt: now.Add(ttl),
		},
		Token: raw,
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO invite_tokens
		(id, org_id, email, role, token_hash, created_by, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		inv.ID, inv.OrgID, inv.Email, inv.Role, hash, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt)
	if err != nil {
		if isUniqueViolation(err) {
			return InviteIssued{}, ErrEmailTaken
		}
		return InviteIssued{}, err
	}
	s.Audit(ctx, actorID, orgID, AuditInviteIssue, inv.ID, email)
	inv.URL = buildInviteURL(publicBaseURL, raw)
	return inv, nil
}

// ListInvites returns invites in the org in creation order — relied on
// by the admin console's invite table.
func (s *Store) ListInvites(ctx context.Context, orgID string) ([]Invite, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, role, created_by, created_at, expires_at, accepted_at, accepted_by, revoked_at
		FROM invite_tokens
		WHERE org_id = $1
		ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Invite, 0)
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.AcceptedBy, &inv.RevokedAt); err != nil {
			return nil, err
		}
		inv.OrgID = orgID
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite marks the invite record as revoked so subsequent accept
// attempts return ErrInviteNotFound. Accepted invites cannot be revoked
// (they have already produced a user row); the function is a no-op on
// those records so admin tools can stamp a Revoke button unconditionally.
func (s *Store) RevokeInvite(ctx context.Context, orgID, actorID, inviteID string) error {
	if orgID == "" {
		orgID = "default"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE invite_tokens
		SET revoked_at = now()
		WHERE id = $1 AND org_id = $2 AND accepted_at IS NULL AND revoked_at IS NULL`,
		inviteID, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "already gone" from "already accepted" so the
		// UI can pick a friendly error message.
		var accepted, revoked *time.Time
		err := s.pool.QueryRow(ctx, `
			SELECT accepted_at, revoked_at FROM invite_tokens WHERE id = $1 AND org_id = $2`,
			inviteID, orgID).Scan(&accepted, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if accepted != nil {
			return errors.New("core: invite already accepted; revoke the user instead")
		}
		if revoked != nil {
			return errors.New("core: invite already revoked")
		}
		return ErrNotFound
	}
	s.Audit(ctx, actorID, orgID, AuditInviteRevoke, inviteID, "")
	return nil
}

// LookupInvite finds an invite by its raw token (sha256-hashed on the
// way in) and returns the public-facing fields so the accept page can
// show "you've been invited as <role> to <org> by <creator>". The hash
// makes the table safe in DB dumps while keeping a one-direction check
// at accept time.
func (s *Store) LookupInvite(ctx context.Context, raw string) (Invite, error) {
	hash := hashInviteToken(raw)
	var inv Invite
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, email, role, created_by, created_at, expires_at, accepted_at, accepted_by, revoked_at
		FROM invite_tokens WHERE token_hash = $1`, hash).Scan(
		&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.AcceptedBy, &inv.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, ErrInviteNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// AcceptInvite swaps an invite token for a real user record. It is
// idempotent at the storage layer — re-attempting the same token returns
// the already-created user rather than failing. Password becomes the
// invitee's first active credential; the user.create audit row fires in
// the same call so the existing user journey continues to see one event
// per activated account.
func (s *Store) AcceptInvite(ctx context.Context, raw, password string) (User, string, error) {
	hash := hashInviteToken(raw)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inv Invite
	role := ""
	err = tx.QueryRow(ctx, `
		SELECT id, org_id, email, role, accepted_at, expires_at, revoked_at
		FROM invite_tokens WHERE token_hash = $1 FOR UPDATE`, hash).Scan(
		&inv.ID, &inv.OrgID, &inv.Email, &role, &inv.AcceptedAt, &inv.ExpiresAt, &inv.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrInviteNotFound
	}
	if err != nil {
		return User{}, "", err
	}

	if inv.RevokedAt != nil {
		return User{}, "", ErrInviteNotFound
	}
	if inv.AcceptedAt != nil {
		// Re-visit path: emit the original user rather than a duplicate.
		var u User
		if err := tx.QueryRow(ctx, `
			SELECT id, org_id, email, role, enforce_limits, created_at, onboarded_at
			FROM users WHERE id = $1`, *inv.AcceptedBy).Scan(
			&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt, &u.OnboardedAt,
		); err != nil {
			return User{}, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, "", err
		}
		return u, inv.ID, nil
	}
	if time.Now().UTC().After(inv.ExpiresAt) {
		return User{}, "", ErrInviteNotFound
	}

	// Materialise user row inside the same transaction so a crash
	// between here and the COMMIT cannot leave a half-accepted invite.
	hash2, err := bcryptGenerate(password)
	if err != nil {
		return User{}, "", err
	}
	userID := uuid.NewString()
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		INSERT INTO users (id, org_id, email, password_hash, role, enforce_limits)
		VALUES ($1,$2,$3,$4,$5,TRUE)`,
		userID, inv.OrgID, inv.Email, hash2, role)
	if err != nil {
		if isUniqueViolation(err) {
			// Edge case: an admin manually created the same email
			// while the invite was open. Treat as a hard fail so the
			// admin can revoke one or the other consciously — never
			// silently overwrite a real password hash.
			return User{}, "", ErrEmailTaken
		}
		return User{}, "", err
	}
	_, err = tx.Exec(ctx, `
		UPDATE invite_tokens SET accepted_at = $1, accepted_by = $2 WHERE id = $3`,
		now, userID, inv.ID)
	if err != nil {
		return User{}, "", err
	}
	s.Audit(ctx, userID, inv.OrgID, AuditInviteAccept, inv.ID, inv.Email)
	s.Audit(ctx, userID, inv.OrgID, AuditUserCreate, userID, inv.Email)
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", err
	}
	return User{
		ID:            userID,
		OrgID:         inv.OrgID,
		Email:         inv.Email,
		Role:          role,
		EnforceLimits: true,
		CreatedAt:     now,
	}, inv.ID, nil
}
