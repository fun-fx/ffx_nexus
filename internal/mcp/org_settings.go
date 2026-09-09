package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultTimeoutMs = 60_000

// OrgSettings holds org-level MCP install defaults.
type OrgSettings struct {
	OrgID             string    `json:"org_id"`
	DefaultTimeoutMs  int       `json:"default_timeout_ms"`
	DefaultStickyHTTP bool      `json:"default_sticky_http"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// OrgSettingsStore reads and writes mcp_org_settings rows.
type OrgSettingsStore interface {
	Get(ctx context.Context, orgID string) (OrgSettings, error)
	Upsert(ctx context.Context, orgID string, timeoutMs *int, sticky *bool) (OrgSettings, error)
}

// PostgresOrgSettingsStore persists org MCP preferences.
type PostgresOrgSettingsStore struct {
	pool *pgxpool.Pool
}

// NewPostgresOrgSettingsStore returns a store backed by mcp_org_settings.
func NewPostgresOrgSettingsStore(pool *pgxpool.Pool) *PostgresOrgSettingsStore {
	if pool == nil {
		return nil
	}
	return &PostgresOrgSettingsStore{pool: pool}
}

func defaultOrgSettings(orgID string) OrgSettings {
	return OrgSettings{
		OrgID:             orgID,
		DefaultTimeoutMs:  defaultTimeoutMs,
		DefaultStickyHTTP: true,
	}
}

// Get returns stored settings or defaults when no row exists.
func (s *PostgresOrgSettingsStore) Get(ctx context.Context, orgID string) (OrgSettings, error) {
	if s == nil || s.pool == nil {
		return OrgSettings{}, errors.New("mcp: org settings store not configured")
	}
	var out OrgSettings
	err := s.pool.QueryRow(ctx, `
		SELECT org_id, default_timeout_ms, default_sticky_http, updated_at
		FROM mcp_org_settings WHERE org_id = $1`, orgID).
		Scan(&out.OrgID, &out.DefaultTimeoutMs, &out.DefaultStickyHTTP, &out.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return defaultOrgSettings(orgID), nil
		}
		return OrgSettings{}, err
	}
	return out, nil
}

// Upsert creates or updates org MCP defaults.
func (s *PostgresOrgSettingsStore) Upsert(
	ctx context.Context,
	orgID string,
	timeoutMs *int,
	sticky *bool,
) (OrgSettings, error) {
	if s == nil || s.pool == nil {
		return OrgSettings{}, errors.New("mcp: org settings store not configured")
	}
	cur, err := s.Get(ctx, orgID)
	if err != nil {
		return OrgSettings{}, err
	}
	if timeoutMs != nil {
		cur.DefaultTimeoutMs = *timeoutMs
	}
	if sticky != nil {
		cur.DefaultStickyHTTP = *sticky
	}
	if cur.DefaultTimeoutMs < 1000 {
		return OrgSettings{}, errors.New("mcp: default_timeout_ms must be at least 1000")
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO mcp_org_settings (org_id, default_timeout_ms, default_sticky_http)
		VALUES ($1, $2, $3)
		ON CONFLICT (org_id) DO UPDATE SET
			default_timeout_ms  = EXCLUDED.default_timeout_ms,
			default_sticky_http = EXCLUDED.default_sticky_http,
			updated_at          = NOW()
		RETURNING org_id, default_timeout_ms, default_sticky_http, updated_at`,
		orgID, cur.DefaultTimeoutMs, cur.DefaultStickyHTTP).
		Scan(&cur.OrgID, &cur.DefaultTimeoutMs, &cur.DefaultStickyHTTP, &cur.UpdatedAt)
	return cur, err
}
