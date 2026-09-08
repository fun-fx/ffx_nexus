-- MCP server registry: operator-declared tool servers the gateway proxies.
-- spec_yaml holds connection details (stdio/http), auth headers, and
-- optional virtual-key allow lists. Re-validated on read like eval_plugins.

CREATE TABLE IF NOT EXISTS mcp_servers (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL,
    spec_yaml      TEXT NOT NULL,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT mcp_servers_unique_name UNIQUE (org_id, name)
);

CREATE INDEX IF NOT EXISTS idx_mcp_servers_org ON mcp_servers (org_id);
