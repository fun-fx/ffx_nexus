# Enterprise model & v2 roadmap

Status: **DRAFT — for review**
Author: Nexus team
Scope: how Nexus evolves from strict-BYOK self-hosted to multi-tenant enterprise SaaS.

---

## 1. Where we are today (v1, shipped)

| Capability | Implementation |
|---|---|
| Gateway | OpenAI / Anthropic / Gemini passthrough, SSE streaming, virtual keys |
| Auth | Email + password; **SSO via OIDC** (Keycloak tested in prod) |
| LLM keys | **Strict BYOK** — each user registers their own provider key |
| Multi-tenant | Single org (`default`); Postgres row-level isolation |
| Observability | ClickHouse + OpenTelemetry, per-user quality / spend rollups |
| Eval | Async heuristic + SLM judge (Ollama) + Python sidecar (DeepEval / RAGAS) |
| Hosting | Self-hosted Helm chart, prod on Talos + Cozystack at `*.<tailnet>.ts.net` |

**What works for**: small teams (1–20 users) who already have provider accounts and are happy to plug their own keys in.

**What doesn't work for**: enterprise customers (50+ users) where the IT / security team must own the provider keys.

---

## 2. The enterprise model (v2)

### 2.1 What enterprise customers actually need

From a typical security review:

> "We need SSO via Okta / Azure AD. We need audit log export. We need a single org-level LLM key managed by IT, with per-department virtual keys, per-user spend rollups, and the ability to revoke any user's access in 5 seconds."

The Robert-feedback from v1 planning (paraphrased):

> "How can you do enterprise with BYOK? In enterprise I can use my company keys (or a set of keys) and never leak them to the team. The team just uses the virtual keys assigned to each account."

Both reduce to: **org-level LLM key management** + **virtual keys as the only thing the user sees**.

### 2.2 Platform credentials (org-level provider keys)

```
[LLM Provider] ── key ──→ [Customer IT team]
                                │
                                │  1 registration via Nexus admin UI
                                ▼
                         [Nexus Platform]
                                │
                                │  org_id, name, allowed_users, allowed_models
                                ▼
                          [Virtual key pool]
                                │
                  ┌─────────────┼─────────────┐
                  ▼             ▼             ▼
              vkey-eng      vkey-sales    vkey-finance
              (10 users)    (5 users)     (3 users)
                  │             │             │
                Engineers    AEs          Finance team
                (SSO login)  (SSO login)  (SSO login)
```

**Key properties**:
- The real LLM key is **never returned to the user**, ever — not in API responses, not in logs, not in audit exports.
- The user only ever holds a **virtual key** (`nxs_live_...`) that resolves to a platform credential at request time.
- Org admins can rotate the platform credential without invalidating virtual keys.
- Per-department budgets roll up to org-level spend.

### 2.3 Multi-tenant data planes

Each customer org gets:

| Resource | Isolation |
|---|---|
| Postgres | Separate schema, separate encryption keys (KMS-managed) |
| ClickHouse | Separate database |
| Redis | Separate key prefix |
| SSO | Separate Keycloak realm (or external IdP, per customer) |
| Logs | Tenant ID in every trace span |

Cross-tenant queries are impossible by construction.

### 2.4 SSO: customer IdP, not just ours

Today v1's Keycloak is a **stand-in for any OIDC provider**. The same code path supports:

- **Customer-managed IdP**: Okta, Azure AD, Google Workspace, OneLogin
- **Self-hosted Keycloak**: customer runs their own (common in EU finance / gov)
- **Nexus-hosted Keycloak**: we run a managed realm per customer (`kc.nexus.ffx.ai/realms/customer-a`)

The customer's choice. We support all three.

### 2.5 Audit + compliance

| Capability | Target |
|---|---|
| Per-request trace with actor + org | ✅ v1 |
| Audit log export (S3, GCS, ClickHouse external) | v2 |
| SOC 2 Type II | v2 |
| HIPAA BAA | v2 (with on-prem) |
| ISO 27001 | v2 |
| EU AI Act compliance | v2 |

---

## 3. Hosting model (v2)

| Tier | Where it runs | How customers access |
|---|---|---|
| **OSS** (free) | Customer's own infra | Self-hosted, customer controls everything |
| **Cloud (multi-tenant)** | Our infra, our domain | `nexus.ffx.ai`, shared ClickHouse with tenant ID |
| **Cloud (dedicated)** | Our infra, separate DB | `<customer>.nexus.ffx.ai`, fully isolated |
| **On-prem (Enterprise)** | Customer's VPC, our Helm chart | Customer's ingress, our support |

The **marketing site** (`nexus.ffx.ai`) ships in v1.1 with the Bifrost-style hero, pricing, and docs.

---

## 4. v2 work breakdown

| # | Work | Estimated |
|---|---|---|
| 1 | Org-level provider keys (CRUD, KMS encryption, per-org policy) | 2 weeks |
| 2 | Virtual key → org credential resolution at request time | 1 week |
| 3 | Customer IdP: SAML + OIDC generic (drop Keycloak as default) | 1 week |
| 4 | Per-org ClickHouse database + tenant_id propagation | 1 week |
| 5 | Audit log export (S3 / GCS / OTLP) | 1 week |
| 6 | Billing: usage rollup per org / per virtual key → Stripe | 2 weeks |
| 7 | Dedicated-tenant Cloud tier (`<customer>.nexus.ffx.ai` per CF Pages project) | 1 week |
| 8 | On-prem Helm chart polish (air-gapped install, HA Postgres, backup runbook) | 2 weeks |
| 9 | SOC 2 Type II: pen test, evidence collection, audit | 3-6 months (in parallel) |

---

## 5. v1.1 (between v1 and v2)

What we ship in the next 4 weeks to bridge:

1. **Public marketing site** at `nexus.ffx.ai` (Bifrost-style, this PR).
2. **Public app entry point** at `app.nexus.ffx.ai` — same prod stack, public ingress.
3. **Improved onboarding** — guided BYOK flow, "test your key" button.
4. **Audit log basic** — `GET /api/audit?since=...` with actor, target, outcome.
5. **Helm chart polish** — better defaults, single-node install profile.

---

## 6. Open questions for Robert

1. **Pricing unit** — usage-based (per 1k traced requests) or flat-rate per org?
2. **Sales motion** — self-serve signup, or sales-led? Different funnel for OSS vs. Cloud.
3. **On-prem support** — do we offer it? Cost structure for air-gapped deployment?
4. **Dedicated tier** — is `<customer>.nexus.ffx.ai` a real ask, or is shared tenancy OK for SMB?
5. **Compliance timeline** — when does SOC 2 matter for closing enterprise deals?
