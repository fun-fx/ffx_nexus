# Credential base_url policy — save-time and dial-time gates

Two gates, one shared rule. Both the save-time path (CreateCredential
in `internal/core/store.go`) and the dial-time path
(`CredentialResolver.Resolve` in
`internal/gateway/credential_resolver.go`) consult
`urlpolicy.Validate(raw, allowlistCSV)` before persisting or
dialling. The same rule is enforced in two places so drift can
be checked at PR review time: a save-time change without the
corresponding dial-time pick-up would survive in tests but
fail at the customer's first request after a credential edit.

## The gate's surface

A base_url passes when **all five** items are true:

1. **Non-empty.** — Operators who leave the field blank see
   `ErrEmptyURL`.
2. **HTTPS scheme only.** — `http://`, `ws://`, `ftp://`, etc.
   produce `ErrUnsupportedScheme`. SSH / TLS on a different port
   is also caught here.
3. **No path-traversal segments.** — Anything containing `..`
   produces `ErrTraversalSuffix`. Targets like
   `https://api.openai.com/v1/../../admin` are probes for
   upstream-mounted paths.
4. **Parses as a URL with a hostname.** — `ErrParseURL` covers
   missing hostnames, malformed ports, or velocities in the
   path the URL package can't parse.
5. **Hostname resolves to a public IP, OR appears in the
   operator's allowlist.** — Loopback, link-local, RFC1918,
   RFC4193 (IPv6 ULA), CGNAT-range, and `unspecified` addresses
   fail with `ErrPrivateNetwork`. The exception is the
   operator's CIDR allowlist (see below).

## Operator allowlist (`NEXUS_EGRESS_TENANT_ALLOWED_CIDRS`)

The allowlist is a comma-separated CIDR list. Two use cases:

- **Self-hosted customer running OpenAI on `10.0.42.7`.**
  Without the gate, the save-time check would reject
  `https://10.0.42.7/v1` because the address is RFC1918. The
  operator adds `10.0.42.0/24` to the allowlist; both gates
  honour it.

- **Self-hosted eval pipeline pointing at an internal
  Langfuse.** Same mechanism; the relevant CIDR goes into the
  allowlist. Link-local cannot be allowlisted even here, by
  design: link-local is where instance metadata services live
  (`169.254.169.254`), and exempting it would re-permit a class
  of attack we deliberately closed.

The allowlist is **the same** as `egressTenantAllowedCidrs` in
the Helm chart (`deploy/helm/nexus/values.yaml`); both
gates read the same CSV so a single configuration entry
controls both paths.

## Where each gate lives

**Save-time** (`Store.CreateCredential`):

- Reject paths the resolver would reject on the first request,
  so the operator gets the error at the form's submit step,
  not at the first timeout while debugging latency.
- Called from three sites: console admin route
  (`/api/admin/credentials`), per-user self-service
  (`/api/me/credentials`), and the register-on-login migration
  path in `internal/console/auth.go`.

**Dial-time** (`CredentialResolver.Resolve`):

- Runs each time a credential is returned to a handler, even
  if the resolver has the entry cached. A pre-validation cache
  hit is re-validated before the call returns so an operator
  who tightens the allowlist at runtime sees the change
  honoured on the next request, not after the cache TTL.
- Without this re-validation, a saved credential whose
  base_url no longer passes could be returned with no error
  and would dial anyway. The two-gate design exists because
  each gate catches what the other misses: save-time catches
  fresh bad URLs immediately; dial-time catches drift caused
  by runtime tightening or upstream IP movement.

## Fail modes that are deliberately hard-closed

- A `base_url` set to `http://` (no TLS): rejected at both
  gates with `ErrUnsupportedScheme`. The credential is not
  persisted at save-time and is never used at dial-time.
- A `base_url` set to `https://169.254.169.254`: rejected
  with `ErrPrivateNetwork` at both gates. Even with
  `--egress-allow-loopback` set elsewhere in the policy, this
  address is unreachable from the gateway.
- A `base_url` ending in `/v1/../../etc/passwd`: rejected with
  `ErrTraversalSuffix`. The traversal segment is detected
  before DNS resolution, so the path never sees traffic.

## Why not fail-open on a single private-IP entry

A naive policy that allows "any private IP with `?allow=true`
in the query" is what most SSRF studies name as the first
point of compromise. The shape here is a CIDR list baked into
helmet configuration, audit-logged on apply, and validated
via the same allowlist parser in both gates. Adding an IP to
the list is a Helm change; nobody can do it from the runtime
console.

## Test surface

- `internal/urlpolicy/urlpolicy_test.go` — eight pin-style
  unit tests covering empty, scheme, traversal, public,
  private, allowlist, IPv6. A future refactor that drops the
  loopback / link-local check fails at least one of these.
- The dial-time gate re-runs the same validator every
  Resolve, so a regression in `urlpolicy` is observed at
  the next credential hit.

## Migration path

For customers whose credentials were stored **before** this
gate shipped: the dial-time gate catches them on the next
request, returning a 500 with `ErrPrivateNetwork` only once
the new Resolve path sees them. Save-time is naturally
silent for the historical set. Operators must rotate the
allowlist CSV in Helm to bring those credentials back,
or migrate them to public-IP endpoints.
