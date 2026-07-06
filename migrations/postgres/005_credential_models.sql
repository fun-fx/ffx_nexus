-- User-defined credentials may include a custom model inventory when the
-- provider speaks the OpenAI wire format at a non-default base URL. Storing
-- the model ids per-credential lets the gateway expose them at /v1/models
-- without round-tripping to the upstream at startup. The column is JSONB so
-- we keep room for capability tags (chat / embed / moderation / image) as
-- the dynamic compat provider grows.
--
-- Shape: {"chat": ["gpt-x"], "embed": [...]}  — absent keys mean the
-- owner is not advertising that capability.
--
-- Backward compatible: existing rows stay valid because the default satisfies
-- every credential that did not declare a list (semantics: "use provider
-- defaults", which behaves the same as today).
ALTER TABLE provider_credentials
    ADD COLUMN IF NOT EXISTS models JSONB NOT NULL DEFAULT '{}'::jsonb;
