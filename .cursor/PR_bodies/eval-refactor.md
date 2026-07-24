# Eval surface — diagnostic + refactor options

Plan doc for the user feedback that arrived 2026-07-24. Purpose is
**diagnostic only** — no code changes until the user picks a path
forward. The four feedback points are mapped to the current code
shape first, then the options are listed.

---

## What the eval surface actually does today

### Backend (Go) — runtime config is a single global snapshot

| Layer | Path | Notes |
|---|---|---|
| Bootstrap | `internal/config/config.go` (`JudgeBaseURL`, `JudgeModel`, `JudgeAPIKey`, `EvalServiceURL`, `EvalServiceMetrics`, …) | Seeded by env vars at startup. |
| Worker | `internal/evals/worker.go` — holds `piiEnabled`, `completenessEnabled`, `judgeBaseURL`, `judgeModel`, `judgeAPIKey`, `remoteURL`, `remoteMetrics`, `judgeSampleRate`, `workerCount` | One global worker, one global config. |
| Heuristics | `internal/evals/heuristics.go` — `PIIEvaluator`, `CompletenessEvaluator` | Tunable on/off via `SetPIIEnabled` / `SetCompletenessEnabled`. No per-eval parameters — just an on/off. |
| SLM Judge | `internal/evals/judge.go` (uses `JudgeRuntimeConfig{BaseURL, Model, APIKey}`) | Single judge model wiring; toggle via `ConfigureJudges`. |
| Remote judge | `internal/evals/remote.go` (`RemoteEvaluator`, `RemoteConfig{BaseURL, Metrics, Timeout}`) | One remote endpoint, comma-separated metric list. |
| Console API | `internal/console/eval_config.go` — single `EvalConfigSnapshot` + `EvalConfigPatch` | Admin-only. **One config, one snapshot, no list.** |
| Runtime patch | `cmd/nexus/eval_runtime.go` — applies `EvalConfigPatch` to the worker | All settings land on the same worker; no per-eval scope. |

The snapshot the admin UI gets from `/api/eval/config`:

```text
eval.pii_enabled, eval.completeness_enabled, eval.sample_rate,
eval.workers, eval.judge.{enabled, base_url, model, api_key_set},
eval.remote.{enabled, url, metrics, timeout}
```

### Frontend (React) — one global card

`web/src/pages/Eval.tsx` renders a single `EvalRules` table (4 rows: PII,
Completeness, SLM judge, Remote eval) + a `WeightsCard` for routing weights.

There is no list of evals, no per-eval card, no per-eval edit. The page
reflects the single global snapshot exactly.

### Python sidecar — env-var driven, no API to mutate

`eval-service/app/config.py`:

```text
JUDGE_BASE_URL, JUDGE_MODEL, JUDGE_API_KEY,
EMBEDDINGS_BASE_URL, EMBEDDINGS_MODEL, EMBEDDINGS_API_KEY,
DEFAULT_METRICS, METRIC_THRESHOLD
```

All read-only at module-load time from env vars. The only Go→Python
pipe is `/evaluate` (a single function); the worker just forwards trace
+ active metrics to it. The Python service has no admin endpoint and
cannot be reconfigured at runtime through the console.

### Deploy

| File | What it sets |
|---|---|
| `deploy/helm/nexus/templates/configmap.yaml` line 65 | `NEXUS_JUDGE_BASE_URL`, `NEXUS_JUDGE_MODEL`, `NEXUS_JUDGE_API_KEY`, `NEXUS_EVAL_*`, `NEXUS_EVAL_SERVICE_URL|METRICS|TIMEOUT`. |
| `deploy/docker-compose.yml` lines 82/220 | Same set via docker-compose. |

---

## Mapping the four feedback points

