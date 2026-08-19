# Audit Log Role Separation (c0.4 / Phase E)

The audit log is **append-only**. This page describes the four Postgres
roles that interact with it and the contract each role fulfils. The
goal is for the append-only property to be enforced by the database
permission system in addition to the application code.

## Roles

### `nexus_migration`
Used only during `migrate.Run`. Privileges:

- `SELECT, INSERT, UPDATE` on `schema_migrations`.
- `SELECT` on `audit_log` (for the optional adoption checks done at
  boot).
- No `INSERT` / `UPDATE` / `DELETE` on `audit_log`. A migration that
  writes audit rows directly is refused.

### `nexus_app`
This is the runtime application connection string Nexus uses for all
control-plane operations. Privileges:

- `SELECT, INSERT` on `audit_log`.
- **No `UPDATE` / `DELETE` on `audit_log`** — the append-only property
  is enforceable at the database layer.
- `SELECT, INSERT, UPDATE, DELETE` on every non-audit table the
  application owns.

### `nexus_audit_read`
A read-only analytics / BI role. Privileges:

- `SELECT` only on `audit_log`.

### `nexus_audit_purge`
The retention cleanup role. Used exclusively by the scheduled
retention worker. Privileges:

- `EXECUTE` on the `nexus_audit_purge_rows(interval)` function only.
- **No direct DELETE / UPDATE on `audit_log`**.
- The `nexus_audit_purge_rows` function itself rejects intervals
  shorter than 1 hour and refuses to delete aggregated rows
  (`count > 0` is preserved).

The function is `SECURITY DEFINER` so the role can call it without
holding the underlying DELETE grant; the WHERE clause embeds the
required `created_at < NOW() - older_than` predicate so even a
compromised `nexus_audit_purge_rows` user cannot delete a recent
row.

## Helm configuration

A hardened install provides four secret values:

- `audit.appDsn` — connection string for `nexus_app`.
- `audit.readOnlyDsn` — connection string for `nexus_audit_read`.
  Optional; if absent the application uses the app DSN.
- `audit.purgeDsn` — connection string for `nexus_audit_purge`.
  Optional; if absent the retention worker is **disabled** (the cron
  job exits with `disabled` reason) — an operator who doesn't want
  silent retention can omit the secret.
- `audit.migrationDsn` — connection string for `nexus_migration`.
  Used by `cmd/nexus migrate` only.

In a single-DB-account deployment (the default self-hosted install),
all four roles collapse into one (`nexus_app`). The append-only
property is then enforced only by the application code's inventory
test (`TestAuditAppendOnlyEnforcedInAppCode`). Operators who want the
hardening should refer to Phase E's `audit.hardening.enabled: true`
Helm value, which causes the boot process to verify the four roles
exist and to refuse to start if `audit_log` is writable by `nexus_app`
in a way that violates the contract (e.g. direct DELETE permitted).

## Single-DB-account fallback and weakened protections

When the operator only supplies one DSN, the boot process keeps the
running application and the audit_log reader on the same identity.
The following protections are **weakened**:

- The application identity can in principle issue `DELETE FROM
  audit_log` if a future code path is buggy enough to construct it.
  Mitigation: the inventory test refuses new code paths that call
  UPDATE / DELETE on audit_log; this shifts the property from
  database enforcement to code-review enforcement.
- The audit-read role is identical to the app role, so a future
  compromise of the analytics role still grants production table
  access. Mitigation: keep analytics queries behind admin-only routes
  in the console.
- The audit-purge role is unavailable, so retention cleanup does not
  run. The audit_log therefore grows unbounded.

The Helm chart surfaces a warning banner in the operator console
when the install detects single-DSN mode.

## Audit-row deletion paths in the application code

`TestAuditAppendOnlyEnforcedInAppCode` (in
`internal/core/audit_append_only_test.go`) walks every Go file and
fails the build if any code path outside the configured retention
purge site does `UPDATE` or `DELETE` against `audit_log`. The single
allow-listed identifier is the const
`PurgeStaleAuditRows`-bearing function in `internal/core/audit.go`,
and even that path requires an interval >= 1 hour at the SQL layer.

Adding a new code path that mutates `audit_log` will require lifting
the `allowList` constant in the test by one and documenting the new
site.
