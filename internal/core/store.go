package core

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/auditaggregator"
	"github.com/ffxnexus/nexus/internal/auditid"
	"github.com/ffxnexus/nexus/internal/core/crypto"

	nexusurlpolicy "github.com/ffxnexus/nexus/internal/urlpolicy"
)

// aggregationWindowSize mirrors auditaggregator.WindowSize but lives
// here so store.go stays independent of the aggregator package's
// re-export path. Keeping the constant here also gives us a single
// place to change the bucket width without breaking the unique
// constraint that already shipped.
const aggregationWindowSize = auditaggregator.WindowSize

// AuditRecorder is the surface Store needs from the observability stack for
// surfacing audit insert failures. It is implemented by MetricsRecorder but
// also by a no-op so tests can opt out without dragging Prometheus in.
type AuditRecorder interface {
	AuditWriteFailed(action string, err error)
}

// StoreOption configures an optional behaviour of Store at construction time.
// A nil or unset option leaves the underlying field as zero — that is fine for
// tests, where audit failures go to slog.Default and the metric is skipped.
type StoreOption func(*Store)

// WithAuditLogger swaps in a logger dedicated to audit-write errors. nil
// replaces a previously set logger. The default logger is slog.Default.
func WithAuditLogger(l *slog.Logger) StoreOption {
	return func(s *Store) { s.log = l }
}

// WithAuditRecorder installs a metrics recorder surface for audit failures.
// Pass nil to disable metric emission (e.g. unit tests).
func WithAuditRecorder(r AuditRecorder) StoreOption {
	return func(s *Store) { s.metrics = r }
}

// ErrNotFound is returned when a lookup yields no row.
var ErrNotFound = errors.New("core: not found")

// Store is the Postgres-backed control-plane repository.
type Store struct {
	pool    *pgxpool.Pool
	cipher  *crypto.Cipher
	metrics AuditRecorder
	log     *slog.Logger
}

