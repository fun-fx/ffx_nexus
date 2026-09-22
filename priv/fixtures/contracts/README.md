# Contract fixtures (Phase 0)

Language-neutral goldens for the Elixir rewrite. The Go binary in this
repository remains production. These files are the spec a second agent
should implement against.

Pin: [`PIN.json`](PIN.json). Elixir copies this tree at that SHA into
`priv/fixtures/contracts` (vendored) or as a submodule.

## Layout

| Path | Contents |
| --- | --- |
| `schema/postgres.sql` | Concatenated `migrations/postgres/*.sql` (empty DB reconstruct) |
| `schema/clickhouse.sql` | Concatenated `migrations/clickhouse/*.sql` |
| `schema/ledger.json` | `schema_migrations` ids + SHA-256 of each SQL file |
| `schema/redis.md` | RPM/spend key layout (no Lua today) |
| `sse/*.sse` | OpenAI chat SSE entity bodies (raw bytes) |
| `sse/manifest.json` | SHA-256 of each `.sse` file |
| `errors/` | `/v1` vs `/api` JSON + mid-stream SSE comments |
| `failover/first_byte.md` | First-byte failover rules |

## Normalization

Before hashing a **live** response, substitute:

- request ids → `req_golden`
- unix `created` timestamps → `1720000000`
- RFC3339 timestamps → `1970-01-01T00:00:00Z`

Fixture files are already normalized. Hash them as stored (including
final newline / CRLF). Do not pretty-print JSON.

## Commands

```bash
# Rebuild schema concat + ledger checksums (after adding migrations)
./scripts/dump_schema.sh

# Verify fixture SHA-256 matches sse/manifest.json (no server)
go test ./internal/contracts/

# Optional: black-box a running binary (Go or Elixir) against the goldens
NEXUS_BASE_URL=http://127.0.0.1:8080 go run ./scripts/contract_harness

# Load baseline (same command later against Elixir)
NEXUS_BASE_URL=http://127.0.0.1:8080 go run ./scripts/load_baseline
```
