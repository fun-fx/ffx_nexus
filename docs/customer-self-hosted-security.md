# Self-hosted security model

This document states the security properties Nexus enforces in a customer
deployment, the deployment assumptions those properties depend on, and — just as
importantly — what Nexus does *not* enforce, so nobody plans around a control
that is not there.

It covers browser-facing security (origins, cookies, CSRF), the tenant boundary
between organizations inside one installation, and the outbound destination
policy. Network policy, secret handling, metrics exposure and content capture are
covered in the operations documents. The full resource-by-resource tenancy
contract is in docs/tenancy-model.md.

> Scope note: this file grows over the release-candidate work. Sections marked
> **(pending)** are not yet implemented and must not be treated as controls.

---

## 1. Browser origin policy

The console is a single-page app that authenticates with a cookie. Two separate
controls guard it, because they answer different questions.

### 1.1 What is configured

`NEXUS_PUBLIC_WEB_ORIGINS` (Helm: `config.publicWebOrigins`) is a comma-separated
allowlist of the origins that may talk to the console with credentials. It is
also the source of the CSP `connect-src` directive, so the operator declares
their console origins once.

```yaml
config:
  publicWebOrigins:
    - https://console.customer.example
```

**Same-origin always works and needs no configuration.** If the console and its
API are served from one hostname — the default deployment — you can leave the
allowlist empty. Set it only when a browser loads the console from a *different*
origin than the API it calls.

### 1.2 Matching rules

An origin passes only on an exact match of scheme, host and port:

| Request origin | Against `https://console.customer.example` | Why |
| --- | --- | --- |
| `https://console.customer.example` | allowed | exact |
| `https://CONSOLE.customer.example` | allowed | scheme and host are case-insensitive per RFC 3986 |
| `https://console.customer.example:443` | allowed | `:443` is the default port for `https` and is not part of the origin |
| `https://console.customer.example:8443` | refused | a different port is a different origin |
| `http://console.customer.example` | refused | scheme is part of the origin |
| `https://console.customer.example.evil.example` | refused | the allowed host is only a prefix |
| `https://evil.example@console.customer.example` | refused | userinfo is not valid in an origin |
| `null` | refused | the opaque origin a sandboxed iframe sends |
| `*` | refused as configuration | Nexus does not offer "any origin may send credentials" |

Loopback HTTP origins (`http://localhost:5173`) are accepted **only** when
`NEXUS_DEV_MODE=true`, which logs a warning at boot and must never be set in a
customer deployment. Lookalike hosts such as `http://localhost.evil.example` are
refused even in dev mode, because the match is on the parsed hostname rather than
a string prefix.

**Failure mode is closed.** An unset allowlist does not fall back to permissive:
cross-origin credentialed requests are refused and only same-origin passes.

Tests: `internal/console/origin_bypass_test.go`, `internal/console/origin_test.go`.

### 1.3 WebSocket

The `/api/live` WebSocket handshake is a plain `GET` that browsers perform with
no preflight, and `Access-Control-Allow-Origin` plays no part in it. CORS
therefore does not protect it. The upgrader applies the same allowlist
independently through `CheckOrigin`, and refuses the same bypass shapes listed
above.

---

## 2. Cookies and TLS termination

### 2.1 Attributes

| Cookie | Path | HttpOnly | Secure | SameSite | Expiry |
| --- | --- | --- | --- | --- | --- |
| session | `/` | yes | yes (default) | Lax | session TTL |
| SSO state | `/api/auth/sso` | yes | yes (default) | Lax | short state TTL |

`SameSite=Lax` rather than `Strict` is required for SSO: `Strict` would drop the
state cookie on the IdP's cross-site redirect back to Nexus, breaking every
login. `Lax` still withholds the cookie on cross-site subrequests, which is the
part that matters.

The SSO state cookie is scoped to `/api/auth/sso` because only the callback reads
it; there is no reason to attach it to every console request.

### 2.2 Deployment assumption: terminate TLS in front of Nexus

**The `Secure` attribute comes from configuration, not from the request.**

In every realistic deployment the browser speaks HTTPS to an ingress and the
ingress speaks plain HTTP to the pod, so the pod observes a non-TLS request even
when the connection was encrypted end to end. Nexus therefore marks cookies
`Secure` based on `NEXUS_SECURE_COOKIES` (default `true`) and **never inspects
`X-Forwarded-Proto`**.

This is deliberate. `X-Forwarded-Proto` is a client-supplied header, and a pod's
ClusterIP is reachable from inside the cluster. A request sent directly to the
pod with `X-Forwarded-Proto: https` would otherwise dictate its own cookie
flags. Because Nexus ignores the header entirely, there is no "trusted proxy"
configuration to get wrong, and no requirement that your ingress strip or
overwrite it for cookie correctness.

What this *does* require of the deployment:

1. **Terminate TLS in front of Nexus** (ingress, load balancer, or service mesh)
   and serve the console only over HTTPS.
