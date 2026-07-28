-- Eval plugins: user/declarative adapters that wrap an external
-- evaluation service (LangSmith, Langfuse, Datadog, Braintrust,
-- Arize, OTel, webhook, …) into a Nexus `external` evaluator kind.
--
-- Plugins are stored as the raw YAML so a future schema bump can be
-- shipped without a destructive migration: we re-validate on read.
-- The `enabled` column is the admin override (Helm-installed plugins
-- can be toggled without re-templating). `org_id` is empty for
-- cluster-wide plugin entries; populated for per-org customizations.
CREATE TABLE IF NOT EXISTS eval_plugins (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL,
    spec_yaml      TEXT NOT NULL,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT eval_plugins_unique_name UNIQUE (org_id, name)
);

CREATE INDEX IF NOT EXISTS idx_eval_plugins_org ON eval_plugins (org_id);

-- Lightweight secret reference table. We don't store plaintext keys;
-- the row points at either a K8s Secret mounted into the gateway
-- pod or at the existing in-cluster eval_credentials store.
CREATE TABLE IF NOT EXISTS eval_plugin_secrets (
    plugin_id      TEXT NOT NULL REFERENCES eval_plugins(id) ON DELETE CASCADE,
    secret_kind    TEXT NOT NULL,                       -- 'k8s_secret' | 'keyref'
    secret_ref     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (plugin_id, secret_kind)
);
