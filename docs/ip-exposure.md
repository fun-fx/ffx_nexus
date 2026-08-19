# Source IP & Cookie Secure Decisions (c0.6)

This page describes Nexus's policy for source IP attribution and
explains the relationship to the earlier decision **not to trust
`X-Forwarded-Proto` for cookie Secure determination** (Phase B).
The two decisions look superficially similar (both involve a
proxy-supplied HTTP header determining a security-relevant property)
but the threat models are different, so the policies are different.

## Source IP

### The contract

1. **Raw socket address is canonical.** `audit_log.request_ip` stores
   `RemoteAddr` verbatim. Even if a proxy header says otherwise, the
   raw socket is what we know is true.
2. **Header-based effective IP is opt-in.** Operators MUST supply
   `trustedProxyCIDRs` for Nexus to trust `X-Forwarded-For`. When
   that list is empty, the effective IP is the socket address and
   `X-Forwarded-For` is ignored entirely.
3. **The walk is right-to-left, not left-to-right.** `X-Forwarded-For`
   conventionally lists hops in chronological order — the leftmost is
   the original client, the rightmost was appended by our reverse
   proxy. We trust the rightmost because we know our proxy lives in
   the trusted CIDRs; everything to the left of that hop is unverified
   proxy hops and the original client.
4. **The walk stops on the first untrusted hop.** If we see an IP
   outside `trustedProxyCIDRs` while walking leftward, we stop. The
   effective IP is the rightmost-trusted IP we already accepted.
5. **Capped hop count.** `trustedProxyHops` (default 1) caps the walk.
   Most Cloudflare-fronted deployments need exactly 1 hop; deeper
   chains require explicit opt-in.
6. **Raw socket is retained even when effective IP is header-derived.**
   The audit row carries `request_ip` (the socket) and `effective_ip`
   (the header-derived value when trusted) in separate fields. This
   lets an analyst reconcile logs with HTTP access logs even when the
   header chain was spoofed.
7. **Header length cap.** Headers longer than 8 KiB are
   silently thrown away and the socket address is used. A megabyte
   X-Forwarded-For is not a real client.
8. **IPv6 and port-included addresses are supported.** Bracketed forms
   (`[::1]:80`) and bare forms (`::1`) parse cleanly without panicking.

### Masking options

Operators may configure `--ip-mask-level`:

- `off`: keep the full IP in retention.
- `loose`: zero the host portion (/24 for IPv4, /48 for IPv6).
- `strict`: zero the host portion + last 16 bits (/16 for IPv4, /32 for IPv6).
- `none`: hash the IP with a per-org salt so cross-org correlation
  is impossible.

The masking is applied at the audit-row write site, after the raw
socket is recorded. The audit-page API renders masked IPs by default
unless the viewer has the `audit.masked_view: false` role.

### Retained evidence

- `raw_socket_addr` — the unmodified `RemoteAddr` (host:port).
- `effective_ip` — the IP we used for rate-limit / access decision.
- `hops_used` — count of hops accepted from `X-Forwarded-For`.
- `chain` — the full parsed hop list (capped at 8 KiB).

These fields are populated by the `internal/ippolicy` package and
consumed by both the gateway and console middlewares.

## Why IP source ≠ cookie Secure

Phase B decided we would NOT trust `X-Forwarded-Proto` for the
`Secure` cookie attribute, because a TLS-terminating proxy that
declares HTTPS in `X-Forwarded-Proto` does not give Nexus any way
to prove the original hop was HTTPS — an attacker on a
man-in-the-middle position can simply send the header. The decision
recorded there is: the cookie's `Secure` attribute follows the
**transport between the customer and Nexus**, full stop.

The source-IP decision here is *less conservative*: we DO trust
`X-Forwarded-For` when the upstream proxy is in our trusted CIDRs.
Why the difference? Because the *threats are asymmetric*:

| Decision                   | What an attacker gains by lying                                  |
|----------------------------|------------------------------------------------------------------|
| Trick `X-Forwarded-Proto`  | Cookies marked Secure sent over HTTP, intercepted in plaintext. |
| Trick `X-Forwarded-For`    | Their IP appears in audit_log / defeat /api/auth/rate-limit.     |

A forged `X-Forwarded-Proto` causes a **customer credential leak**.
A forged `X-Forwarded-For` causes rate-limit bypass and audit
attribution noise — both of which our 8 KiB cap, hop limit, and
CIDR-walk guard against, and which an analyst can still detect
because the raw socket address is retained separately.

If the trust boundary breaks for an `X-Forwarded-Proto` attacker,
they can read production credentials. If the trust boundary breaks
for an `X-Forwarded-For` attacker, they can hide behind a
fake IP for a while. The latter is much less catastrophic. Both
decisions are documented tests in the code; their test-mutation
follow-up is "what if a future engineer turns the trust on" — for
cookies, the test fires loud; for IP, the walk still rejects
untrusted hops.

## Documentation rules

Any change to the policy above must update:
- `internal/ippolicy/ippolicy.go` (the source of truth)
- `docs/ip-exposure.md` (this file)
- `internal/console/security.go` (`clientIP` helper, which delegates
  to `ippolicy`)
- The Helm chart `values.schema.json` (the configuration surface)
