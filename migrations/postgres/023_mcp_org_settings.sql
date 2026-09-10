-- Per-org MCP defaults applied when installing from the Library or creating servers.

CREATE TABLE IF NOT EXISTS mcp_org_settings (
    org_id              TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    default_timeout_ms  INT NOT NULL DEFAULT 60000,
    default_sticky_http BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
