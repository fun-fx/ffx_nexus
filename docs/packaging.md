# Nexus packaging — open-core vs commercial

Nexus ships as a **single Go binary** with an optional React console. This document
defines what is open source today, what is planned as commercial, and how to
self-host each tier.

## Open source (this repository)

Available under the project license in the repository root.

| Area | Included |
| --- | --- |
| Gateway | OpenAI-compatible `/v1/chat/completions`, `/v1/models`, streaming |
| Providers | OpenAI, Anthropic, Gemini adapters |
| Observability | OTel-aligned traces, ClickHouse persistence, live WebSocket feed |
| Console | Session auth, virtual keys, credential CRUD, dashboard |
| Control plane | Encrypted provider credentials, RPM limits, monthly budgets |
| Evaluations | Heuristic evaluators, optional local SLM judge, optional Python sidecar |
| Routing | Quality-aware aliases, load balancing, policy gates |
| Guardrails | PII block/redact, JSON schema validation, self-correction |
| Semantic cache | Redis + embeddings endpoint |
| Deployment | Docker, Compose, Helm chart |

**Self-host minimum:** run the binary with env provider keys (zero-dependency mode).

**Self-host full stack:** Postgres + ClickHouse + Redis + the Helm chart under
[`deploy/helm/nexus`](../deploy/helm/nexus).

## Commercial (planned, not in OSS tree)

These are product boundaries for a future commercial offering. They are **not**
required to run or extend the OSS gateway.

| Feature | Notes |
| --- | --- |
| SSO / SAML / OIDC | Enterprise identity; OSS uses email/password sessions |
| RBAC | Fine-grained roles beyond org admin / member |
| Managed cloud | Hosted control plane, billing, org provisioning |
| Enterprise support | SLAs, dedicated onboarding |

**Already in OSS but often gated commercially elsewhere:** BYOK / per-user keys,
quality-aware routing, and eval-driven policy are part of the open-core
differentiator and remain in this repository.

## Build artifacts

| Artifact | Registry / path | Purpose |
| --- | --- | --- |
| `ffx/nexus` | `ghcr.io/fun-fx/ffx_nexus` | Gateway + console |
| `ffx/nexus-eval` | build from `eval-service/` | Optional eval sidecar |

## Versioning

- **App / chart version** — `deploy/helm/nexus/Chart.yaml` `appVersion` and `image.tag`
- **Database migrations** — applied on Nexus startup from `migrations/`
- **Breaking changes** — noted in release tags and `DESCRIPTION.md`

## Single-command self-hosting

**Kubernetes (Helm):** wire your own Postgres/ClickHouse/Redis, then:

```bash
helm upgrade --install nexus deploy/helm/nexus -f deploy/helm/nexus/values-full.yaml
```

Set `existingSecret` or populate `secrets.*` + dependency URLs before install.

Future refinements: Helm subchart dependencies for optional in-cluster databases.
