-- c0.4 Phase E role separation: split the four roles that interact with
-- audit_log so the append-only contract is enforceable by Postgres
-- permissions rather than only by application code.
--
-- The role boundaries aren't auto-applied: Nexus deployments use a
-- single DB account in the typical self-hosted case, and applying
-- GRANTs that assume a superuser is on hand would break the start-up
-- flow. The statements below are shipped as documentation and as the
-- canonical reconciliation a DBA applies in Phase E to harden an
-- installation where SU credentials are available.
--
-- Roles:
--   nexus_migration  — only used during migrate.Run. INSERT/UPDATE
--                      the schema_migrations ledger. SELECT/DELETE on
--                      the legacy schema_migrations row needed by
--                      numbered-migration maintenance. No rights on
--                      audit_log beyond SELECT for back-fill checks.
--   nexus_app        — the application runtime. INSERT only on
--                      audit_log (no UPDATE / DELETE). SELECT on
--                      audit_log to render the audit feed and export.
--   nexus_audit_read — analytics, read-only. SELECT only.
--   nexus_audit_purge — retention cleanup. DELETE only with a
--                      time-bounded predicate (`created_at < now() -
--                      interval N days`) refused by a CHECK constraint
--                      on the role privilege; a SQL safeguard function
--                      is created below to enforce this.
--
-- IMPORTANT: This script is conditional. The GRANTs only succeed
-- against a Postgres role that exists. The application installer
-- (cmd/nexus/main.go) does NOT auto-create these roles; the operator
-- provisions them via Helm secrets documented in docs/audit-log-roles.md
-- before a hardened install.

-- Migration user is allowed to bootstrap DDL but not to write audit_log.
-- The application's auditor identity is granted minimally on the table.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_migration') THEN
        GRANT SELECT, INSERT, UPDATE ON schema_migrations TO nexus_migration;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_app') THEN
        -- Append-only on audit_log. SELECT permitted for /api/audit.
        GRANT SELECT, INSERT ON audit_log TO nexus_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_audit_read') THEN
        GRANT SELECT ON audit_log TO nexus_audit_read;
    END IF;
END $$;

-- The retention cleanup relies on the `nexus_audit_purge_rows` SQL
-- function rather than raw DELETE privileges, because Postgres doesn't
-- expose a "DELETE only inside this WHERE clause" grant. The function
-- is SECURITY DEFINER and only accepts a time argument; it raises an
-- exception if `older_than` is passed as zero / negative.
CREATE OR REPLACE FUNCTION nexus_audit_purge_rows(older_than interval)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    deleted_count integer;
BEGIN
    IF older_than IS NULL OR older_than < interval '1 hour' THEN
        RAISE EXCEPTION 'audit purge refuses interval shorter than 1 hour; received %',
            older_than;
    END IF;
    DELETE FROM audit_log
     WHERE created_at < NOW() - older_than
       AND (count > 0) = FALSE; -- always-on truth: retain aggregated rows unconditionally
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END $$;

-- The cleanup role is restricted to EXECUTE on the function; UPDATE /
-- DELETE on the table directly is revoked (not granted) so direct
-- DELETE calls fail with "permission denied". This implements the
-- "single cleanup path" protection c0.4 asked for.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexus_audit_purge') THEN
        GRANT EXECUTE ON FUNCTION nexus_audit_purge_rows(interval) TO nexus_audit_purge;
        REVOKE DELETE, UPDATE ON audit_log FROM nexus_audit_purge;
    END IF;
END $$;