| Feedback | Today | Limitation |
|---|---|---|
| "Is the eval functionality doing anything right now?" | Yes — heuristic PII/Completeness run on every successful trace; the remote Python eval fires on a fraction per `EvalSampleRate` via the `/evaluate` endpoint. **But** admin can only toggle four bools and three text boxes from the console; there are no per-eval-result displays and no per-eval history. | Hard to *see* what it is doing beyond percentage gauges in Grafana. The Eval page is essentially **a control panel**, not a results strip. |
| "Each eval should have a config." | Heuristics today are on/off only — no thresholds, no rule lists, no per-metric params. The Remote judge has one set of metrics applied to every score. | Admin cannot score "pii_leak pass if no SSN even if email is allowed" or "answer_relevancy threshold = 0.3 per region". |
| "Env vars should not be the source of truth." | `JudgeBaseURL`, `JudgeModel`, `JudgeAPIKey`, `EvalService*URL`, `EvalService*Metrics`, `Embeddings*` all live in env vars + Helm configmap; the console can override judge + remote at runtime via `Apply()`. Embeddings endpoint is **never reachable from the console** today — it’s pure env-var driven for the Python sidecar. | Re-deploys required to change embeddings endpoint. The remote Python service cannot be told to use a different provider at all. |
| "Keys should be from the org pool / user pool / inline." | The judge key is the operator-shipped `NEXUS_JUDGE_API_KEY`. It never points at a per-user or org-shared key. | A user/admin cannot plug in *their own* OpenAI key to run SLM judge without re-deploying. No key source selector. |
| "Evals for users vs orgs?" | Eval scores carry `UserID` + `RequestModel` (see `internal/evals/pg.go:32`) so per-user aggregation is possible in the data layer. But eval **configuration** (thresholds, providers, judging models) is global — there are no per-user or per-org eval profiles. | A user cannot sign up and say "judge my outputs with model X at threshold Y" the same way a router model can be shared (PRs #132/#133/#134). |

---

## What the refactor looks like

### 1. Per-eval configuration

Move from "four booleans on one worker" to **a list of named eval
profiles**, each with its own parameter set:

```json
[
  {
    "id": "pii_strict",
    "kind": "heuristic_pii",
    "scope": { "level": "user", "owner_id": "u-alice" },
    "params": { "block_on_hits": ["ssn","card"] }
  },
  {
    "id": "answer_relevancy_oa",
    "kind": "remote_eval",
    "scope": { "level": "org", "org_id": "default" },
    "params": {
      "slug": "answer_relevancy",
      "endpoint": { "base_url": "https://api.openai.com/v1",
                    "model": "gpt-4o",
                    "key_source": "user",
                    "key_id": "cred_xyz" }
    },
    "threshold": 0.6,
    "sample_rate": 0.2
  }
]
```

The Go types would migrate from `RuntimeState` (struct) to
`EvalProfileSet` (slice), and `Worker` would iterate active profiles
per trace instead of "if piiOn → push PIIEvaluator".

### 2. Dynamic evals via the console

Any per-eval parameter available today via env var becomes editable
through `PATCH /api/me/eval/profiles` and `POST /api/me/eval/profiles`
(non-destructive). Required pieces:

- JSON schema for each `kind` (heuristic_pii, heuristic_completeness,
  slm_judge, remote_eval, embeddings_judge …) backed by a registry on
  the Go side so adding a new kind does not require patching the UI.
- `EvalEndpoint` struct that reuses the registry's `ScopeHint`
  machinery: base URL, model, key-source enum (`user | org | inline`),
  owner_id. Inline credentials encrypted same way as `ProviderCredential`.
- Babel for forward-compat: env var values become the **seed** for the
  implicit `default` profile that the bootstrap creates if no profile
  exists yet.

### 3. Key sourcing

- **Org pool**: query the existing `provider_credentials` table where
  `user_id IS NULL`. Console already has
  `internal/core/store.go::ListCredentials`. Filter by allowed
  `provider` per eval kind.
- **User pool**: same table where `user_id = caller.ID`.
- **Inline**: admin opens a secret drawer (same UX as the keys page
  via PR #134); encrypted with the existing `crypto` package at rest
  and held in-process for the worker's lifetime.

### 4. User / org scope

Each profile gets `scope: "user" | "org"` (admin sees both as in
PR #133). Visibility rules:

- **org profiles** are visible/editable by admins; any member can see
  their results aggregated.
- **user profiles** are visible/editable only by the owner; admin
  always sees them for audit.
- A consumer (gateway request path) inherits the **most specific
  profile that applies to the caller's user/org**, falling back to org
  if no user profile exists, falling back to the global default. This
  mirrors how router models already resolve with PR #132/#133.

---

## Decision 2026-07-24

The user picked **D — Full rebuild: per-eval refactor + Python sidecar
override wire**. Schedule is the same 3-PR cut used by the router-scope
series so review surface area is bounded.

| PR | Scope |
|---|---|
| [#135] Eval profiles backend | Replace worker global config with `EvalProfileSet` (per-profile kind, scope, params, endpoint, threshold, sample_rate, key source). New `eval_profiles` ClickHouse/Postgres table. Console endpoints `GET/POST/PATCH/DELETE /api/me/eval/profiles`. Env vars become bootstrap seed. ~1.5 days backend + tests. |
| [#136] `/evaluate` wire override | Go forwards per-call judge/embeddings/threshold override as `EvalEndpoint` payload. Python (`eval-service/app/judge.py` + `schemas.py`) accepts overrides per request, env fallback. Caller-filter (scope), provider_credentials key resolution, grace: inline secrets encrypted/in-process. ~1.5 days across both languages. |
| [#137] Frontend profile CRUD | Eval.tsx becomes a list of `EvalProfileCard` (one per profile). Modal `(name, kind, scope level, owner/key ref, endpoint, sample rate, threshold)` with key-source drawer. Per-profile results strip linking to existing `/api/me/quality`. ~1 day, with vitest + Playwright a11y. |

### Why three PRs and not one big-bang

- **Reviewability**: each PR touches one language first (Go → Go+Python → Web). The reviewer only has to read one side at a time.
- **Rollback**: profile schema swap (PR #135) is reversible if the wire change (PR #136) turns out to be wrong. A single-PR refactor would force a full revert.
- **Existing env-var contract**: PR #135 keeps the current env-var names as bootstrap seed values; PRs #136/#137 layer new behaviour on top. No deploy surprises.

## Open questions for the user (D scope)

1. **Sample-rate per profile** vs **per-org**? Today sample_rate is
   global. PR #135 should keep it per-profile (the per-org view is
   derivable as sum of union) — confirm or override.
2. **Inline key storage**: should inline-entered keys stay in memory
   (lost on restart, simple) or persist encrypted in a new
   `eval_credentials` table (lost-on-reroll, survives restart,
   uniform with `provider_credentials`)? Default: persist + TTL.
3. **Org-scoped profile editing**: members can see org profiles in
   read-only mode + propose changes; admin approves. Or hide entirely
   from members? Default: read-only.
4. **Heuristic PII configurable**: today the four regex patterns are
   hard-coded. Do you want admins to add/edit/delete regex entries per
   org? Default: leave hard-coded for now, expose regex ids only.
