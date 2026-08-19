# Audit Action / Reason / Public Code Catalog (c0.2 / c0.4 / c0.8 stable contract)

This document is the change-managed contract for every AuditAction literal,
every AuditReason literal, and the mapping between AuditReason and
`apierr.Code`. The string values are stable. Renaming a value is a
customer-visible breaking change: SIEM rules written against these strings
will silently drop rows the day the value changes.

The Go source-of-truth lives in:
- `internal/core/audit.go` (`AuditAction`, `AuditReason`)
- `internal/apierr/apierr.go` (`Code` consts)
- `internal/core/audit_inventory_test.go` (c0.8 inventory)
- `internal/core/audit_reason_inventory_test.go` (c0.2 inventory)

The matching tests fail on any silent drift. Any reviewer approving a
rename must verify the integrated test failure and either revert or
explicitly write the new strings through a deprecation cycle.

## Stability rules

- **Action strings**: 60+ entries. Values are namespaced `<category>.<verb>`.
  Categories cover the full denied-attempts taxonomy: `auth.*`, `sso.*`,
  `denied.*`, `key.*`, `credential.*`, `user.*`, `invite.*`, `routing.*`,
  `eval.*`, `benchmark.*`, `policy.*`, `security.*`, `audit.*`.

- **Reason strings**: 26 entries. Many-to-one mapping to `apierr.Code`
  is intentional: customers see one apierr.Code, SIEM keeps more granular
  reasons.

- **Public codes**: stable strings in the apierr.Body JSON. Customers
  branch on these in their SDK error handlers.

## Mapping

| AuditReason                            | apierr.Code                  | Note                                    |
|----------------------------------------|------------------------------|-----------------------------------------|
| `forbidden`                            | `forbidden`                  | primary                                 |
| `org_boundary`                         | `forbidden`                  | cross-org attempt                       |
| `origin_not_allowed`                   | `forbidden`                  | CORS / Origin gate                      |
| `cors_disallowed`                      | `forbidden`                  | explicit CORS preflight rejection        |
| `model_not_allowed`                    | `forbidden`                  | model allowlist violation               |
| `invalid_credentials`                  | `unauthenticated`            | primary                                 |
| `key_invalid`                          | `unauthenticated`            | absent / malformed / unknown key         |
| `key_expired`                          | `unauthenticated`            | past TTL                                 |
| `key_revoked`                          | `unauthenticated`            | deliberate revocation                   |
| `rate_limited`                         | `rate_limited`               | IP / key RPM cap                        |
| `request_too_large`                    | `request_too_large`          | body, file, prompt input                |
| `egress_address_blocked`               | `egress_denied`              | private/loopback egress reject           |
| `egress_resolver_fail`                 | `egress_denied`              | DNS / EHOSTUNREACH                      |
| `egress_dns_rebind`                    | `egress_denied`              | A→B A-record flip                       |
| `plugin_manifest_invalid`              | `eval_plugin_invalid`        | eval plugin save-time failure           |
| `budget_exceeded`                      | `budget_exceeded`            | per-key / per-org monthly budget        |
| `concurrency_cap_exceeded`             | `concurrency_limit`          | per-model cap                           |
| `invite_invalid`                       | `invite_invalid`             | primary                                 |
| `invite_expired`                       | `invite_invalid`             | past TTL                                |
| `invite_replay`                        | `invite_invalid`             | single-use replay attempt               |
| `sso_state_mismatch`                   | `sso_state_invalid`          | state cookie vs param                    |
| `sso_nonce_mismatch`                    | `sso_state_invalid`          | nonce cookie vs param                   |
| `schema_contract_violation`            | `schema_contract_violation`  | runtime SQL contract drift               |
| `audit_permission_denied`              | `admin_required`             | non-admin tries to call /api/audit      |
| `internal_error`                       | `internal_error`             | catch-all                                |
| `unknown`                              | `internal_error`             | pre-classification                      |

A `AuditReason` not present in this table is a defect: the inventory test
(`TestReasonMappingIsExhaustive`) fails. A `apierr.Code` route that no reason
maps to is also a defect: `TestReasonAndPublicCodeCrossReferenceMatchesInventory`
fails unless a reason is wired.

## c0.3 Burst-aggregation policy

High-volume denial events are burst-collapsed into a single audit row per
5-minute window (UTC-aligned, multiple-of-5 minute boundary). The
aggregator set is closed and pinned by
`internal/auditaggregator/aggregator_test.go`:

| Aggregated actions                                                  |
|---------------------------------------------------------------------|
| `auth.login.denied`                                                 |
| `user.login.denied`                                                 |
| `key.rejected.invalid` / `-expired` / `-revoked`                    |
| `rate_limited`                                                      |
| `request_size`                                                      |
| `budget.exceeded`                                                   |
| `concurrency.cap`                                                   |
| `model.allowlist`                                                   |
| `invite.rejected.invalid` / `invite.rejected.replay`                |

The dedup key is `(action, actor, resource_fingerprint, first_at)`. The
resource_fingerprint is `sha256(target)[0:16]` so attacker-controlled
long URLs don't blow up the index.

Information lost in burst-collapse (documented per c0.3.4):

- **Individual request paths**: only the first path is preserved; subsequent
  attempts that hit the same fingerprint fall into the same row. An
  attacker who probes `/v1/chat` 1000 times surfaces as a row with
  count=1000, target_id=/v1/chat — there is no per-request record
  kept.
- **Individual client_request_id**: only the most recent request id is
  kept on update; the first request id is replaced on the second
  collision.
- **Timestamps**: `first_at` and `last_at` bracket the window but
  intermediate timestamps within the window collapse.
- **Operator forensic depth**: investigating "exactly which IP hit at
  14:01:36 UTC" requires correlating with HTTP access logs (the
  audit row retains the count + window, not per-event context).

Anything not in the aggregated set is written individually. High-severity
denials (`denied.org.boundary`, `denied.origin`, `denied.cors`,
`denied.egress`, `denied.audit.view`, `denied.audit.export`,
`denied.schema.contract`, `security.panic.recovered`) are excluded from
the aggregated set so each occurrence remains a separate row for
investigators.

## Inventory tests (must pass at PR time)

- `internal/core/audit_inventory_test.go` (c0.8) — action / reason / code
  catalog must match declared constants
- `internal/core/audit_reason_inventory_test.go` (c0.2) — golden strings
  pinned
- `internal/apierr/leak_test.go` — protected signatures and code constants

Adding/amending a value CHANGES the tests in the same PR. No silent drift.
