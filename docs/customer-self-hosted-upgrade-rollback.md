# Upgrading and rolling back a self-hosted install

The design in one sentence: **the schema only moves forward, and the
application is what you roll back.** Everything below follows from that.

## 1. There are no down migrations

Not "we haven't written them yet" — there is no reverse path in the code, and
there will not be one. Automatic DDL reversal is how production data gets
deleted during an incident, at the moment when the person running it is least
able to check what they are about to drop.

What makes that safe is that schema changes are written expand/contract:

1. **Expand** — add the new column, table or index. Nothing reads it yet.
2. **Migrate** — a release starts writing and reading the new shape.
3. **Contract** — the old shape is removed, but only once no version you might
   roll back to still references it.

So version N of the binary runs against the N+1 schema. Rolling the
application back is safe **on its own**, with the schema left where it is.
That is the supported rollback, and it is a `helm rollback`.

## 2. Migrations run as a Helm hook

The chart schedules them; you do not run them by hand.

| | |
| --- | --- |
| Hook | `pre-install,pre-upgrade`, weight `-5` |
| Job name | `<release>-migrate-<revision>` |
| Image | the same `image.tag` as the Deployment |
| Env | the same ConfigMap and Secret as the Deployment |
| Command | `nexus migrate --engine=<migrations.engine> --timeout=<migrations.timeout>` |
| `restartPolicy` | `Never` |
| `backoffLimit` | `migrations.backoffLimit`, default `0` |
| `activeDeadlineSeconds` | `migrations.activeDeadlineSeconds`, default `900` |
| Delete policy | `before-hook-creation,hook-succeeded` |

Two consequences worth knowing before your first upgrade.

**A failed migration is a failed `helm upgrade`.** Helm waits for the hook, so
a bad migration aborts the release. You get an error and a Job you can read
the logs of, rather than a fleet of pods that started against a schema they do
not match. This is the intended behaviour, not a rough edge.

**The failed Job is deliberately kept.** `hook-failed` is absent from the
delete policy, so the pod stays around for you to read:

```bash
kubectl -n nexus get jobs -l app.kubernetes.io/component=migration
kubectl -n nexus logs job/nexus-migrate-7
```

`backoffLimit: 0` is also deliberate. A migration failure is nearly always
deterministic — bad SQL, a missing grant — and retrying mostly buries the real
error under three copies of itself. Raise it to 1 or 2 only if your database
endpoint has transient DNS trouble.

### If you advance the schema yourself

Set `migrations.enabled: false` if a DBA-gated pipeline owns schema changes.
If you do, you **must** run `nexus migrate` before the rollout, or every pod
will come up and stay NotReady.

## 3. Pods verify the schema at boot, and refuse to serve if it is behind

Independently of the hook, each pod checks the ledger at startup. With
`NEXUS_AUTO_MIGRATE` unset (the default, and the correct setting in
Kubernetes) it verifies and does not apply.

`/readyz` returns `503` with the outstanding list:

```json
{
  "ready": false,
  "checks": [
    {
      "name": "postgres_schema",
      "ok": false,
      "required": true,
      "detail": "1 migration(s) outstanding (postgres/014_invite_tokens.sql). Run `nexus migrate` — the chart does this in a pre-upgrade hook Job. Set NEXUS_AUTO_MIGRATE=true only for local development."
    }
  ]
}
```

The two schema checks are graded differently, which matters when you are
deciding whether to page someone:

- **`postgres_schema` is required.** Outstanding Postgres migrations mean the
  pod will not serve traffic at all.
- **`clickhouse_schema` is not required.** Outstanding ClickHouse migrations
  degrade trace and benchmark history. LLM request handling is unaffected and
  the pod stays in service.

`NEXUS_AUTO_MIGRATE=true` makes a pod apply migrations during boot. It exists
for docker-compose and local development. Do not set it in a cluster: with
more than one replica you get several pods racing to migrate at startup, and
the advisory lock in §5 turns that into a rollout where pods wait on each
other instead of starting.

## 4. Pin your version

`image.tag` defaults to the chart's `appVersion`, so an unpinned install moves
when you bump the chart. Pin both:

```bash
helm upgrade nexus oci://ghcr.io/fun-fx/charts/nexus \
  --version 0.6.12 \
  --set image.tag=0.6.12 \
  -f values-production.yaml \
  --atomic --wait --timeout 10m
```

Better still, pin the image by digest — a tag can be repointed, a digest
cannot:

```yaml
image:
  repository: ghcr.io/fun-fx/ffx_nexus
  tag: "0.6.12@sha256:<digest>"
```

The migration Job uses the same tag as the Deployment, so pinning the image
pins the migration that will run. They cannot drift.

## 5. What `nexus migrate` actually does

Useful when you are reading a failed Job's logs.

**Discovery.** Migrations are embedded in the binary at
`migrations/<engine>/NNN_name.sql`, sorted by the leading ordinal. Duplicate
ordinals are a hard error at load.

**The ledger.** Every attempt is recorded in `schema_migrations`, on both
engines:

| Column | |
| --- | --- |
| `id` | `postgres/014_invite_tokens.sql` — engine included |
| `checksum` | SHA-256 of the file at apply time |
| `applied_at`, `duration_ms` | when, and how long |
| `success` | failed attempts are recorded too, and are retried |
| `error` | the failure text |
| `nexus_version` | the build that applied it |

