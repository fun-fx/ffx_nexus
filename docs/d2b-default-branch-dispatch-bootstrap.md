# Default-branch CNI dispatch bootstrap (d2b.34)

This document records the role of `.github/workflows/cni-nightly.yml`
on the **default branch** (`main`). It does NOT execute CNI
enforcement. Its single job is a `registration-only-refusal` guard
that unconditionally exits 78 (BSD sysexits.h EX_CONFIG) on EVERY
ref — main, feature branches (incl. `fix/d2b-cni-clean-tree-artifact`),
tags, and arbitrary SHA refs — regardless of the input values.

### Why the refusal is unconditional

GitHub's documentation supports `--ref BRANCH` for `workflow_dispatch`.
We cannot, prior to a real platform run, prove that every dispatch
against the candidate branch's `fix/d2b-cni-clean-tree-artifact` ref
definitively resolves its YAML to the candidate's full enforcement
workflow definition (rather than this default-branch bootstrap). Until
that's empi­ri­cal, the safe posture is to make this default-branch
definition reject every ref. Any green run from this path would be a
confusing operational artifact: an operator could mistake this run for
a real heavy CNI pass.

Exit 78 (BSD sysexits.h EX_CONFIG) is a deliberate, distinct refusal
code, separate from POSIX exit 1 and from the candidate enforcement
workflow's exit contract (10 source dirtiness, 11 artifact I/O, 14
image pipeline, 15 fixture invalid). Operators seeing a `failure`
conclusion from this workflow can correlate the `run_index` against
the bootstrap-semantic, not the candidate evidence.

GitHub Actions documentation states that a `workflow_dispatch`
event can only be triggered when the workflow is present in the
default branch:

> "To trigger the workflow_dispatch event, your workflow must be in
>  the default branch."
> — *GitHub Docs — Manually running a workflow*
> https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow

When `main` had no YAML at `.github/workflows/cni-nightly.yml`, the
repository's workflow registry did not advertise `workflow_dispatch`
for any ref, including the candidate branch
`fix/d2b-cni-clean-tree-artifact @ 7b18f4eb4c927167fb272c6fda8e24d6ae62b1be`.
`gh workflow run cni-nightly.yml --ref fix/...` returned HTTP 422:

```
could not create workflow dispatch event: HTTP 422
  Workflow does not have 'workflow_dispatch' trigger
```

The fix is to put a workflow file at the **same path on `main`** with
the **same input schema** as the candidate enforcement workflow but
with a guard job that refuses enforcement EVERY ref. Putting the
file there registers the trigger in the repository's workflow
registry without exposing `main` to any enforcement work.

This is **bootstrap-only**:

- The presence of `workflow_dispatch:` here does **not** mean
  `main` runs heavy CNI — nor does it mean any other ref runs
  heavy CNI from this file. The single job
  `registration-only-refusal` always exits 78 with a refusal
  message.
- The default-branch guard job is the **only job** in this file.
- Scheduler / push / pull_request triggers are **not** enabled.

## Inputs

Bootstrap accepts the same two inputs as the candidate enforcement
workflow:

| Name | Type | Required | Description |
|---|---|---|---|
| `recovery_pr_sha` | string | yes | Pinned recovery PR SHA. The candidate enforcement workflow uses this to pin checkout and identity capture. |
| `run_index` | string | yes | Run index: `1-of-3`, `2-of-3`, or `3-of-3`. Recorded into the candidate enforcement artifact name. |

The bootstrap **does not consume** these inputs for enforcement;
they are echoed to the run log so operators can confirm correlation.

## Operator procedure

To run the heavy CNI enforcement against an approved candidate SHA:

```bash
gh workflow run cni-nightly.yml \
  --repo fun-fx/ffx_nexus \
  --ref fix/d2b-cni-clean-tree-artifact \
  -f recovery_pr_sha=7b18f4eb4c927167fb272c6fda8e24d6ae62b1be \
  -f run_index=1-of-3
```

The candidate branch's `.github/workflows/cni-nightly.yml` at commit
`7b18f4eb4c927167fb272c6fda8e24d6ae62b1be` carries the full
enforcement body (Step A clean-tree, identity capture, kind cluster,
Cilium, scenarios, etc). When the registry advertises `workflow_dispatch`
from the default-branch bootstrap, dispatching against the candidate
branch ref compiles and runs that full body.

If someone mistakenly dispatches against `--ref main`, the bootstrap
prints the refusal message and exits with code 78 (refusal code is
distinct from run-index evidence; do NOT record this conclusion as
D-2b evidence).

## Permissions / Safety

| Field | Value |
|---|---|
| `permissions` | `contents: read` only |
| Triggers | `workflow_dispatch` only (no `schedule`, no `push`, no `pull_request`) |
| Concurrency group | `cni-policy-gate-bootstrap-${{ github.ref }}` |
| Concurrency cancel | `cancel-in-progress: true` (bootstrap-only) |
| Forbidden commands | `kind`, `kubectl`, `helm install`, `cilium`, `docker build`, `trivy`, `kubectl apply`, `scripts/install-nexus-test.sh`, fixture apply, scenario scripts, artifact upload, secret checkout |

## What this file does NOT do

- It does **not** modify `networkPolicy.enforcementAcknowledged`
  default or chart fail-closed guard.
- It does **not** modify fixture hardening, Go security, or the
  `cni-listener` CVE fix.
- It does **not** modify the candidate CI contracts:
  `scripts/test_clean_tree_artifact_regression.sh`,
  `deploy/helm/nexus/tests/*`, `scripts/test_helm_render.sh`,
  the Step A `fail_artifact_io` exit-10/11 contract.
- It does **not** start D-2c implementation.
- It does **not** push to any remote.

## Cross-references

- Source candidate: `7b18f4eb4c927167fb272c6fda8e24d6ae62b1be`
  (PR #271 — fix(d2b-cni): fail closed on clean-tree artifact I/O).
- Historical heavy run: `32827654462` (e656787, exit 10 from
  self-reference clean-tree defect; not enforcement evidence).
- Failed dispatch: `2026-08-25T09:39:54Z`,
  HTTP 422, run ID none.
