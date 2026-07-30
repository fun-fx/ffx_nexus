-- Durable home for the credentials pasted into the console's Plugin
-- Keys panel. Before this table the values lived in process memory
-- only, so every rolling update silently un-configured every plugin:
-- dispatch kept failing auth while the console still listed the
-- plugin as enabled.
--
-- Values are encrypted with the same AES-256-GCM master key that
-- protects provider_credentials, so a database dump carries no
-- plaintext vendor keys. `plugin` is the manifest's metadata.name
-- (the same token auth.secretRef resolves against) and `key_name` is
-- one entry of auth.keyRef, e.g. public_key / secret_key.
CREATE TABLE IF NOT EXISTS eval_plugin_keys (
    plugin            TEXT NOT NULL,
    key_name          TEXT NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin, key_name)
);