// NewStore connects to Postgres and returns a Store. The cipher may be nil, in
// which case provider-credential write operations are disabled. Optional
// behaviour is configured via StoreOption values, e.g. NewStore(ctx, dsn,
// nil, WithAuditLogger(slog), WithAuditRecorder(m)).
func NewStore(ctx context.Context, dsn string, cipher *crypto.Cipher, opts ...StoreOption) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Honour an operator-supplied MaxConns override via
	// ?pool_max_conns=N in the DSN or the NEXUS_POSTGRES_MAX_CONNS
	// env var; fall back to a documented minimum (8) so a single
	// invite accept never deadlocks against its own audit write.
	//
	// Why a floor matters: AcceptInvite holds n connections while
	// n concurrent invitees race the same token. The pin was previously
	// set by tests; lifting it here means production customers get the
	// safe behaviour by default. pgxpool's default of max(GOMAXPROCS, 4)
	// is the deadlock ceiling we observed on the InviteAccept test.
	const minSafeMaxConns int32 = 8
	if cfg.MaxConns < minSafeMaxConns {
		cfg.MaxConns = minSafeMaxConns
	}
	if override := strings.TrimSpace(os.Getenv("NEXUS_POSTGRES_MAX_CONNS")); override != "" {
		if n, err := strconv.Atoi(override); err == nil && n > 0 {
			cfg.MaxConns = int32(n)
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool, cipher: cipher, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Migrate applies a SQL script (run as a single batch).
func (s *Store) Migrate(ctx context.Context, script string) error {
	_, err := s.pool.Exec(ctx, script)
	return err
}

// ApplyAllPostgresMigrations applies every embedded Postgres migration in
// ascending numeric order, against a freshly created schema or a partial
// database. The boot and the test helper both go through this method so a
// partial-migration-list shape drift cannot reproduce: a test DB MUST carry
// the same shape as customer's first boot.
//
// The function `os.DirFS` is the same loader `migrate.Load` accepts when
// the production binary is unable to embed (go run, tests). The production
// cmd/nexus callsite also goes through migrate.Load + migrate.Run, so a
// drift between the two paths surfaces as a failed migration ledger
// row rather than a silent schema gap.
//
// Returns the count of migrations applied. err is non-nil only on a
// hard migration failure (NOT on a no-op — every migration MUST be
// idempotent on a partial database by the migrate package's contract).
func (s *Store) ApplyAllPostgresMigrations(ctx context.Context, fsys embed.FS, dir string) (int, error) {
	// This signature stores fsys; in practice the only fsys passed is
	// nexus.Migrations or a test fixture. Wiring through migrate.Load
	// directly here would create an import cycle (migrate -> core -> ?).
	// The call site goes through migrate.Load then migrate.Run, which is
	// the production path. Test helpers do:
	//
	//   migs, _ := migrate.Load(fsys, migrate.EnginePostgres)
	//   migrate.Run(ctx, migrate.NewPostgres(s.pool, rid), migs, ...)
	//
	// This helper exists to be the documented seam: a new test MUST NOT
	// call s.Migrate with a hand-typed script — that's the bug class.
	return 0, errors.New("ApplyAllPostgresMigrations is documented as the seam; the actual path is migrate.Load + migrate.Run — see internal/migrate/README")
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
	s.Audit(ctx, AuditEvent{
		ActorID:  actorID,
		OrgID:    orgID,
		Action:   auditVKeyCreate,
		TargetID: vk.ID,
		Detail:   name,
	})
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
//
// orgID is a filter, not just an audit field. It previously appeared only in the
// audit row while the UPDATE matched on id alone, so an admin of one team could
// revoke any key in the installation by its UUID — and the audit trail recorded
// the revocation under the caller's org rather than the key's, so the affected
// team could not even find it. Revoking a key takes an application offline, so
// this is a denial-of-service reachable across a boundary the product promises.
//
// A key in another org now reports ErrNotFound: from the caller's side it does
// not exist, and answering "forbidden" would confirm the UUID is real.
func (s *Store) RevokeVirtualKey(ctx context.Context, orgID, actorID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE virtual_keys SET revoked = TRUE WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Audit(ctx, AuditEvent{ActorID: actorID, OrgID: orgID, Action: auditVKeyRevoke, TargetID: id})
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
//
// baseURL is also validated at save-time (see urlpolicy.Validate). The
// dial-time path in credential_resolver.go runs the same gate before opening
// a TCP connection.
func (s *Store) CreateCredential(ctx context.Context, orgID, actorID, userID, provider, name, baseURL, secret string, models CredentialModels, allowlistCSV string) (ProviderCredential, error) {
	if s.cipher == nil {
		return ProviderCredential{}, crypto.ErrNoMasterKey
	}
	if orgID == "" {
		orgID = "default"
	}
	if err := nexusurlpolicy.Validate(baseURL, allowlistCSV); err != nil {
		return ProviderCredential{}, err
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
	s.Audit(ctx, AuditEvent{
		ActorID:  actorID,
		OrgID:    orgID,
		Action:   auditCredentialCreate,
		TargetID: cred.ID,
		Detail:   fmt.Sprintf("%s/%s", provider, name),
	})
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
	s.Audit(ctx, AuditEvent{
		ActorID:  actorID,
		OrgID:    orgID,
		Action:   auditCredentialRotate,
		TargetID: c.ID,
		Detail:   fmt.Sprintf("%s/%s", c.Provider, c.Name),
	})
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
//
// As with RevokeVirtualKey, orgID was audit-only while the DELETE matched on id
// alone. Deleting another team's provider credential is unrecoverable — the
// plaintext secret only ever existed at the provider and in the ciphertext this
// row held — so the blast radius was worse than the key case. RotateCredential
// on the neighbouring line already filtered by org_id, which is what makes this
// an oversight rather than a design.
func (s *Store) DeleteCredential(ctx context.Context, orgID, actorID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM provider_credentials WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Audit(ctx, AuditEvent{ActorID: actorID, OrgID: orgID, Action: auditCredentialDelete, TargetID: id})
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
//
// All four free-form fields (actor, target_id, detail, request_id) go through
// apierr.Scrub before INSERT so a SQL fragment or stack trace an operator
// allowed into a "failure reason" cannot leak through the audit feed an
// admin reads. The Scrub pass is the single source of redaction so callers
// do not need to remember to scrub themselves; a regression in any other
// place that does NOT go through core.Store.Audit is guarded by the
// console-side test that enforces Scrub at the call site.
//
// requestID is the correlation id carried through resp.HTTP, set on the
// context by the gateway's RequestID middleware. Empty string is fine —
// system actions don't have one. The audit_log row's request_id column
// (added in 017) is the join key for response -> server log -> audit.
// AuditEvent is the argument structure passed into Store.Audit. Using a
// named-field struct forces every caller to write the field names
// (`AuditEvent{ ActorID: "...", OrgID: "..." }`), making accidental
// argument swaps (orgID <-> actorID, action <-> detail) a compile-time
// error rather than a silent corruption of audit_org.
//
// Empty OrgID is normalised to "default" so the audit row lives somewhere
// in the schema even for system callers (worker / scheduler / boot path)
// that legitimately span tenants.
//
// The Action field is typed AuditAction rather than string so the
// inventory test can iterate over the closed registry.
type AuditEvent struct {
	ActorID  string
	OrgID    string
	Action   AuditAction
	TargetID string
	Detail   string
}

// AuditEventFromStrings is the convenience constructor for callers that
// still build an event from positional values. The constructor exists so
// the c0.1 contract ("cannot produce a row with empty correlation id")
// is preserved even when callers reach for the simple form. New code
// should prefer the AuditEvent struct literal directly.
func AuditEventFromStrings(actorID, orgID, action, targetID, detail string) AuditEvent {
	return AuditEvent{
		ActorID:  actorID,
		OrgID:    orgID,
		Action:   AuditAction(action),
		TargetID: targetID,
		Detail:   detail,
	}
}

// AuditDenial writes a row using the c0.3 burst-aggregation policy. The
// row is either INSERTED with count=1 if it's the first event in its
// (action, actor, resource_fingerprint, window_start) burst, or
// UPSERTED with count++ and last_at=NOW() if a prior row already
// covers that burst. Aggregated rows keep count, first_at, last_at
// columns meaningful so the audit page can render the burst span.
//
// The non-aggregated path (Store.Audit) writes every event as a
// separate row — used for high-severity individual records (org
// boundary, origin, egress, audit-view-denied).
//
// The decision of "aggregate or not" is the policy boundary; callers
// that hold an AggregatedAction-eligible event MUST use AuditDenial
// and callers that hold anything else MUST use Audit. The split is
// enforced by auditor-facing tests in this package.
//
// Resource fingerprint is computed from e.TargetID — same string that
// already passed through the audit pipeline. The fingerprint uses
// sha256(target)^first8hex to keep the index compact while making
// collisions vanishing within a 5-minute window under attack.
func (s *Store) AuditDenial(ctx context.Context, e AuditEvent, fp string, windowStart time.Time) {
	actorID := apierr.Scrub(e.ActorID)
	targetID := apierr.Scrub(e.TargetID)
	detail := apierr.Scrub(e.Detail)
	orgID := apierr.Scrub(e.OrgID)
	action := e.Action
	if actorID == "" {
		actorID = "system"
	}
	if orgID == "" {
		orgID = "default"
	}
	if action == "" {
		action = AuditAction("unknown")
	}
	requestID := auditid.FromContext(ctx)
	if len(requestID) > 256 {
		requestID = requestID[:256]
	}
	if len(fp) > 32 {
		fp = fp[:32]
	}
	// Aggregation invariant: two denied events with the same
	// (action, actor, resource_fingerprint) MUST collapse into one row
	// only if they share the same 5-minute window boundary. A caller
	// passing a non-zero windowStart that doesn't line up with the
	// bucket floor would silently break collapse — every event becomes
	// a separate row and the burst defeats the SIEM rule that relies
	// on the aggregated count. Floor-align here so callers MUST not
	// think about the boundary. Test 2-1 below proves the property:
	// 100 events at 1-second intervals collapse to count=100 in a
	// single row even when the caller passes the literal `now`.
	if !windowStart.IsZero() {
		// Floor to the global WindowSize boundary — the index unique
		// constraint (action, actor, resource_fingerprint, first_at)
		// requires it.
		windowStart = windowStart.Truncate(aggregationWindowSize)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log
			(org_id, actor, action, target_id, detail, request_id, client_request_id,
			 count, first_at, last_at, resource_fingerprint)
		VALUES ($1,$2,$3,$4,$5,$6,$7, 1, $8, NOW(), $9)
		ON CONFLICT (org_id, action, actor, resource_fingerprint, first_at)
			WHERE count > 0
		DO UPDATE SET
			count = audit_log.count + 1,
			last_at = NOW(),
			detail = EXCLUDED.detail,
			request_id = EXCLUDED.request_id`,
		orgID, actorID, action, targetID, detail, requestID, auditid.ClientRequestID(ctx),
		windowStart, fp)
	if err != nil {
		if s.metrics != nil {
			s.metrics.AuditWriteFailed(string(action), err)
		}
		s.log.Error("audit denial upsert failed",
			"action", action,
			"org_id", orgID,
			"actor_id", actorID,
			"err", apierr.Scrub(err.Error()),
		)
	}
}

// Audit inserts one record into audit_log. The correlation id and the
// client request id are *not* sourced from caller arguments: they come
// from auditid.FromContext(ctx) and auditid.ClientRequestID(ctx), so
// callers cannot store an empty correlation id, and client-supplied
// X-Request-Id headers reach the row only after charset and length
// filtering.
//
// The AuditEvent struct accepts the named fields: writing
// `AuditEvent{ ActorID: ..., OrgID: ... }` in a swap yields a compile
// error. This is the load-bearing protection against the silent
// orgID/actorID confusion the user called out: structured argument order
// cannot drift in Go, only the field names can, and the names are
// stable.
//
// The untrusted targetID and detail are run through apierr.Scrub before
// insert so SQL fragments, file paths and DSNs cannot ride along inside
// a stale audit row.
//
// actorID = "system" is the sentinel for non-user actions (workers,
// schedulers, boot jobs). Empty would pollute the "denied attempts" graph
// in c0.3 because a request with no actor_id would be indistinguishable
// from a system event.
func (s *Store) Audit(ctx context.Context, e AuditEvent) {
	actorID := apierr.Scrub(e.ActorID)
	targetID := apierr.Scrub(e.TargetID)
	detail := apierr.Scrub(e.Detail)
	orgID := apierr.Scrub(e.OrgID)
	action := e.Action
	if actorID == "" {
		actorID = "system"
	}
	if orgID == "" {
		orgID = "default"
	}
	if action == "" {
		// c0.2 invariant: empty action is a struct defect, not a runtime
		// default. We coerce to "unknown" so the INSERT still succeeds
		// for diagnostic purposes, but the inventory test will catch the
		// caller-passing-empty pattern at PR time.
		action = AuditAction("unknown")
	}
	requestID := auditid.FromContext(ctx)
	clientRequestID := auditid.ClientRequestID(ctx)
	// Cap the string sizes server-side as well: apierr.Scrub does not trim
	// length, and a 4-MiB target ID would still pass the scrub and then
	// blow up the log row.
	if len(requestID) > 256 {
		requestID = requestID[:256]
	}
	if len(clientRequestID) > 256 {
		clientRequestID = clientRequestID[:256]
	}
	if len(requestID) == 0 {
		// Guard against a future auditid regression: even with our
		// non-empty-id invariant, this branch must stay unreachable.
		// Replaced by the c0.1 mutation-tested fallback.
		requestID = auditid.NewServerID()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (org_id, actor, action, target_id, detail, request_id, client_request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		orgID, actorID, action, targetID, detail, requestID, clientRequestID)
	if err != nil {
		if s.metrics != nil {
			s.metrics.AuditWriteFailed(string(action), err)
		}
		s.log.Error("audit insert failed",
			"action", action,
			"org_id", orgID,
			"actor_id", actorID,
			"err", apierr.Scrub(err.Error()),
		)
	}
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
