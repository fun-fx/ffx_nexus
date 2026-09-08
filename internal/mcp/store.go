package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrServerNotFound is returned when an MCP server row is missing.
var ErrServerNotFound = errors.New("mcp: server not found")

// PostgresStore persists MCP server registry rows.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a store backed by mcp_servers.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	if pool == nil {
		return nil
	}
	return &PostgresStore{pool: pool}
}

const serverColumns = `id, org_id, name, spec_yaml, enabled, created_at, updated_at`

// List returns servers visible to orgID (cluster-wide + org-specific).
func (s *PostgresStore) List(ctx context.Context, orgID string) ([]ServerRecord, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("mcp: postgres store not configured")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+serverColumns+`
		FROM mcp_servers
		WHERE $1 = '' OR org_id = '' OR org_id = $1
		ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerRecord
	for rows.Next() {
		rec, err := scanServerRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Get loads one server by id.
func (s *PostgresStore) Get(ctx context.Context, id string) (*ServerRecord, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("mcp: postgres store not configured")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+serverColumns+` FROM mcp_servers WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrServerNotFound
	}
	rec, err := scanServerRecord(rows)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// Save upserts a server row and stamps timestamps back onto r.
func (s *PostgresStore) Save(ctx context.Context, r *ServerRecord) error {
	if s == nil || s.pool == nil {
		return errors.New("mcp: postgres store not configured")
	}
	if r == nil {
		return errors.New("mcp: nil server record")
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("mcp: name is required")
	}
	if strings.TrimSpace(r.SpecYAML) == "" {
		return errors.New("mcp: spec_yaml is required")
	}
	if _, err := DecodeSpec([]byte(r.SpecYAML)); err != nil {
		return fmt.Errorf("re-validate spec_yaml: %w", err)
	}
	if strings.TrimSpace(r.ID) == "" {
		var existing string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM mcp_servers WHERE org_id = $1 AND name = $2`,
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
		INSERT INTO mcp_servers (id, org_id, name, spec_yaml, enabled)
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

// Delete removes a server row.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	if s == nil || s.pool == nil {
		return errors.New("mcp: postgres store not configured")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1`, id)
	return err
}

func scanServerRecord(rows pgx.Rows) (ServerRecord, error) {
	var rec ServerRecord
	err := rows.Scan(&rec.ID, &rec.OrgID, &rec.Name, &rec.SpecYAML,
		&rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt)
	return rec, err
}
