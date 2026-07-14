package core

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/ffxnexus/nexus/internal/core/crypto"
)

// ErrEmailTaken is returned when creating a user whose email already exists in
// the org.
var ErrEmailTaken = errors.New("core: email already registered")

// ErrInvalidCredentials is returned on a failed login (unknown email or wrong
// password) without disclosing which.
var ErrInvalidCredentials = errors.New("core: invalid credentials")

// --- Users ---

// CreateUser provisions a user with a bcrypt-hashed password. role defaults to
// "member" when empty. actorID is the user_id of the caller (empty for
// self-signup or system); recorded in the audit log.
func (s *Store) CreateUser(ctx context.Context, orgID, actorID, email, password, role string) (User, error) {
	if orgID == "" {
		orgID = "default"
	}
	if role == "" {
		role = RoleMember
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	u := User{
		ID:            uuid.NewString(),
		OrgID:         orgID,
		Email:         email,
		Role:          role,
		EnforceLimits: true,
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO users (id, org_id, email, password_hash, role, enforce_limits)
		VALUES ($1,$2,$3,$4,$5,TRUE)`,
		u.ID, u.OrgID, u.Email, string(hash), u.Role)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	s.Audit(ctx, actorID, orgID, AuditUserCreate, u.ID, email)
	return u, nil
}

// GetUser returns a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, email, role, enforce_limits, created_at
		FROM users WHERE id = $1`, id).Scan(
		&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// ListUsers returns all users in an org (no password hashes).
func (s *Store) ListUsers(ctx context.Context, orgID string) ([]User, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, email, role, enforce_limits, created_at
		FROM users WHERE org_id = $1 ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserByEmail looks up a user by email within an org. Used by the SSO
// callback to decide whether the incoming IdP identity should be linked to
// an existing account or should trigger JIT provisioning.
func (s *Store) GetUserByEmail(ctx context.Context, orgID, email string) (User, error) {
	if orgID == "" {
		orgID = "default"
	}
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, email, role, enforce_limits, created_at
		FROM users WHERE org_id = $1 AND email = $2`, orgID, email).Scan(
		&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// LinkSSOIdentity records that a user is now bound to a specific
// (provider, subject) pair at the given issuer. Idempotent: re-binding to
// the same identity is a no-op. Returns the (possibly already-existing) row.
func (s *Store) LinkSSOIdentity(ctx context.Context, userID, provider, subject, issuer string) error {
	if subject == "" || provider == "" {
		return nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET sso_provider = $1, sso_subject = $2, sso_issuer = $3
		WHERE id = $4
		  AND (sso_subject IS NULL
		       OR (sso_provider = $1 AND sso_subject = $2))`,
		provider, subject, issuer, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The user is already bound to a different identity — refuse to
		// silently re-link. Surfaced as ErrNotFound so the caller can show
		// a clear error rather than a generic 500.
		return ErrNotFound
	}
	return nil
}

// CreateSession inserts a session row directly, without a password check.
// Used by the SSO callback (and any future non-password login path) where
// the caller has already authenticated the identity out-of-band.
func (s *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	token := GenerateSessionToken()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_sessions (token_hash, user_id, expires_at)
		VALUES ($1,$2,$3)`, crypto.HashKey(token), userID, time.Now().Add(ttl))
	if err != nil {
		return "", err
	}
	return token, nil
}

// SetEnforceLimits flips the per-user budget/RPM enforcement toggle.
func (s *Store) SetEnforceLimits(ctx context.Context, userID string, enforce bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET enforce_limits = $1 WHERE id = $2`, enforce, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user (cascades to their keys, credentials, sessions).
// actorID is the user_id of the caller (empty for system); recorded in the
// audit log.
func (s *Store) DeleteUser(ctx context.Context, orgID, actorID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Audit(ctx, actorID, orgID, AuditUserDelete, id, "")
	return nil
}

// CountUsers returns the number of users in an org. Used to decide whether to
// bootstrap a first admin.
func (s *Store) CountUsers(ctx context.Context, orgID string) (int, error) {
	if orgID == "" {
		orgID = "default"
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE org_id = $1`, orgID).Scan(&n)
	return n, err
}

// --- Sessions / login ---

// Authenticate verifies an email+password and, on success, creates a session
// returning the plaintext session token (stored only as a hash) and the user.
// The audit log records the authenticated user as the actor (self-action).
func (s *Store) Authenticate(ctx context.Context, orgID, email, password string, ttl time.Duration) (string, User, error) {
	if orgID == "" {
		orgID = "default"
	}
	var u User
	var hash string
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, email, role, enforce_limits, created_at, password_hash
		FROM users WHERE org_id = $1 AND email = $2`, orgID, email).Scan(
		&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", User{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", User{}, ErrInvalidCredentials
	}
	token := GenerateSessionToken()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_sessions (token_hash, user_id, expires_at)
		VALUES ($1,$2,$3)`, crypto.HashKey(token), u.ID, time.Now().Add(ttl))
	if err != nil {
		return "", User{}, err
	}
	s.Audit(ctx, u.ID, orgID, AuditUserLogin, u.ID, email)
	return token, u, nil
}

// UserBySession resolves the user behind a plaintext session token, enforcing
// expiry. Returns ErrNotFound for unknown/expired tokens.
func (s *Store) UserBySession(ctx context.Context, token string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.org_id, u.email, u.role, u.enforce_limits, u.created_at
		FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`,
		crypto.HashKey(token)).Scan(
		&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Logout deletes a session by its plaintext token. Unknown tokens are a no-op.
func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, crypto.HashKey(token))
	return err
}

// RotateUserCredential rotates a credential owned by a specific user (BYOK),
// ensuring users can only rotate their own secrets. The audit log records the
// owning user as the actor (self-action).
func (s *Store) RotateUserCredential(ctx context.Context, orgID, userID, id, newSecret string) (ProviderCredential, error) {
	if s.cipher == nil {
		return ProviderCredential{}, crypto.ErrNoMasterKey
	}
	ct, err := s.cipher.Encrypt([]byte(newSecret))
	if err != nil {
		return ProviderCredential{}, err
	}
	var c ProviderCredential
	var uid *string
	err = s.pool.QueryRow(ctx, `
		UPDATE provider_credentials
		SET secret_ciphertext = $1, secret_last4 = $2, rotated_at = now()
		WHERE id = $3 AND org_id = $4 AND user_id = $5
		RETURNING id, org_id, user_id, provider, name, base_url, secret_last4, enabled, created_at, rotated_at`,
		ct, Last4(newSecret), id, orgID, userID).Scan(
		&c.ID, &c.OrgID, &uid, &c.Provider, &c.Name, &c.BaseURL, &c.SecretLast4, &c.Enabled, &c.CreatedAt, &c.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderCredential{}, ErrNotFound
	}
	if err != nil {
		return ProviderCredential{}, err
	}
	if uid != nil {
		c.UserID = *uid
	}
	s.Audit(ctx, userID, orgID, AuditCredentialRotate, c.ID, c.Provider)
	return c, nil
}

// DeleteUserCredential deletes a credential owned by a specific user (BYOK).
// The audit log records the owning user as the actor (self-action).
func (s *Store) DeleteUserCredential(ctx context.Context, orgID, userID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE id = $1 AND org_id = $2 AND user_id = $3`, id, orgID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Audit(ctx, userID, orgID, AuditCredentialDelete, id, "")
	return nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
