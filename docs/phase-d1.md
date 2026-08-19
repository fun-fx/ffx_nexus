# Phase D-1: Gateway / Worker Split

Phase D-1 moves the benchmark scheduler and the durable Postgres
lease heartbeat onto a separate Worker Deployment. The Gateway
Deployment now serves only the OpenAI-compatible request pathway
and the Console API; the Worker Deployment runs the cron loop and
holds the single-leader lease for `benchmark_scheduler`.

## Topology

The Helm chart renders two Deployments backed by the same Nexus
container image. Each Deployment has its own component label so
the Service selectors can route traffic to the right pods:

| Deployment            | Component label | Ports served              | Replicas |
| --------------------- | --------------- | ------------------------- | -------- |
| `<release>-gateway`   | `gateway`       | gateway, console, metrics | 1-N      |
| `<release>-worker`    | `worker`        | metrics (internal only)   | 1-N      |

The two Deployments share the same Postgres pool configuration
(URL, TLS) but split the connection budget independently:

| Pool                      | Floor (minSafeMaxConns) | Ceiling (maxSafeMaxConns) | Default |
| ------------------------- | ----------------------- | ------------------------- | ------- |
| Gateway (`dependencies.postgres`) | 8 | 64 | 16 |
| Worker (`worker.postgres`)         | 8 | 32 | 16 |

The Helm `pre-install-validation` Job enforces both ranges at
install / upgrade time. Setting either block below the floor or
above the ceiling fails the install with a precise pointer to the
offending knob.

## Why a separate Deployment

Three interacting problems coalesced before Phase D-1:

1. **Deadlock under load.** The shared pgxpool serving both the
   chat completion hot path and the cron renew loop could drain
   to zero connections mid-tick. Once the renew loop took the
   last connection, an incoming chat completion became a
   goroutine parked on `pool.Acquire`. The earlier
   `NEXUS_POSTGRES_MAX_CONNS` floor=8 fix surfaced this
   invariant; the Phase D-1 split makes it impossible for cron
   renewals to contend with request traffic because they live
   on independent pools.
2. **Single-leader semantics.** The cron tick is
   idempotent (UPDATE next_launch_at), so replicating it across
   pods was previously tolerated. Operators that wanted a
   strict single leader had no lease primitive to lean on;
   custom schedulers wrote their own PG advisory lock
   fragments. Phase D-1 introduces a shipping-grade lease
   layer (`internal/leaser`) so any future scheduler can lift
   the same primitive without inventing it.
3. **Connection sizing.** A separate pool lets each Deployment
   be sized for its workload instead of the worst-case sum. A
   Gateway hot path wants `MaxConns=16` to handle provider
   fan-out; a Worker that ticks once per 30 s and renews one
   row every 7 s is happy with `MaxConns=8`.

## Single-leader lease protocol

The lease protocol lives in `internal/leaser`. Every Worker pod
calls `Manager.Acquire(role, ownerID)` at boot, where
`ownerID = "<hostname>-<pid>"`. The first pod to take the row
upserts the lease with:

- `acquired_at = NOW()`
- `expires_at = NOW() + 15s`
- `lock_token = 16 random hex bytes`

A renew goroutine bumps `heartbeat_at` and `expires_at` every 7 s
using a `WHERE role = ? AND lock_token = ? AND expires_at > NOW()`
predicate. If two consecutive renews fail, the row is dropped
from the manager's active set and a new pod takes over.

The `pg_try_advisory_lock(hash(role))` call is a fast path to
prevent two goroutines on the same pod from racing. The durable
row is the single source of truth; advisory locks are advisory.

## Failover behaviour

| Scenario                                       | Lease behaviour | Audit behaviour                              |
| ---------------------------------------------- | --------------- | -------------------------------------------- |
| Worker pod pre-stop (SIGTERM, 30 s)            | explicit Release on shutdown; new pod takes over within 50 ms | best-effort AuditDenial entries queued |
| Worker pod crash (SIGKILL, OOM, node loss)     | TTL=15 s elapses; new pod takes over | best-effort AuditDenial swallowed (logged) |
| Postgres unreachable for 2 consecutive renews | renew error logged; goroutine enters hot retry; row is dropped if more than 2 cycles | unchanged |
| Network blip (< 7 s)                           | renew succeeds on next cycle | unchanged |
| Handover during active tick                    | the new leader starts from `tickUnderLease` and reads due schedules immediately | unchanged |

## Connection pool sizing (operations)

| Workload                  | Recommended maxConns | Notes                                          |
| ------------------------- | -------------------- | ---------------------------------------------- |
| Gateway, 1 replica        | 16                   | 1 to migrations, 1 to console admin, 14 to chat |
| Gateway, 4 replicas       | 16 each (64 total)   | stays under Postgres max_connections=200       |
| Worker, 1 replica         | 8                    | one connection for lease renew, plus audit write |
| Worker, 3 replicas        | 8 each (24 total)    | any one holds the lease; other two sleep       |

If you exceed 200 connections × `replicaCount` total in the
Gateway block, the `pre-install-validation` Job emits a warning
rather than failing. Increase `max_connections` on the Postgres
side or lower per-replica `maxConns`.

## Disabling Phase D-1 (rollback path)

Set `worker.replicaCount: 0` to skip the Worker Deployment
altogether. The Gateway Deployment retains the pre-D-1 cron
runner (every replica ticks idempotently). The leases table is
left intact so future Worker pods can pick up the role without
re-migration.

Alternatively, set `NEXUS_SCHEDULER_ROLE_ENABLED=false` on a
transitional Worker pod to keep the lease dormant without
removing the Deployment.

## Sizing checklist

1. Pick Postgres `max_connections` to start. Default
   `max_connections=200`. Larger values require shared_buffers
   and kernel tuning — exceed only if measured.
2. Phase D-1 divides the budget: gateway side about 2/3, worker
   side about 1/3.
3. Round each side down to the maxConns that keeps
   `replicaCount * maxConns` under the budget.
4. Confirm the schema contract (`internal/schemacontract`) and
   ClickHouse verifier pass on the new image with
   `go test ./internal/schemacontract/ -count=1`.
5. Confirm a fresh helm install with custom values does not
   fail the pre-install-validation Job.

## Troubleshooting

**Cron never fires.**
Check the Worker pod logs for `cron: scheduler lease acquired`.
If absent, the lease acquire failed; verify that
`benchmark_scheduler_leases` exists and that the Worker's
`worker.postgres.maxConns` is at least 8.

**Two Workers each fire the same schedule.**
This is a divergent-bug symptom. Inspect
`SELECT owner_id, expires_at > NOW() FROM benchmark_scheduler_leases`
on the database. If `owner_id` is empty or stale, the renew loop
is failing. Look for `leaser: heartbeat failed` in the logs.

**Gateway hits Postgres connection storm on Worker fail-over.**
This is a Helm sizing issue, not a code bug. Reduce
`worker.replicaCount` and check that
`worker.postgres.maxConns` is not above the recommended range.

**Connections balloon to > max_connections.**
The pre-install-validation Job emits a warning, not a failure.
Trim replicaCount or per-pod maxConns accordingly; do not just
raise `max_connections` on Postgres without coordinating with
shared_buffers.
