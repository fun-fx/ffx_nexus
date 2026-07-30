package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ffxnexus/nexus/internal/core/crypto"
)

// Eval-plugin credentials. The console's Plugin Keys panel writes the
// vendor keys an EvalPlugin manifest references through auth.keyRef;
// dispatch and collect read them back on every process start.
//
// These live in their own table rather than provider_credentials
// because they are not routable upstreams: they carry no model list,
// no base URL, and must never appear on the credentials page or in
// BYOK precedence. Encryption reuses the store's master-key cipher so
// the two secret surfaces have identical at-rest guarantees.

// SaveEvalPluginKeys replaces the full key set stored for one plugin.
// Passing an empty map deletes every key, which is what the panel's
// Clear button means. The write is transactional so a partially
// applied rotation can never leave one half of a key pair behind.
func (s *Store) SaveEvalPluginKeys(ctx context.Context, plugin string, kv map[string]string) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if s.cipher == nil {
		return crypto.ErrNoMasterKey
	}
	if plugin == "" {
		return errors.New("core: plugin name is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM eval_plugin_keys WHERE plugin = $1`, plugin); err != nil {
		return err
	}
	for name, value := range kv {
		if name == "" || value == "" {
			// Empty values count as "not configured" everywhere else;
			// persisting them would make Has() lie after a restart.
			continue
		}
		ct, err := s.cipher.Encrypt([]byte(value))
		if err != nil {
			return fmt.Errorf("encrypt plugin key %s/%s: %w", plugin, name, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO eval_plugin_keys (plugin, key_name, secret_ciphertext)
			VALUES ($1, $2, $3)`, plugin, name, ct); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LoadEvalPluginKeys returns the decrypted key set for one plugin. A
// plugin with no stored keys yields an empty map and a nil error so
// callers can distinguish "nothing configured" from "lookup failed".
func (s *Store) LoadEvalPluginKeys(ctx context.Context, plugin string) (map[string]string, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	if s.cipher == nil {
		return nil, crypto.ErrNoMasterKey
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key_name, secret_ciphertext FROM eval_plugin_keys WHERE plugin = $1`, plugin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var name string
		var ct []byte
		if err := rows.Scan(&name, &ct); err != nil {
			return nil, err
		}
		plain, err := s.cipher.Decrypt(ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt plugin key %s/%s: %w", plugin, name, err)
		}
		out[name] = string(plain)
	}
	return out, rows.Err()
}

// DeleteEvalPluginKeys removes every key stored for a plugin.
func (s *Store) DeleteEvalPluginKeys(ctx context.Context, plugin string) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM eval_plugin_keys WHERE plugin = $1`, plugin)
	return err
}

// ListEvalPluginKeyOwners returns the plugin names that have at least
// one key stored. Boot uses it to warm the resolver cache so the first
// trace after a restart does not pay a database round-trip.
func (s *Store) ListEvalPluginKeyOwners(ctx context.Context) ([]string, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT plugin FROM eval_plugin_keys ORDER BY plugin`)
	if err != nil {
		return nil, err
	}
	return collectStrings(rows)
}

func collectStrings(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