2. **Leave `NEXUS_SECURE_COOKIES` at its default `true`.** Setting it to `false`
   is for local plain-HTTP development only; it logs a warning at boot.
3. Prefer HSTS at the ingress. `Secure` stops the cookie being *sent* over
   cleartext; HSTS stops the browser trying in the first place.

Tests: `internal/console/cookie_proxy_test.go`.

---

## 3. CSRF

**A CORS allowlist is not CSRF protection, and Nexus does not treat it as one.**

CORS governs what a script may *read*. A cross-site form or `<img>` can still
*issue* a credentialed `POST` with no preflight at all, and the browser will
deliver it with the session cookie attached. For "revoke this key" or "delete
this user", the attacker never needed to read the response.

Nexus therefore checks the `Origin` header server-side on every state-changing
request:

- **Applies to** `POST`, `PUT`, `PATCH`, `DELETE` and any other non-safe method.
- **Passes** when the origin is same-origin, or is in the allowlist.
- **Refuses** with `403` otherwise, logging the origin, method and path.
- **`GET`/`HEAD`/`OPTIONS` are exempt** because they must not change state. If
  one of them does, the handler is the defect, not this list.

### A missing `Origin` header is allowed through

Browsers attach `Origin` to every state-changing request, so its absence means a
non-browser client: `curl`, a CI job, your automation. Those carry no ambient
cookie for an attacker to borrow, so they are not subject to CSRF, and rejecting
on absence would break every API client while adding nothing — an attacker's page
cannot suppress the header, so "no `Origin`" is not a bypass.

### Implication for your automation

Server-to-server callers should authenticate with a **virtual key** against the
gateway rather than with a console session cookie. Cookie authentication is for
browsers.

Tests: `internal/console/origin_test.go` (`TestCrossOriginStateChangingRequestsAreRefused`
and neighbours).

---

## 4. Tenant boundary between organizations

A single-customer installation still separates teams and departments as
organizations. That boundary is enforced on the server, in the SQL, and not in
the front-end.

### 4.1 How a request's org is decided

The organization comes from the authenticated session. The `X-Org-Id` header is
consulted **only** for requests that have no session, and can never override a
session's org. A caller cannot change tenant by setting a header.

### 4.2 Object-level authorization

Every route that accepts an object id verifies that the object belongs to the
caller's org before reading or acting on it. Refusals are `404`, not `403`: a
`403` saying "that belongs to another org" confirms the id exists, which turns a
guess into an enumeration oracle.

Covered objects include virtual keys, provider credentials, users, invites,
audit entries, spend, benchmark runs, benchmark schedules, eval profiles and eval
plugins.

Rows written before org attribution existed (`org_id = ''`) belong to the
installation's **default org**, not to every org.

### 4.3 Cluster-wide vs per-org configuration

Two config objects support a deliberate cluster-wide scope, written by the
operator through env/Helm seeding and inherited by every org:

- **Eval profiles** with no `org_id` — the seeded `default-pii` and
  `default-completeness` heuristics.
- **Eval plugins** with no `org_id` — plugins installed from a Helm-mounted
  manifest directory.

Cluster-wide rows are readable and usable by every org. **No HTTP request can
create one**: the console always stamps the caller's org, so a tenant cannot
mint configuration that runs against another tenant's traffic. An org's own row
shadows an inherited row of the same name rather than doubling it.

### 4.4 Deliberately installation-wide aggregates

Two read paths aggregate across every org on purpose. Both are documented in code
and guarded by tests so the decision cannot erode into a leak:

- **`router.ModelStats`** (`GET /api/routing`) — model-level latency, error rate
  and throughput used for routing decisions. This answers "is this upstream
  healthy", a question about a shared provider. The payload carries no field
  capable of naming a tenant, user, trace or prompt, and
  `internal/router/tenancy_test.go` fails the build if one is added.
- **Benchmark quality blend** (`GET /api/eval/benchmarks/leaderboard`) — the
  average score the router blends per model. The page exists to explain the
  router's behaviour, so a per-org recomputation would disagree with what the
  router actually does. Per-run detail on the same page — run ids, min/max
  scores, sample counts — *is* org-scoped, because a run reflects one tenant's
  spec, dataset and provider key. A row may therefore show a blended average with
  an empty `latest_run_id`, which reads correctly as "the router is using
  benchmark data for this model, but none of it is yours."

If your threat model does not permit model-level operational aggregates to be
shared between departments, deploy those departments as separate installations.
Nexus does not offer a per-org routing brain.

---

## 5. Outbound destination policy (SSRF)

Nexus makes server-side HTTP requests to destinations somebody configured: your
OTLP collector, your mail relay, and — importantly — endpoints that **org admins**
supply through the API. Those are two different trust levels and get two
different policies.

### 5.1 Why the tenant case is not the same as the operator case

Two paths POST prompt content to a URL an org admin chose **and store the
response**:

- An eval profile's `endpoint.base_url`: the worker sends the prompt and
  completion, then writes the reply into `eval_scores` as the score rationale,
  which the console renders.
