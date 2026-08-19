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
