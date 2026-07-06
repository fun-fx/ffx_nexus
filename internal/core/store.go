package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ffxnexus/nexus/internal/core/crypto"
)

// ErrNotFound is returned when a lookup yields no row.
var ErrNotFound = errors.New("core: not found")

// Store is the Postgres-backed control-plane repository.
type Store struct {
	pool   *pgxpool.Pool
	cipher *crypto.Cipher
}

// NewStore connects to Postgres and returns a Store. The cipher may be nil, in
// which case provider-credential write operations are disabled.
func NewStore(ctx context.Context, dsn string, cipher *crypto.Cipher) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, cipher: cipher}, nil
}

// Migrate applies a SQL script (run as a single batch).
func (s *Store) Migrate(ctx context.Context, script string) error {
	_, err := s.pool.Exec(ctx, script)
	return err
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// HasCipher reports whether secret encryption is available.
func (s *Store) HasCipher() bool { return s.cipher != nil }

// Pool exposes the underlying connection pool for callers that need to run
// hand-written SQL outside the helpers in this file (the SSO callback
// looks up users by their (provider, subject) identity, which is not a
// hot enough path to warrant its own typed method).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// --- Virtual keys ---

// CreateVirtualKey generates a key, stores its hash, and returns the row plus
// the one-time plaintext. userID may be empty for an org-level key. actorID
// is the user_id of the caller (empty for system); recorded in the audit log.
func (s *Store) CreateVirtualKey(ctx context.Context, orgID, actorID, userID, name string, allowedModels []string, rpm int, monthlyBudget, minQuality float64) (VirtualKey, string, error) {
	if orgID == "" {
		orgID = "default"
	}
	plaintext, prefix, last4 := GenerateVirtualKey()
	vk := VirtualKey{
		ID:            uuid.NewString(),
		OrgID:         orgID,
		UserID:        userID,
		Name:          name,
		KeyPrefix:     prefix,
		KeyLast4:      last4,
		AllowedModels: allowedModels,
		RPMLimit:      rpm,
		MonthlyBudget: monthlyBudget,
		MinQuality:    minQuality,
	}
	if vk.AllowedModels == nil {
		vk.AllowedModels = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO virtual_keys
			(id, org_id, user_id, name, key_hash, key_prefix, key_last4,
			 allowed_models, rpm_limit, monthly_budget_usd, min_quality_score)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		vk.ID, vk.OrgID, nullStr(userID), vk.Name, crypto.HashKey(plaintext), vk.KeyPrefix, vk.KeyLast4,
		vk.AllowedModels, vk.RPMLimit, vk.MonthlyBudget, vk.MinQuality)
	if err != nil {
		return VirtualKey{}, "", err
	}
	s.Audit(ctx, actorID, orgID, "vkey.create", vk.ID, name)
	return vk, plaintext, nil
}

// AuthorizedKey is a virtual key plus the owning user's enforcement toggle,
// resolved in a single lookup for the auth hot path. EnforceLimits defaults to
// true for org-level keys (no owning user).
type AuthorizedKey struct {
	VirtualKey
	EnforceLimits bool
}

// LookupVirtualKey finds an active (non-revoked) key by its plaintext value,
// joining the owning user (if any) to surface the per-user enforce_limits flag.
func (s *Store) LookupVirtualKey(ctx context.Context, plaintext string) (AuthorizedKey, error) {
	var ak AuthorizedKey
	var userID *string
	var enforce *bool
	err := s.pool.QueryRow(ctx, `
		SELECT vk.id, vk.org_id, vk.user_id, vk.name, vk.key_prefix, vk.key_last4,
		       vk.allowed_models, vk.rpm_limit, vk.monthly_budget_usd, vk.min_quality_score,
		       vk.revoked, vk.created_at, u.enforce_limits
		FROM virtual_keys vk
		LEFT JOIN users u ON u.id = vk.user_id
		WHERE vk.key_hash = $1 AND vk.revoked = FALSE`,
		crypto.HashKey(plaintext)).Scan(
		&ak.ID, &ak.OrgID, &userID, &ak.Name, &ak.KeyPrefix, &ak.KeyLast4, &ak.AllowedModels,
		&ak.RPMLimit, &ak.MonthlyBudget, &ak.MinQuality, &ak.Revoked, &ak.CreatedAt, &enforce)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizedKey{}, ErrNotFound
	}
	if userID != nil {
		ak.UserID = *userID
	}
	// Org-level keys (no user) always enforce; user keys honor their toggle.
	ak.EnforceLimits = enforce == nil || *enforce
	return ak, err
}

