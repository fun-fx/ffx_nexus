# Audit Failure Policy (c0.5)

When the audit subsystem cannot write a row, what should the rest of the
service do? Two failure modes need different responses:

- **Fail-stop**: the customer request that triggered the audit must fail.
  The audit row is the durable record of the security-relevant action;
  silently dropping it is worse than a customer-visible error.
- **Best-effort**: the audit row is desired but not load-bearing.
  Failure must surface as a metric and a log entry, but the customer
  request must succeed anyway.

A blanket "everything fails on audit failure" decision would cause a
Postgres outage to take the entire LLM gateway offline. A blanket
"everything is best-effort" decision would let privilege escalation
happen invisibly. The table below is the catalogue we operate on.

| Operation                                          | Class       | Reason                                                   |
|----------------------------------------------------|-------------|----------------------------------------------------------|
| `audit.view`                                       | fail-stop   | Auditor must always see the data. If we cannot record "admin viewed audit", we cannot enforce the absence of evading queries. |
| `audit.export`                                     | fail-stop   | Same reason; export is a higher-privilege read.          |
| `auth.login.succeeded`                             | fail-stop   | Compliance audit: customer support needs to confirm success. |
| `auth.login.denied`                                | best-effort | High-volume; DoS amplifier if every failed login stops service. |
| `user.update` (admin-only)                         | fail-stop   | Admin actions are security-state changes.                |
| `credential.update`                                | fail-stop   | Credential rotation is a security-state change.          |
| `key.create`, `key.revoke`                         | fail-stop   | API key lifecycle is a security-state change.            |
| `key.accepted`                                     | best-effort | Hot path; ingestion of high-volume traffic.              |
| `key.rejected.invalid / expired / revoked`         | best-effort | Aggregated path; high-volume under attack.               |
| `denied.org.boundary`                              | fail-stop   | Cross-org attempt is a security incident.                |
| `denied.origin`, `denied.cors`                     | fail-stop   | May indicate probing.                                    |
| `denied.egress`                                    | fail-stop   | Egress blocks are security-relevant (data exfil prevention). |
| `rate_limited`                                     | best-effort | Aggregated; failing every rate-limit response is a DoS on services. |
| `request_size`                                     | best-effort | Aggregated; same reasoning.                              |
| `eval.*.create`                                    | best-effort | Configuration edits, low security impact.                |
| `benchmark.run.*`                                  | best-effort | Operational telemetry; not security-relevant.            |
| `integration.*`                                    | best-effort | External-system wiring; not security-relevant.           |
| `security.panic.recovered`                         | fail-stop   | A recovered panic is a security-relevant event.          |
| `policy.create` / `policy.delete`                  | fail-stop   | Policy state changes are security-relevant.              |

The classification is closed: adding an audit action without a fail-stop /
best-effort decision is a code-review defect. The classification lives
in `internal/core/audit_failstop_test.go` and the `auditFailureClass`
map; it is exercised by `TestAuditFailureClassIsExhaustive`.

The companion contract:

- **Metric**: `nexus_audit_write_failed_total{category, reason}` is
  the operator's alarm source. Categories and reasons are bounded;
  `org_id` / `actor_id` are deliberately *not* labels (would explode
  cardinality under attack).
- **Log line**: every audit failure writes a structured slog entry
  with `request_id`, `action`, and a scrubbed error. The log entry is
  the second-line signal: an operator reviewing a customer's ticket
  can correlate by the same request id.
- **Decision**: the engine layer that commits the audit row reads
  the metric + log signal and chooses fail-stop vs best-effort for
  its own HTTP response. The framework does NOT auto-fail the
  request — that decision is made by the handler.

## Test failure injection

`TestAuditFailureInjectionDoesNotStopGatewayRequests` in
`internal/core/audit_failstop_test.go`:

1. Wraps the audit-metric surface with a `failingStore` that
   simulates `Audit` returning a write error.
2. Drives a hypothetical LLM gateway request whose logger emits the
   request id but whose path does NOT consult the audit row.
3. Asserts:
   - The gateway request completes 200 OK.
   - `nexus_audit_write_failed_total{category=...,reason=...}` is
     incremented by exactly 1.
   - The slog error carries the original request id.

The companion test `TestAuditFailureDoesStopFailStopOperation`
asserts the opposite: a fail-stop-class request (e.g. login) sees
the same audit failure surface and returns 500.

## What is NOT in this design

- **Queue-fronted audit writes** are NOT used. A queue would cause
  audit failures to be silently dropped when the queue is full and
  would defeat the "audit row at the same instant as the action"
  invariant. The design is direct-write so a failure is observed
  synchronously by the handler.
- **Fallback to alternative storage** is NOT implemented. A dual-write
  strategy (e.g. write to ClickHouse on Postgres failure) would mask
  the outage and re-introduce the c0.4 single-source-of-truth weakness.
  Failures are visible as metrics and stay visible until resolved.
