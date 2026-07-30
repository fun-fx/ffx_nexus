package evalplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the durable PluginStore. It replaces MemoryStore in
// any deployment that has a control-plane database.
//
// This matters more than the usual "memory vs. durable" trade-off: a
// plugin row is the only record that an evaluation vendor is wired up
// at all, so keeping it in process memory meant every rolling update
// silently uninstalled every console-installed plugin while the
// operator's dashboard kept showing it as enabled.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a store backed by the eval_plugins table.
// A nil pool yields a nil store so callers can fall back to
// MemoryStore with a plain nil check.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	if pool == nil {
		return nil
	}
	return &PostgresStore{pool: pool}
}

const pluginColumns = `id, org_id, name, spec_yaml, enabled, created_at, updated_at`

// List mirrors MemoryStore.List: an empty orgID means "every row in the
// deployment" (what boot uses to hydrate the registry), while a
// non-empty orgID returns cluster-wide rows plus that org's own.
func (s *PostgresStore) List(ctx context.Context, orgID string) ([]PluginRecord, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("evalplugin: postgres store not configured")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+pluginColumns+`
		FROM eval_plugins
		WHERE $1 = '' OR org_id = '' OR org_id = $1
		ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PluginRecord
	for rows.Next() {
		rec, err := scanPluginRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, id string) (*PluginRecord, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("evalplugin: postgres store not configured")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+pluginColumns+` FROM eval_plugins WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrPluginNotFound
	}
	rec, err := scanPluginRecord(rows)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// Save upserts the row and stamps the assigned id, CreatedAt and
// UpdatedAt back onto r.
//
// A record with no id that collides with an existing (org_id, name)
// adopts that row instead of failing the unique constraint: the
// console's "install" and "edit" paths are the same manifest submit,
// and an operator re-pasting a manifest means "make this the config",
// not "reject me because a row already exists".
func (s *PostgresStore) Save(ctx context.Context, r *PluginRecord) error {
	if s == nil || s.pool == nil {
		return errors.New("evalplugin: postgres store not configured")
	}
	if r == nil {
		return errors.New("evalplugin: nil plugin record")
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("plugin name is required")
	}
	if strings.TrimSpace(r.SpecYAML) == "" {
		return errors.New("plugin spec_yaml is required")
	}
	if _, err := Decode([]byte(r.SpecYAML)); err != nil {
		return fmt.Errorf("re-validate spec_yaml: %w", err)
	}

	if strings.TrimSpace(r.ID) == "" {
		var existing string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM eval_plugins WHERE org_id = $1 AND name = $2`,
			r.OrgID, r.Name).Scan(&existing)
		switch {
		case err == nil:
			r.ID = existing
		case errors.Is(err, pgx.ErrNoRows):
			r.ID = uuid.NewString()
		default:
			return err
		}
	}

	return s.pool.QueryRow(ctx, `
		INSERT INTO eval_plugins (id, org_id, name, spec_yaml, enabled)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			org_id     = EXCLUDED.org_id,
			name       = EXCLUDED.name,
			spec_yaml  = EXCLUDED.spec_yaml,
			enabled    = EXCLUDED.enabled,
			updated_at = now()
		RETURNING created_at, updated_at`,
		r.ID, r.OrgID, r.Name, r.SpecYAML, r.Enabled,
	).Scan(&r.CreatedAt, &r.UpdatedAt)
}

func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	if s == nil || s.pool == nil {
		return errors.New("evalplugin: postgres store not configured")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM eval_plugins WHERE id = $1`, id)
	return err
}

func scanPluginRecord(rows pgx.Rows) (PluginRecord, error) {
	var rec PluginRecord
	err := rows.Scan(&rec.ID, &rec.OrgID, &rec.Name, &rec.SpecYAML,
		&rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt)
	return rec, err
}
