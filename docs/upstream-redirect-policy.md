# Upstream redirect policy on LLM calls

The OpenAICompat adapters install a single `http.Client.CheckRedirect`
callback, `stripAuthorizationOnCrossOriginRedirect`, on every provider
client. The callback is what stops an upstream `307 Location` from
forwarding our caller-owned credential shape to a different physical
host, which is the standard server-to-server SSRF-on-redirect shape.

The policy has three parts and the tests are the contract:

1. **Hop cap.** `maxUpstreamRedirects = 10`. After ten hops we return
   `http.ErrUseLastResponse` so the caller sees the `3xx` and the worker
   does not loop. Same cap as the browser/CLI default; any further
   increase is a thesis-level decision (more hops = more pool
   consumption per request, more chances of being the SSRF amplifier).

2. **Cross-origin credential strip.** When the previous hop's scheme or
   host differs from the next hop we delete `Authorization`,
   `x-api-key`, `Cookie`, and `Proxy-Authorization` from the request,
   and we set `Host = ""` so Go's stdlib fills in the destination box
   instead of binding to the previous origin. The previous version
   only stripped `Authorization`/`x-api-key`. `Cookie` and
   `Proxy-Authorization` were forwarded because Go's default is to
   forward everything.

3. **No-origin fallthrough.** A relative Location gets resolved against
   the previous URL; RFC 9110 §10.2.2 forbids a credential leak on a
   same-scheme relative hop, so we treat it like a same-origin redirect
   and only re-evaluate on the next iteration.

The same-origin branch keeps every credential-shaped header because the
redirect is internal to the vendor (path rewriting, protocol upgrade on
the same host).

## Why this is not the `egress.Tenant` guard

The upstream `baseURL` is checked separately at credential-save time by
the console (`internal/console/credentials/...`). Once an org saves a
base URL we treat it as the org's deliberate choice and the only
remaining attack surface is an upstream that betrays the org: a 307 to
a credential-harvesting host. This callback is the mitigation for that
specific failure mode and is invoked on every redirect hop made by the
default transport.

## What we deliberately did NOT add here

- An FQDN allowlist: a self-hosted install points at whatever Langfuse
  / Resend / Resolver the customer runs, and the customer-edge is
  their egress gateway. See `egress/egress.go` package doc and
  `docs/customer-self-hosted-security.md`.
- A second CheckRedirect on the egress guard: the public URLs
  customers tend to redirect to are not the product destination, so
  pre-resolution we don't know which policy to apply.
- An inventory test for the callback itself: it is per-package internal
  and consumed by exactly the adapters declared in `internal/egress`.
  Adding a meta-inventory test invents a third site without lifting
  any policy.
