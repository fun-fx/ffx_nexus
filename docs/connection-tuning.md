# Connection tuning — how to size `NEXUS_POSTGRES_MAX_CONNS`

## Why this knob is exposed

`internal/core/store.go` enforces a minimum `pgxpool.MaxConns` of 8
per replica. The floor exists because the c0.x #A-3 deadlock ceiling
(where five concurrent invite accepts would each grab the outer
transaction's connection plus a second one for `s.Audit`) is
observed when the pool is sized smaller. Below 8 the audit-fail
safe path quietly deadlocks, surfacing as 8-minute test timeouts on
the `invite_integration_test.go` suite and Hermes outage in
production for tokens that exceed pool capacity.

The Helm chart wires this knob through `dependencies.postgres.maxConns`,
floored at `minSafeMaxConns` (default 8) and capped at
`maxSafeMaxConns` (default 64). A pre-install validation Job
(`deploy/helm/nexus/templates/pre-install-validation.yaml`) and the
`values.schema.json` boundary reject out-of-range values before the
main Deployment even renders.

## Pick a value

The chart's preview checklist for a 4-replica Gateway deployment
sharing a Postgres with `max_connections=200`:

| per-replica MaxConns | total (replicas × value) | headroom for migrations / BI |
|----------------------|--------------------------|------------------------------|
|  8 (default, floor)  |   32 / 200              |  168 left                   |
| 16                   |   64 / 200              |  136 left                   |
| 32                   |  128 / 200              |   72 left                   |
| 48                   |  192 / 200              |    8 left (Dangerous)       |
| 64 (default, ceiling) |  256 / 200              | **REJECTED** (over-ceiling) |

Subscribe `$maxConns * replicaCount < Postgres max_connections * 0.7`
as the operational rule of thumb so the migrations + replica probes
+ BI reader still have way to come in.

## Phase D-1 split (Gateway / Worker)

When Gateway and Worker are split into separate Helm releases, G and W
will need pools with different sizing on the same Postgres. Phase D-1
will expose `dependencies.postgres.workerMaxConns` and
`dependencies.postgres.gatewayMaxConns` separately, and
`NewWorker`-side code will read `NEXUS_POSTGRES_WORKER_MAX_CONNS`
instead of the unified value. Until Phase D-1 ships, both share
`NEXUS_POSTGRES_MAX_CONNS`.

## Phase D-1 sizing profiles

Phase D-1 sizes Gateway and Worker pools separately because the two
roles scale on different axes. The pre-install validation Job
(`pre-install-validation.yaml`) sums Gateway and Worker pools plus a
fixed allowance for migrations, CLI, BI readers, and `(WANT *
repCount) + (WWANT * wRepCount) + 1(migration) + 4(CLI) + 3(superuser
reserved) + 8(BI) + 12(safety headroom)` and rejects values above 200.

| Profile         | Gateway replicas | Gateway maxConns | Worker replicas | Worker maxConns | SUM (with overhead) | Intended for |
|-----------------|------------------|------------------|-----------------|------------------|---------------------|--------------|
| Small / single  | 1                | 8                | 1               | 8                | 36                  | local dev / single-container install / on-call verification cluster |
| Medium / 3-replica | 3              | 8                | 3               | 8                | 76                  | pilot customers / one team, 1 production region |
| Large / HA cluster | 5             | 16               | 3               | 8                | 142                 | production multi-region / 100+ tenants |
| Maximum / ceiling | 5               | 24               | 3               | 16               | 178                 | the documented ceiling; reach this profile only after instrumenting pg_stat_activity to confirm no client is starved |

Notes:

- The SUM column assumes Postgres `max_connections=200`. Operators with
  `max_connections=300` and the documented safety ratio of `0.7`
  (-ish) can push Gateway replicas to 8 or Worker maxConns to 16 in
  the same profile. The 200 ceiling is a fixed product spec, not a
  tuning surface; raising it requires an explicit Postgres
  `ALTER SYSTEM SET max_connections = 300;` plus a server restart.
- The Worker pool's *minimum* of 8 is not arbitrary: lease
  acquisition grabs a connection off the same pool that the cron
  runner uses, and the lease-pinned connection (the one that holds
  the role's advisory lock for the lifetime of the pod) does not
  return until the pod terminates. Below 8, a concurrent heartbeat
  + tick + cron write would starve. See `internal/leaser/leaser.go`
  and the connection-leak invariant test in `integration_test.go`.

## Operator overrides via `extraEnv`

`NEXUS_POSTGRES_MAX_CONNS` is set through the Secret template if the
Helm chart path produced a value; if a customer sets

```yaml
extraEnv:
  - name: NEXUS_POSTGRES_MAX_CONNS
    value: "16"
```

the env var in the extraEnv list will override the chart's
value, but only within the same floor/ceiling band. The Helm-side
validation reads `dependencies.postgres.maxConns`, so setting
`extraEnv` directly bypasses the chart's pre-install guard. The
recommended path is to set the knob through
`dependencies.postgres.maxConns` and let the chart pick.

## References

- `internal/core/store.go` — `NewStore` enforces the floor.
- `docs/audit-failstop-policy.md` — the audit-fail safe path
  explains why a connection per audit-fail-stop transaction is held.
- `migrations/postgres/022_audit_burst_org.sql` — the burst unique
  index that a single row-level lock can contend on under attack,
  the very class of `MaxConns` short-out that motivated the floor.
