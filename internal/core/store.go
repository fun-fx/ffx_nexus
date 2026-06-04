package core

import (
	"context"
	"errors"
	"fmt"

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

// --- Virtual keys ---

// CreateVirtualKey generates a key, stores its hash, and returns the row plus
// the one-time plaintext.
func (s *Store) CreateVirtualKey(ctx context.Context, orgID, name string, allowedModels []string, rpm int, monthlyBudget, minQuality float64) (VirtualKey, string, error) {
	if orgID == "" {
		orgID = "default"
	}
	plaintext, prefix, last4 := GenerateVirtualKey()
	vk := VirtualKey{
		ID:            uuid.NewString(),
		OrgID:         orgID,
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
			(id, org_id, name, key_hash, key_prefix, key_last4,
			 allowed_models, rpm_limit, monthly_budget_usd, min_quality_score)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		vk.ID, vk.OrgID, vk.Name, crypto.HashKey(plaintext), vk.KeyPrefix, vk.KeyLast4,
		vk.AllowedModels, vk.RPMLimit, vk.MonthlyBudget, vk.MinQuality)
	if err != nil {
		return VirtualKey{}, "", err
	}
	s.audit(ctx, orgID, "vkey.create", vk.ID, name)
	return vk, plaintext, nil
}

// LookupVirtualKey finds an active (non-revoked) key by its plaintext value.
func (s *Store) LookupVirtualKey(ctx context.Context, plaintext string) (VirtualKey, error) {
	var vk VirtualKey
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, name, key_prefix, key_last4, allowed_models,
		       rpm_limit, monthly_budget_usd, min_quality_score, revoked, created_at
		FROM virtual_keys
		WHERE key_hash = $1 AND revoked = FALSE`,
		crypto.HashKey(plaintext)).Scan(
		&vk.ID, &vk.OrgID, &vk.Name, &vk.KeyPrefix, &vk.KeyLast4, &vk.AllowedModels,
		&vk.RPMLimit, &vk.MonthlyBudget, &vk.MinQuality, &vk.Revoked, &vk.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VirtualKey{}, ErrNotFound
	}
	return vk, err
}

// ListVirtualKeys returns all keys for an org (no secrets).
func (s *Store) ListVirtualKeys(ctx context.Context, orgID string) ([]VirtualKey, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, name, key_prefix, key_last4, allowed_models,
		       rpm_limit, monthly_budget_usd, min_quality_score, revoked, created_at
		FROM virtual_keys WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VirtualKey
	for rows.Next() {
		var vk VirtualKey
		if err := rows.Scan(
			&vk.ID, &vk.OrgID, &vk.Name, &vk.KeyPrefix, &vk.KeyLast4, &vk.AllowedModels,
			&vk.RPMLimit, &vk.MonthlyBudget, &vk.MinQuality, &vk.Revoked, &vk.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, vk)
	}
	return out, rows.Err()
}

// RevokeVirtualKey marks a key revoked.
func (s *Store) RevokeVirtualKey(ctx context.Context, orgID, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE virtual_keys SET revoked = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.audit(ctx, orgID, "vkey.revoke", id, "")
	return nil
}

// --- Provider credentials ---

// CreateCredential encrypts and stores an upstream provider secret.
func (s *Store) CreateCredential(ctx context.Context, orgID, provider, name, baseURL, secret string) (ProviderCredential, error) {
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
		Provider:    provider,
		Name:        name,
		BaseURL:     baseURL,
		SecretLast4: Last4(secret),
		Enabled:     true,
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO provider_credentials
			(id, org_id, provider, name, base_url, secret_ciphertext, secret_last4, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE)`,
		cred.ID, cred.OrgID, cred.Provider, cred.Name, cred.BaseURL, ct, cred.SecretLast4)
	if err != nil {
		return ProviderCredential{}, err
	}
	s.audit(ctx, orgID, "credential.create", cred.ID, fmt.Sprintf("%s/%s", provider, name))
	return cred, nil
}

// ListCredentials returns credential metadata (no secrets) for an org.
func (s *Store) ListCredentials(ctx context.Context, orgID string) ([]ProviderCredential, error) {
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, provider, name, base_url, secret_last4, enabled, created_at
		FROM provider_credentials WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderCredential
	for rows.Next() {
		var c ProviderCredential
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Provider, &c.Name, &c.BaseURL, &c.SecretLast4, &c.Enabled, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DecryptedCredential is a credential with its plaintext secret, used internally
// to register providers at startup. Never serialized to API responses.
type DecryptedCredential struct {
	ProviderCredential
	Secret string
}

// LoadEnabledCredentials returns all enabled credentials with decrypted secrets.
func (s *Store) LoadEnabledCredentials(ctx context.Context, orgID string) ([]DecryptedCredential, error) {
	if s.cipher == nil {
		return nil, crypto.ErrNoMasterKey
	}
	if orgID == "" {
		orgID = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, provider, name, base_url, secret_ciphertext, secret_last4, enabled, created_at
		FROM provider_credentials WHERE org_id = $1 AND enabled = TRUE ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DecryptedCredential
	for rows.Next() {
		var c DecryptedCredential
		var ct []byte
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Provider, &c.Name, &c.BaseURL, &ct, &c.SecretLast4, &c.Enabled, &c.CreatedAt); err != nil {
			return nil, err
		}
		secret, err := s.cipher.Decrypt(ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt credential %s: %w", c.ID, err)
		}
		c.Secret = string(secret)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCredential removes a credential.
func (s *Store) DeleteCredential(ctx context.Context, orgID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.audit(ctx, orgID, "credential.delete", id, "")
	return nil
}

// audit writes a best-effort audit entry; failures are swallowed.
func (s *Store) audit(ctx context.Context, orgID, action, targetID, detail string) {
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO audit_log (org_id, action, target_id, detail)
		VALUES ($1,$2,$3,$4)`, orgID, action, targetID, detail)
}
