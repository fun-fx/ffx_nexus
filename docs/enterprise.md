# Enterprise deployments

Nexus is open core. Everything in this repository — the gateway, the console,
the eval layer, the router, the Helm chart — is what you self-host, and there
is no licence key gating a feature. This page describes what a production
deployment inside your own perimeter looks like, and what we do alongside it.

**[Book a demo](https://nexus.ffx.ai/demo)**

## What "enterprise" means here

It does not mean a different binary. It means the deployment shape that a
security review actually passes: your cluster, your datastores, your identity
provider, your credentials, and a network policy that fails closed.

- [Self-hosted install](customer-self-hosted-install.md) — the end-to-end
  install, from namespace to first login
- [Security posture](customer-self-hosted-security.md) — what is stored, what
  is redacted, what leaves the cluster
- [Upgrade and rollback](customer-self-hosted-upgrade-rollback.md) — the
  rehearsal procedure and the failure modes it covers
- [NetworkPolicy prerequisites](network-policy-prerequisites.md) — what your
  CNI must support before enforcement is meaningful

## One gateway, every provider

OpenAI, Anthropic, Gemini, Groq, Mistral, The Grid and any OpenAI-compatible
endpoint plug in behind the same interface, so one integration covers whatever
combination your teams standardised on. Streaming, virtual keys per team, and
per-call cost tracking are part of the gateway rather than tacked on. A team
that wants to keep its provider relationships but standardise its internal
surface maintains one endpoint, not ten.

## The provider key never reaches the caller

Provider API keys live in the platform. They are never returned to a caller
and never appear in logs or audit exports. Operators, teams and individual
users hold **virtual keys** (`nxs_live_…`) that the platform resolves to a real
credential at request time.

Rotating the platform key does not invalidate the virtual keys. Budgets and
routing rules attach to the virtual key, so revoking one cuts off cost from a
single switch.

Credentials are encrypted at rest with a key you supply (`NEXUS_MASTER_KEY`)
and that we never hold. In `strict_byok` mode a tenant's requests only ever go
out on that tenant's own provider credential — there is no shared fallback to
fall back to.

## Tenant isolation by construction

Each customer org has its own encryption keys, its own ClickHouse database,
its own SSO realm — or its own customer-managed IdP — and a tenant ID on every
trace span. Cross-tenant reads are prevented by the query path, not by a
policy document. See [the tenancy model](tenancy-model.md).

## SSO with your IdP

One sign-in path accepts any OIDC provider: your own Keycloak if you
self-host, your own Okta or Azure AD or Google Workspace if IT standardised on
one, or a Nexus-managed realm if you would rather not run one. The same code
path serves all three; there is no product split.

## Audit, by role

Every request carries the actor and the org, and the audit-log export surfaces
that pairing as a queryable artefact — who did what, when, on whose authority,
with what resolution. See [audit log roles](audit-log-roles.md) and the
[fail-stop policy](audit-failstop-policy.md) that decides when an unwritable
audit log stops the request instead of dropping the record.

## Egress control that fails closed

The chart's `enterprise` NetworkPolicy profile refuses to install until every
egress peer is named. That is deliberate: a policy that silently allows what
it cannot classify is worse than no policy, because it reads like protection
in a review. See [NetworkPolicy prerequisites](network-policy-prerequisites.md).

## The console CTA

A self-hosted console shows a "Talk to us" link on its login page only when
the operator sets one:

```yaml
# deploy/helm/nexus/values.yaml
config:
  enterpriseCtaUrl: "https://nexus.ffx.ai/demo"
```

It is empty by default and the link is hidden when unset, because a customer's
own console should not carry our sales link unless they put it there.

## Getting in touch

**[Book a demo](https://nexus.ffx.ai/demo)** — deployment review, a walk
through the security posture, and help sizing the datastores for your traffic.
