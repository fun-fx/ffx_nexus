# Enterprise model

Nexus is built around three things an enterprise deployment needs
to feel safe — and that a gateway with no operational discipline
does not provide by default.

## One gateway, every provider

OpenAI, Anthropic, and Gemini plug in with the same OpenAI-shaped
interface, so a single integration covers whichever combination
your team has decided to standardise on. Streaming responses
(`text/event-stream`), virtual keys per team, and per-call cost
tracking are baked into the gateway rather than tacked on. A team
that wants to keep its provider relationships but standardise its
internal surface gets a single endpoint to maintain, not ten.

## The key never reaches the user

Providers' API keys live in the platform, never returned to the
caller and never appearing in logs or audit exports. Operators,
teams, and individual users hold **virtual keys** (`nxs_live_…`)
that the platform resolves to a real credential at request time.
Rotating the platform key does not invalidate the virtual keys; a
team's budget and routing rules are attached to the virtual key,
so revoking one cuts cost from a single switch.

## Tenant isolation by construction

Each customer org sits on its own Postgres schema with its own
encryption keys, its own ClickHouse database, its own SSO realm (or
its own customer-managed IdP — Okta, Azure AD, Google Workspace,
Onelogin all plug in via the same OIDC path), and a tenant ID in
every trace span. Cross-tenant queries are impossible by
**construction**, not policy.

## SSO with the customer's IdP

A single sign-in path accepts any OIDC-compatible provider: your
own Keycloak if you self-host (common in EU finance and
government), your own Okta if your IT team standardised on it, or a
Nexus-managed realm if you'd rather not run one. Today the same
code path supports all three without a product split.

## Audit, by role

Every request ships with the actor and the org, and the
audit-log export surfaces that pairing as a queryable artefact.
The data is the kind a security review reads first: who did what,
when, on whose authority, with what resolution.