- A plugin manifest's `spec.service.endpoint`: the same, through the collector.

A request from the pod can reach things the org admin cannot: your database, your
internal services, and the cloud instance metadata service. Point an eval profile
at `http://169.254.169.254/latest/meta-data/iam/security-credentials/` and the
pod's IAM credentials come back as an evaluation rationale. The fetch is
server-side, the response returns, and it is persisted.

### 5.2 The policy

| Destination configured by | Public | Private (RFC1918) | Loopback | Link-local / metadata |
| --- | --- | --- | --- | --- |
| **You** (env var, Helm value) | allowed | allowed | allowed | refused |
| **An org admin** (API request, DB row) | allowed | refused by default | always refused | always refused |

Private and loopback are allowed for your own configuration on purpose: in a
self-hosted install the collector is often a sidecar on `127.0.0.1` or a
ClusterIP on `10.x`, and refusing those would mean telemetry only works when it
leaves your cluster.

The check runs **after DNS resolution, at connect time**, so a hostname that
resolves to a public address when validated and a private one when fetched does
not work. Redirects are re-checked at each hop, bounded to three, refused on an
HTTPS-to-HTTP downgrade, and have `Authorization` and vendor key headers stripped
when the host changes.

`HTTP_PROXY`/`HTTPS_PROXY` in the pod environment are deliberately **not
honoured** for these requests. A proxy would make the socket connect to the
proxy's address while the real destination travelled in the request line, where
the check would never see it.

### 5.3 If you run a vendor inside the cluster

To let org admins point eval plugins at an in-cluster Langfuse or collector, name
its range:

```yaml
config:
  egressTenantAllowedCidrs: "10.44.0.0/16"
```

It is a CIDR list rather than a switch so that widening the policy names what is
being opened. Link-local can never be re-permitted, whatever you put here.

### 5.4 What this is not

**It is not a vendor hostname allowlist.** Nexus does not restrict *which*
internet hosts an org admin may send traces to, because enumerating the tools a
customer might pick would mean a product release per customer. FQDN-level egress
control belongs in your egress gateway, NAT firewall or service mesh; see the
operations document for what to allowlist there.

Tests: `internal/egress/egress_test.go`, `internal/evals/egress_tenancy_test.go`,
`internal/console/egress_endpoint_test.go`. Every outbound path is enumerated in
`internal/egress/inventory_test.go`, which fails the build when a new one appears
without a decision.

### 5.5 Mail relay is operator-class and policy-checked

The Invite User feature's outbound mail transport is constructed through
the same egress guard as the HTTP clients: SMTP uses `egress.Dialer(class=Operator)`,
Resend uses `egress.Client(class=Operator)`. The connect-time address
check refuses the SMTP path if the relay hostname resolves to a private
address that the operator did not pre-declare, and refuses the Resend path
the same way.

Two redirects-to-private address scenarios are explicitly handled:

- An SMTP relay that 302s on the SMTP greeting is not a thing (SMTP does
  not have redirects), so the redirect handler's `checkRedirect` does not
  fire; the dial-time check is the full policy.
- A Resend-base-URL whose DNS rebinds to a private address is run through
  the same connect-time `Control` hook the HTTP client uses; the dial
  itself is refused before the POST.

A relay that does not advertise STARTTLS is refused when SMTP credentials
are present — sending credentials in cleartext is the case that the
guardrail is load-bearing for. A relay that allows IP allowlisting and
not username/password auth is supported: leave `NEXUS_SMTP_USERNAME`
empty and the SMTP transport skips AUTH entirely.

Inventory: `internal/egress/inventory_test.go` now also flags any raw
`*net.Dialer` build outside the guard, since bypassing the guard from
a new SMTP or gRPC transport would defeat the connect-time check this
section specifies.

### 5.6 What the operator chooses to trust

Nexus does not pick the customer's mail transport. The operator picks
SMTP or Resend at install time, with the matching credentials, and the
boot helper fail-fasts on any half-configured combination (e.g.,
`smtp` named with no host). The defaults are no transport — silently
dropping invites is worse than a loud boot abort, and the boot path
treats the failure as the softer of two harms.

If you want a third-party mail SaaS that we do not wrap today, file an
issue; the closed set in `internal/console/mailer.go` is closed for the
opposite reason than the eval plugin set — bandwidth to ship and audit a
third wrapper is finite — but it is extensible through the same `Mailer`
interface any transport that follows the existing security properties
will satisfy.

---

## 6. Documentation of controls Nexus does not provide

- **FQDN-based egress restriction** is not enforced by the shipped Kubernetes
  NetworkPolicies, and the destination policy in §5 restricts address ranges
  rather than hostnames. Use an egress gateway, NAT firewall or service mesh for
  vendor allowlisting.
- **Egress restriction by vendor hostname** is not enforced; see §5.4.
- **Schema rollback** is not automated. Migrations are additive; application
  rollback and schema rollback are separate problems, covered in the
  upgrade/rollback document.
