# GitHub branch protection for CNI gate

> Why this file exists: writing a workflow
> `.github/workflows/cni-nightly.yml` with the
> right job names is not enough. GitHub's
> branch-protection "required status checks"
> only enforce the names declared in
> **Settings → Branches → Branch protection
> rules → Require status checks to pass before
> merging → Search for status checks** — they
> do NOT auto-mirror job names from a workflow
> file. Without that GUI step, a workflow can be
> deleted or renamed and merge remains
> unblocked.
>
> This file is the operator-facing runbook.
> It also lists the **exact** job names so the
> run-book matches the workflow file textually
> and a future grep will catch drift.

## Required check names (textual match)

The following required status checks must be
configured under branch protection:

| Required check name (literal)            | Workflow file              | Trigger                                              | What it proves                                                                                                                         |
| ---------------------------------------- | -------------------------- | ----------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `cni-policy-gate / cni-policy-required`  | `cni-nightly.yml`          | pull_request touching networkpolicy / template / contract | The full 13-scenario suite passed on multi-node Kind+Cilium and the upgrade rehearsal produced a green atomic rollback                 |
| `cni-policy-gate / cni-lightweight-gate` | `cni-nightly.yml`          | always (every PR)                                     | NetworkPolicy.yaml renders without error and the static label conformance (`TestFixtureLabelsConformToChart`) passes                  |

### Naming rule

GitHub's required-check matcher is **case-sensitive
substring match** on the full job name:
`<workflow-name> / <job-id>`. The workflow name is
the top-level `name:` key in the YAML, the job-id
is the top-level `id:` key under `jobs:`. We use
`name: cni-policy-required` and
`jobs.<X>.name: cni-enforcement` so the literal
status check string is `cni-policy-required / cni-enforcement`.

### Path triggers

We condition the heavy gate on a path filter that
mirrors the user's explicit ask: NP template and
helpers, files that change feature-to-destination
mapping (`features.*`, `networkPolicy.*`,
`internal/netpolicy/*`, `internal/urlpolicy/*`),
gateway/worker/migration port/Service/ServiceMonitor,
the CNI test scripts/fixtures/workflow.

```
paths:
  - 'deploy/helm/nexus/templates/networkpolicy.yaml'
  - 'deploy/helm/nexus/templates/_helpers.tpl'
  - 'deploy/helm/nexus/values.yaml'
  - 'deploy/helm/nexus/values.schema.json'
  - 'deploy/helm/nexus/templates/deployment*.yaml'
  - 'deploy/helm/nexus/templates/service*.yaml'
  - 'deploy/helm/nexus/templates/servicemonitor.yaml'
  - 'deploy/helm/nexus/templates/migration-job.yaml'
  - 'deploy/helm/nexus/templates/pre-install-validation.yaml'
  - 'internal/netpolicy/**'
  - 'internal/urlpolicy/**'
  - 'internal/featureflag/**'
  - 'internal/depcontract/**'
  - '.github/workflows/cni-nightly.yml'
  - 'scripts/d2b-twelve-scenarios.sh'
  - 'scripts/install-nexus-test.sh'
  - 'scripts/test-cluster-up.sh'
  - 'scripts/test-cluster-down.sh'
  - 'scripts/test-upgrade-rehearsal-up.sh'
  - 'scripts/wait-cilium-endpoints.sh'
  - 'scripts/fixtures/integrationcni/**'
  - 'docs/phase-d2b-*.md'
  - 'docs/cni-*.md'
  - 'docs/network-*.md'
  - 'docs/d2b-*.md'
```

### Concurrency

`concurrency: { group: cni-gate-${{ github.event.pull_request.head.ref || github.ref }}, cancel-in-progress: true }`.
This ensures the latest commit on a branch is the
one whose result is unblocked; older runs are
cancelled and excluded from the merge decision.

### Branch protection setup steps

1. https://github.com/<org>/<repo>/settings/branches
2. Add a branch protection rule on `main`:
   - Require a pull request before merging: **enabled**
   - Require approvals: **1+**
   - Dismiss stale pull request approvals: **enabled**
   - Require status checks to pass before merging: **enabled**
   - **Search for status checks** → add each literal name listed in the table above
   - Require linear history: **enabled**
   - Allow force pushes: **disabled**
3. Save.
4. Create a draft PR with a temporary commit that
   only modifies a path under the filter to confirm
   the heavy job actually runs. Cancel and close it.

### Verifying after a workflow rename

If a workflow file is renamed or the `name:` key
is changed, the previously-required check name no
longer exists and merging becomes blocked. Mitigation:

- The literal required check name is documented
  in this file. Maintainers reference this file
  before any workflow rename.
- A static test does NOT substitute for this GUI
  step. GitHub does not auto-detect job names.

### What protected CI does NOT prove

- The branch-protection rule only enforces that the
  **named** check passed. It does NOT verify that
  the named check is the gate the operator
  intended. A malicious or sloppy rename can on
  the surface keep the rule green while the
  intended cluster test no longer runs.
- We mitigate this with **path-triggered PR
  comments**: every policy-changing PR is required
  to link to a `cni-policy-required / cni-enforcement`
  run URL in the merge box. Reviewers must visually
  confirm the run actually executed. This is
  enforced by the PR template.
