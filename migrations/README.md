# Schema migrations

Migrations are **discovered**, not listed. Any `.sql` file in
`migrations/postgres/` or `migrations/clickhouse/` is applied, in numeric order,
exactly once, and recorded in a `schema_migrations` ledger.

This replaced a hardcoded `[]string` of paths inside `main()`. That list drifted
from the files on disk three separate times, and each time the symptom was
silent:

| Defect | Symptom in production |
| --- | --- |
| `009`–`011` never added to the list | `eval_plugins` never existed; the runtime fell back to an in-memory plugin store, so every rolling update uninstalled every console-installed plugin |
| `014_invite_tokens.sql` never added | `POST /api/invites` returned 500 on every fresh install |
| ClickHouse `benchmark_runs` given a duplicate `007_` ordinal | benchmark history was never persisted |
| `013` listed **before** `012` | `013`'s `ALTER TABLE benchmark_runs` ran before `012` created the table; the error was logged and discarded, so `benchmark_runs.schedule_id` was never created anywhere |

Discovery makes the first three structurally impossible and ordinal sorting
makes the fourth impossible.

## Naming rules

```
NNN_short_description.sql
```

1. **`NNN` is a zero-padded integer** and determines execution order.
2. **Ordinals must be unique per engine.** A duplicate is a hard error at load
   time, not a coin flip. Postgres and ClickHouse are numbered independently.
3. **Take the next free number.** If two branches both claim `015`, whichever
   merges second renumbers. `go test ./internal/migrate/` fails on the
   collision, so this cannot reach `main` unnoticed.
4. **Never edit an applied migration.** The ledger stores each file's SHA-256;
   a changed file aborts the next migration run with `ErrChecksumMismatch`
   rather than silently producing two different schemas from one version
   string. Add a new migration instead.

## Every statement must be replay-safe

Use `IF NOT EXISTS` / `IF EXISTS` on every `CREATE`, `DROP`, `ADD COLUMN` and
`ADD INDEX`. `TestMigrationsAreIdempotent` enforces this.

This is not stylistic. Two mechanisms depend on it:

- **ClickHouse has no transactions and no advisory lock.** Replay-safety is the
  *only* thing making concurrent or retried ClickHouse migrations correct.
- **Adopting an existing database** replays every migration against a database
  that already has most of them. That is how a pre-ledger deployment is brought
  under management without reinitialising anything — and it is how the
  previously-skipped migrations above finally get applied.

## Postgres statements to avoid

`pgExecutor.Apply` wraps each migration and its ledger row in one transaction,
so either both land or neither does. Postgres forbids a few statements inside a
transaction; `TestPostgresMigrationsAreTransactionSafe` rejects them:

`CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `REINDEX CONCURRENTLY`,
`VACUUM`, `CREATE DATABASE`, `DROP DATABASE`, `ALTER SYSTEM`,
`CREATE TABLESPACE`.

If one is genuinely needed, the migration engine needs an explicit
non-transactional escape hatch — do not work around it by disabling the test.

## No down migrations

There are none, deliberately. Automatic DDL reversal is how production data gets
deleted during an incident.

Design changes as **expand/contract** instead: add the new column/table, migrate
reads and writes, and only remove the old shape in a later release once no
running version references it. Version N of the binary therefore runs correctly
against the N+1 schema, which is what makes application rollback safe on its
own. See `docs/customer-self-hosted-upgrade-rollback.md`.

## Applying them

```bash
# Apply everything outstanding (this is what the Helm hook Job runs)
nexus migrate

# What would change? Exits 2 if anything is outstanding, 0 if current.
nexus migrate --check

# One engine only
nexus migrate --engine=postgres
```

Connection details come from `NEXUS_POSTGRES_URL` and `NEXUS_CLICKHOUSE_URL`.
An engine whose URL is empty is skipped.

In Kubernetes the chart runs `nexus migrate` as a `pre-install,pre-upgrade` hook
Job, so a failed migration aborts the release. Application pods verify the
ledger at boot and report `NotReady` on `/readyz` if the schema is behind, rather
than serving traffic against a schema they do not match. Set
`NEXUS_AUTO_MIGRATE=true` for local development only.

## Testing against a real database

Unit tests need nothing. The integration tests need a throwaway Postgres:

```bash
NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
  go test ./internal/migrate/ -run Integration -v
```

They create and drop an isolated schema per test, and cover a fresh install,
re-running, five concurrent migrators, adopting a pre-ledger database without
data loss, checksum drift, and a failed migration not being recorded as applied.