// ListVirtualKeys returns all keys for an org (no secrets).
func (s *Store) ListVirtualKeys(ctx context.Context, orgID string) ([]VirtualKey, error) {
	return s.listVirtualKeys(ctx, orgID, "")
}

// ListVirtualKeysForUser returns only the keys owned by a specific user.
func (s *Store) ListVirtualKeysForUser(ctx context.Context, orgID, userID string) ([]VirtualKey, error) {
	return s.listVirtualKeys(ctx, orgID, userID)
}

func (s *Store) listVirtualKeys(ctx context.Context, orgID, userID string) ([]VirtualKey, error) {
	if orgID == "" {
		orgID = "default"
	}
	query := `
		SELECT id, org_id, user_id, name, key_prefix, key_last4, allowed_models,
		       rpm_limit, monthly_budget_usd, min_quality_score, revoked, created_at
		FROM virtual_keys WHERE org_id = $1`
	args := []any{orgID}
	if userID != "" {
		query += ` AND user_id = $2`
		args = append(args, userID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VirtualKey
	for rows.Next() {
		var vk VirtualKey
		var uid *string
		if err := rows.Scan(
			&vk.ID, &vk.OrgID, &uid, &vk.Name, &vk.KeyPrefix, &vk.KeyLast4, &vk.AllowedModels,
			&vk.RPMLimit, &vk.MonthlyBudget, &vk.MinQuality, &vk.Revoked, &vk.CreatedAt,
		); err != nil {
			return nil, err
		}
		if uid != nil {
			vk.UserID = *uid
		}
		out = append(out, vk)
	}
	return out, rows.Err()
}

// RevokeVirtualKey marks a key revoked. actorID is the user_id of the caller
// (empty for system); recorded in the audit log.
func (s *Store) RevokeVirtualKey(ctx context.Context, orgID, actorID, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE virtual_keys SET revoked = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Audit(ctx, actorID, orgID, "vkey.revoke", id, "")
	return nil
}

// --- Provider credentials ---

// CreateCredential encrypts and stores an upstream provider secret. userID may
// be empty for an org-level (shared/central) credential, or set for a BYOK
// credential private to that user. actorID is the user_id of the caller
// (empty for system); recorded in the audit log.
//
// models carries the optional per-credential model inventory the owner wants
// advertised at /v1/models; pass an empty CredentialModels (not nil) for
// built-in providers that ship their own catalog.
func (s *Store) CreateCredential(ctx context.Context, orgID, actorID, userID, provider, name, baseURL, secret string, models CredentialModels) (ProviderCredential, error) {
	if s.cipher == nil {
		return ProviderCredential{}, crypto.ErrNoMasterKey
	}
	if orgID == "" {
		orgID = "default"
	}
	ct, err := s.cipher.Encrypt([]byte(secret))
	if err != nil {
		return ProviderCredential{}, err
	}
	cred := ProviderCredential{
		ID:          uuid.NewString(),
		OrgID:       orgID,
		UserID:      userID,
		Provider:    provider,
		Name:        name,
		BaseURL:     baseURL,
		Models:      models,
		SecretLast4: Last4(secret),
		Enabled:     true,
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return ProviderCredential{}, err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO provider_credentials
			(id, org_id, user_id, provider, name, base_url, secret_ciphertext, secret_last4, enabled, models)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9)`,
		cred.ID, cred.OrgID, nullStr(userID), cred.Provider, cred.Name, cred.BaseURL, ct, cred.SecretLast4, modelsJSON)
	if err != nil {
		return ProviderCredential{}, err
	}
	s.Audit(ctx, actorID, orgID, "credential.create", cred.ID, fmt.Sprintf("%s/%s", provider, name))
	return cred, nil
}

// ListCredentials returns org-level credential metadata (no secrets) — i.e.
// credentials with no owning user (user_id IS NULL).
func (s *Store) ListCredentials(ctx context.Context, orgID string) ([]ProviderCredential, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, user_id, provider, name, base_url, secret_last4, enabled, created_at, rotated_at, models
		FROM provider_credentials WHERE org_id = $1 AND user_id IS NULL ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	return scanCredentials(rows)
}

// ListCredentialsForUser returns the BYOK credential metadata owned by a user.
func (s *Store) ListCredentialsForUser(ctx context.Context, orgID, userID string) ([]ProviderCredential, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, user_id, provider, name, base_url, secret_last4, enabled, created_at, rotated_at, models
		FROM provider_credentials WHERE org_id = $1 AND user_id = $2 ORDER BY created_at DESC`, orgID, userID)
	if err != nil {
		return nil, err
	}
	return scanCredentials(rows)
}

func scanCredentials(rows pgx.Rows) ([]ProviderCredential, error) {
	defer rows.Close()
	var out []ProviderCredential
	for rows.Next() {
		var c ProviderCredential
		var uid *string
		var modelsRaw []byte
		if err := rows.Scan(&c.ID, &c.OrgID, &uid, &c.Provider, &c.Name, &c.BaseURL, &c.SecretLast4, &c.Enabled, &c.CreatedAt, &c.RotatedAt, &modelsRaw); err != nil {
			return nil, err
		}
		if uid != nil {
			c.UserID = *uid
		}
		if len(modelsRaw) > 0 {
			if err := json.Unmarshal(modelsRaw, &c.Models); err != nil {
				return nil, fmt.Errorf("decode models for credential %s: %w", c.ID, err)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RotateCredential replaces the secret of an existing credential in place,
// re-encrypting it under the master key and recording the rotation time. The
// credential keeps its ID, provider, name, and base URL so existing references
// (and registered providers) stay valid — only the secret material changes.
// actorID is the user_id of the caller (empty for system); recorded in the
// audit log.
// Returns the updated metadata (never the plaintext).
func (s *Store) RotateCredential(ctx context.Context, orgID, actorID, id, newSecret string) (ProviderCredential, error) {
	if s.cipher == nil {
		return ProviderCredential{}, crypto.ErrNoMasterKey
	}
	if orgID == "" {
		orgID = "default"
	}
	ct, err := s.cipher.Encrypt([]byte(newSecret))
	if err != nil {
		return ProviderCredential{}, err
	}
	var c ProviderCredential
	err = s.pool.QueryRow(ctx, `
		UPDATE provider_credentials
		SET secret_ciphertext = $1, secret_last4 = $2, rotated_at = now()
		WHERE id = $3 AND org_id = $4
		RETURNING id, org_id, provider, name, base_url, secret_last4, enabled, created_at, rotated_at`,
		ct, Last4(newSecret), id, orgID).Scan(
		&c.ID, &c.OrgID, &c.Provider, &c.Name, &c.BaseURL, &c.SecretLast4, &c.Enabled, &c.CreatedAt, &c.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderCredential{}, ErrNotFound
	}
	if err != nil {
		return ProviderCredential{}, err
	}
	s.Audit(ctx, actorID, orgID, "credential.rotate", c.ID, fmt.Sprintf("%s/%s", c.Provider, c.Name))
	return c, nil
}

// DecryptedCredential is a credential with its plaintext secret, used internally
// to register providers at startup. Never serialized to API responses.
type DecryptedCredential struct {
	ProviderCredential
	Secret string
}

// LoadEnabledCredentials returns enabled org-level credentials (user_id IS NULL)
// with decrypted secrets. These are the shared/central credentials registered
// at boot. Per-user (BYOK) credentials are resolved per request via
// ResolveCredential, not registered globally.
func (s *Store) LoadEnabledCredentials(ctx context.Context, orgID string) ([]DecryptedCredential, error) {
	if s.cipher == nil {
		return nil, crypto.ErrNoMasterKey
	}
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, provider, name, base_url, secret_ciphertext, secret_last4, enabled, created_at, models
		FROM provider_credentials WHERE org_id = $1 AND user_id IS NULL AND enabled = TRUE ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DecryptedCredential
	for rows.Next() {
		var c DecryptedCredential
		var ct, modelsRaw []byte
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Provider, &c.Name, &c.BaseURL, &ct, &c.SecretLast4, &c.Enabled, &c.CreatedAt, &modelsRaw); err != nil {
			return nil, err
		}
		secret, err := s.cipher.Decrypt(ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt credential %s: %w", c.ID, err)
		}
		c.Secret = string(secret)
		if len(modelsRaw) > 0 {
			if err := json.Unmarshal(modelsRaw, &c.Models); err != nil {
				return nil, fmt.Errorf("decode models for credential %s: %w", c.ID, err)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveCredential looks up a single enabled credential for a provider on
// behalf of a caller, honoring BYOK precedence: a user-owned credential wins
// over an org-level one. It returns the decrypted secret + base URL and a source
// tag ("user" or "org"). When userID is empty only org-level credentials are
// considered. Returns ErrNotFound when nothing matches.
//
// This runs on the hot path; callers should cache the result (see the gateway's
// credential cache) keyed by credential ID so the AES-GCM decrypt and DB hit do
// not repeat per request.
func (s *Store) ResolveCredential(ctx context.Context, orgID, userID, provider string) (DecryptedCredential, string, error) {
	if s.cipher == nil {
		return DecryptedCredential{}, "", crypto.ErrNoMasterKey
	}
	if orgID == "" {
		orgID = "default"
	}
	// Order: user-owned first (BYOK), then org-level. created_at as tiebreaker.
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, user_id, provider, name, base_url, secret_ciphertext, secret_last4, enabled, created_at, models
		FROM provider_credentials
		WHERE org_id = $1 AND provider = $2 AND enabled = TRUE
		  AND (user_id = $3 OR user_id IS NULL)
		ORDER BY (user_id IS NULL), created_at`, orgID, provider, nullStr(userID))
	if err != nil {
		return DecryptedCredential{}, "", err
	}
	defer rows.Close()

	if !rows.Next() {
		return DecryptedCredential{}, "", ErrNotFound
	}
	var c DecryptedCredential
	var uid *string
	var ct, modelsRaw []byte
	if err := rows.Scan(&c.ID, &c.OrgID, &uid, &c.Provider, &c.Name, &c.BaseURL, &ct, &c.SecretLast4, &c.Enabled, &c.CreatedAt, &modelsRaw); err != nil {
		return DecryptedCredential{}, "", err
	}
	secret, err := s.cipher.Decrypt(ct)
	if err != nil {
		return DecryptedCredential{}, "", fmt.Errorf("decrypt credential %s: %w", c.ID, err)
	}
	c.Secret = string(secret)
	if len(modelsRaw) > 0 {
		if err := json.Unmarshal(modelsRaw, &c.Models); err != nil {
			return DecryptedCredential{}, "", fmt.Errorf("decode models for credential %s: %w", c.ID, err)
		}
	}
	source := "org"
	if uid != nil {
		c.UserID = *uid
		source = "user"
	}
	return c, source, nil
}

// nullStr maps an empty string to a SQL NULL so optional FK columns stay NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DeleteCredential removes a credential. actorID is the user_id of the caller
// (empty for system); recorded in the audit log.
func (s *Store) DeleteCredential(ctx context.Context, orgID, actorID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Audit(ctx, actorID, orgID, "credential.delete", id, "")
	return nil
}

// AuditEntry is one row of the audit_log table, surfaced to /api/audit.
type AuditEntry struct {
	ID        int64     `json:"id"`
	OrgID     string    `json:"org_id"`
	ActorID   string    `json:"actor"` // user_id of the caller; "system" for non-user actions
	Action    string    `json:"action"`
	TargetID  string    `json:"target_id"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditListOptions filters the /api/audit response.
type AuditListOptions struct {
	Limit   int       // max rows; default 50, hard cap 500
	Action  string    // exact-match filter; empty = no filter
	ActorID string    // exact-match filter on actor; empty = no filter
	Since   time.Time // only entries newer than this; zero = no filter
}

// Audit writes a best-effort audit entry; failures are swallowed. actorID is
// the user_id of the caller; pass "" for system actions. The audit_log table
// (created in 001_init.sql) stores the value in the existing `actor` column,
// which has a DEFAULT 'system' fallback.
func (s *Store) Audit(ctx context.Context, actorID, orgID, action, targetID, detail string) {
	if actorID == "" {
		actorID = "system"
	}
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO audit_log (org_id, actor, action, target_id, detail)
		VALUES ($1,$2,$3,$4,$5)`, orgID, actorID, action, targetID, detail)
}

// ListAudit reads the most recent entries for an org, applying the supplied
// filters. Used by GET /api/audit.
func (s *Store) ListAudit(ctx context.Context, orgID string, opts AuditListOptions) ([]AuditEntry, error) {
	if orgID == "" {
		orgID = "default"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	query := `
		SELECT id, org_id, actor, action, target_id, detail, created_at
		FROM audit_log
		WHERE org_id = $1`
	args := []any{orgID}
	if opts.Action != "" {
		args = append(args, opts.Action)
		query += ` AND action = $` + strconv.Itoa(len(args))
	}
	if opts.ActorID != "" {
		args = append(args, opts.ActorID)
		query += ` AND actor = $` + strconv.Itoa(len(args))
	}
	if !opts.Since.IsZero() {
		args = append(args, opts.Since)
		query += ` AND created_at >= $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.OrgID, &e.ActorID, &e.Action, &e.TargetID, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