Only rows with `success = true` count as applied.

**Atomicity.** On Postgres, the DDL and its ledger row commit in one
transaction, so there is no state where a migration ran but was not recorded.
ClickHouse has no transactions; correctness there comes from every statement
being written `IF NOT EXISTS`, with the ledger as an audit trail.

**Concurrency.** Postgres work is serialised behind advisory lock
`4242042001`. Safe to run repeatedly and safe to run twice at once — the
second waits. If a run dies holding the lock:

```
timed out waiting for the migration advisory lock (key 4242042001):
another process is migrating, or a previous run died holding it;
inspect with SELECT * FROM pg_locks WHERE locktype='advisory' AND objid=4242042001
```

**ClickHouse is skipped, not failed,** when `NEXUS_CLICKHOUSE_URL` is empty,
so a deployment without ClickHouse needs no extra flags.

### Flags

| Flag | Default | |
| --- | --- | --- |
| `--engine` | `all` | `all`, `postgres`, `clickhouse` |
| `--check` / `--dry-run` | `false` | report outstanding, change nothing, **exit 2** if any |
| `--timeout` | `5m` | overall deadline, including the lock wait |
| `--allow-checksum-drift` | `false` | see below |
| `--verbose` | `false` | debug logging |

Exit codes: `0` up to date or applied, `1` failed, `2` `--check` found
outstanding migrations. That makes `--check` usable as a pre-deploy gate:

```bash
kubectl -n nexus exec deploy/nexus -- nexus migrate --check
```

### Checksum drift

Editing a migration that has already been applied aborts the run:

```
migrate: applied migration has been modified on disk: postgres/014_invite_tokens.sql
(ledger recorded ..., file is now ...). An applied migration must never be
edited - add a new one instead.
```

This is protecting you from a database whose real shape no longer matches the
file that claims to describe it. The correct fix is a new migration.
`--allow-checksum-drift` (`migrations.allowChecksumDrift: true`) exists for
the case where you have verified the schema by hand and know the edit was
cosmetic. Turn it back off afterwards.

## 6. The procedure

### Upgrade

```bash
# 1. Rehearse in staging, same topology as production.
# 2. Check what will run.
kubectl -n nexus exec deploy/nexus -- nexus migrate --check

# 3. Back up Postgres. There is no down migration to undo an expand step.

# 4. Upgrade, pinned and atomic.
helm upgrade nexus oci://ghcr.io/fun-fx/charts/nexus \
  --version <CHART_VERSION> \
  --set image.tag=<APP_VERSION> \
  -f values-production.yaml \
  --atomic --wait --timeout 10m

# 5. Verify.
kubectl -n nexus get pods
curl -s https://<your-gateway>/readyz | jq '.ready'
```

`--atomic` rolls the release back if the rollout fails its readiness gate.
`--wait` is what makes that meaningful; without it Helm returns before the
pods have reported.

### Roll back

```bash
helm history nexus -n nexus
helm rollback nexus <REVISION> -n nexus --wait --timeout 10m
```

Leave the schema alone. The older binary runs against the newer schema — that
is what expand/contract buys you, and it is the whole reason there is nothing
to reverse.

## 7. What the automated rehearsal does and does not prove

The CI gate runs a four-step rehearsal on a real cluster: install with policy
disabled, upgrade to enforce, attempt a deliberately invalid upgrade, and
confirm the enforced state survived it.

It proves the invalid upgrade is **rejected with state preserved** — non-zero
exit, revision unchanged, release still `deployed`, values and rendered
manifest byte-identical. It does not prove observed runtime atomic rollback,
because that step fails at chart render and client validation, before any
change reaches the cluster.

A separate rehearsal covers the runtime case: it points the release at a
namespace that does not exist, which renders and installs cleanly and then
breaks the datapath, and confirms `--atomic` restores a release that answers
`/readyz` again.

The distinction is the one to carry into your own upgrades. Client-side
validation catches a malformed chart. `--atomic` catches a rollout that fails
readiness. **Neither catches a peer list that is merely incomplete** — a
policy that blocks provider egress but not Postgres passes both and fails
every user request. Send real traffic through the gateway before you call an
upgrade done.

Note also that the CNI rehearsal installs with `migrations.enabled=false`,
because its fixture cluster has no real image or Postgres. Migration hook
behaviour is not exercised there.

## 8. Adding a migration

If you are extending Nexus rather than operating it:

- Name it `NNN_short_description.sql` with the next free ordinal for that
  engine. Ordinals must be unique per engine.
- Never edit an applied migration. The checksum is enforced.
- Make every statement replay-safe — `IF NOT EXISTS`, `IF EXISTS`. There is a
  test that requires it.
- Avoid statements Postgres forbids inside a transaction (`CREATE INDEX
  CONCURRENTLY`, `VACUUM`); the migration runs in one. There is a test for
  that too.
- Design expand/contract. Ship the expand, let it run everywhere, and only
  then contract — in a later release, once no version you would roll back to
  reads the old shape.

## Related

- [`network-policy-prerequisites.md`](network-policy-prerequisites.md) — the
  peer list the migration Job needs before it can reach your database
- [`customer-self-hosted-install.md`](customer-self-hosted-install.md) — first
  install
- [`customer-self-hosted-security.md`](customer-self-hosted-security.md) —
  what each secret protects
