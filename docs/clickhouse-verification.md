# ClickHouse verification — what we cover, what we don't

The Postgres control-plane path is **strongly verified**:
`internal/schemacontract/postgres_integration_test.go` extracts
every Postgres SQL literal in the Go source tree and hands each
one to a real Postgres as `PREPARE stmt AS <statement>`. Missing
tables surface as `42P01`, missing columns as `42703`; both before
a customer ever sees them. Total Postgres SQL coverage figure:
**99 of 104 statements are statically preparable**, the 5
remaining are covered by the smoke test detailed in
`docs/runtime-assembled-sql.md`.

The ClickHouse observability path is **partially verified**.
Three reasons explain the gap and outline the mitigations:

## Why PREPARE-equivalent does not work

ClickHouse's wire protocol has no `PREPARE`-without-execute. The
closest surface is `EXPLAIN AST` (parses and returns an AST)
or `DESCRIBE TABLE` (returns one row per column). Both still
require a live server connection — there is no in-process
analyser analogous to Postgres's parser we can borrow.

Without a static verifier, the canonical regression is a SQL
column name (`request_model`) that gets renamed or dropped in
a ClickHouse migration (`migrations/clickhouse/*`). The code
path keeps compiling, the unit tests pass against a mocked
`driver.Conn`, and the failure surfaces only at customer runtime
when the server returns a parse error.

## What we cover today

| Layer | File / Test                              | Coverage                                       |
|-------|------------------------------------------|------------------------------------------------|
| **A** | `clickhouse_smoke_test.go` (default CI)  | Convention + doc-drift guards only — does NOT exercise real ClickHouse. |
| **B** | `clickhouse_wire_test.go` (`-tags=clickhouse_smoke`) | Real server DESCRIBE + INSERT round-trip. Run on nightly / pre-release. |

Layer A catches the cheap regressions:

- **Convention drift**: every file in `clickhouseExceptions` MUST
  still hold SQL. A refactor that renames/wholesales a ClickHouse
  file without SQL in it produces a stale exception, and the
  Postgres contract stops covering it; this guard points at it.

- **Documentation drift**: `docs/runtime-assembled-sql.md` MUST
  mention each of the four `reader.go` lines whose incomplete
  literals cannot be statically prepared. A regression that moves
  the SQL but does not update the doc surfaces here.

Layer B closes the door on schema drift:

- **DESCRIBE for every write-table**: `gateway_traces` and
  `eval_scores` are looked at against the live server. Any
  column the code expects but the live schema dropped surfaces
  at test time.

- **INSERT round-trip**: a single row is written via the same
  `PrepareBatch` path the production code uses, then
  `SELECT count()` confirms it landed. A schema parse error
  fails the test before any customer request can hit the same
  bug.

## What we still don't cover

The list below is the documented residual risk; it is the price
of the gap above and would close only with one of:

1. A wire-stable PREPARE on the ClickHouse protocol side (we
   don't ship the server).
2. A `golang-migrate`-style ClickHouse schema registration that
   would let us inspect the in-server `system.columns` without a
   live connection (no such library today).
3. A custom Postgres-types ClickHouse-language extension that the
   codebase uses (large investment; not on the roadmap).

**Open customer-impact paths we cannot catch in CI today:**

- A column type drift — server returns `String` where the code
  expects `LowCardinality(String)`. Both compile; both look fine
  on the wire; only direct customer traffic reveals the mismatch.
- A `?` placeholder type inference error — code follows the
  server-side `INTERVAL ? SECOND` pattern with a Go-side `int`
  that the driver coerces. A future driver upgrade could change
  that coercion.

## Mitigation in production

- **Per-deploy smoke**: the `/readyz` gate runs a `SELECT 1` on
  both Postgres and ClickHouse; a broken ClickHouse connection
  surfaces at startup before any traffic.
- **Metric**: `ch_write_failed{action=...}` increments on every
  ClickHouse INSERT failure. A spike above zero on a routine
  metric scraper indicates the regression has reached production.
- **Tracing**: trace ingest has a best-effort classification; a
  ClickHouse write failure logs at error level and continues
  serving the gateway response (see fail-stop policy
  `docs/audit-failstop-policy.md`).

The combination is intentionally NOT fail-stop: a single
ClickHouse outage taking down the LLM gateway would be worse
than missing a few traces. Operators see the metric; the
audit_log, the Postgres control plane, and the LLM path keep
working.

## Running layer B locally

```bash
docker run -d --name contract-ch -p 9000:9000 \
    -e CLICKHOUSE_DB=nexus \
    -e CLICKHOUSE_USER=default \
    -e CLICKHOUSE_PASSWORD= -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
    clickhouse/clickhouse-server:24.3

# Apply every migration, then run the test.
NEXUS_TEST_CLICKHOUSE_URL='clickhouse://default:@127.0.0.1:9000/nexus' \
  go test -count=1 -tags=clickhouse_smoke ./internal/schemacontract -v
```

The test reports DESCRIBE for `gateway_traces` and `eval_scores`,
runs the INSERT round-trip on `gateway_traces`, and exits 0 on
a healthy install. Exit 1 indicates a column drift that the
contract caught.
