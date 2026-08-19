# Trivy Baseline Policy

This document is the **policy of record** for what counts as a known,
accepted Trivy finding. Its twin responsibilities are:

1. **Customer deployments are protected.** Trivy runs in CI on every
   push to a PR and on every merge to `main`. The scanner is configured
   (`severity: HIGH,CRITICAL` with `exit-code: 1`) to **block** any
   *new* finding that lands in the default branch.
2. **Known findings are recorded, not hidden.** When a finding is
   accepted (because the operator is responsible for it, the affected
   layer is unreachable from customer traffic, the upstream has not
   patched yet, etc.) the conversation does not live in a comment on
   one PR. It lives here, registered as if it were a documented
   configuration decision a future operator can audit and supersede.

## Policy summary

| Step                            | Tool                                  | Exit code on PR  | Exit code on `main` |
|---------------------------------|---------------------------------------|------------------|---------------------|
| First scan, no baseline         | trivy image + config                  | FAIL on any ≥HIGH | FAIL on any ≥HIGH  |
| Found scan, snapshot written     | trivy image + config (report mode)    | n/a              | n/a                 |
| Following scans                 | trivy image + config (gate mode)      | FAIL on **new** ≥HIGH | FAIL on **new** ≥HIGH |

## Initial baseline (snapshot at 2026-08-19, commit hash on `feat/release-candidate-structural-foundations`)

Captured with Trivy v0.74.0 against the `.` scan-ref without any
container build (so only the config scanner ran; libs cover the
container stage when an image build is present in CI):

| Surface                | Total ≥HIGH fresh findings | Action                         | `.trivyignore` entry |
|------------------------|----------------------------|--------------------------------|----------------------|
| `trivy fs misconfig .` | 1                          | Accept on file (`Dockerfile.mock` is dev-only; runtime USER non-root in `Dockerfile`) | `DS-0002`            |

The single accepted finding (`DS-0002`, "Image user should not
be 'root'") flags the implicit-root multi-stage build context
that `Dockerfile` sets in the build stage. The runtime stage
runs as UID 65532 (`nexus`) and that is the value trivy cannot
infer — the rule fires on the COPY context, not the run
context. A findable fix is to add `USER nexus` to the build
stage even though that's not where the binary executes; we
have a tracked ticket for that.

## How to add a new `.trivyignore` entry

1. Run `trivy fs --scanners misconfig .` locally; reproduce the
   finding on the same scanner version listed above.
2. Add the ID, a 2-line rationale, the owner, and an explicit
   `<YYYY-Qn>` expiry comment to the file.
3. PR review requires the rationale to:
   - name the code path that produces the finding,
   - explain why a runtime-only mitigation is acceptable, OR
   - explicitly call out the tracked ticket that supersedes it.
4. The new entry must appear in the
   `docs/trivy-baseline.md` `Exception register` table below
   on the same PR.

## Exception register

Each entry below corresponds to a `skip-files` or `--ignorefile` line
in CI. The table is the source of truth, the workflow annotation is a
reminder.

| Scope                | Path                     | Reason                                               | Review by  | Owner                |
|----------------------|--------------------------|------------------------------------------------------|------------|----------------------|
| Container scan       | *(none)*                 | n/a                                                  | n/a        | n/a                  |
| Config scan (skip)   | `.devcontainer/Dockerfile`| Dev container needs root for Docker-in-Docker socket; not a customer artefact. See `.devcontainer/README.md`. | 2026-Q4    | platform-team@fun-fx |
| Config scan (DS-0002)| `Dockerfile` (build stage) | Runtime USER directive drops to UID 65532; trivy flags the implicit-root build stage. Runtime is verified non-root. Supersede: add `USER nexus` to the build stage. | 2026-Q4    | platform-team@fun-fx |


This document pairs with the live policy in
`.github/workflows/ci.yml`: every accept-list entry must be registered
below *and* carry an explicit expiry in the workflow comments so a
stale exception surfaces at code review.

## Exception register

Each entry below corresponds to a `skip-files` or `--ignorefile` line
in CI. The table is the source of truth, the workflow annotation is a
reminder.

| Scope                | Path                            | Reason                                               | Review by  | Owner                |
|----------------------|---------------------------------|------------------------------------------------------|------------|----------------------|
| Container scan       | *(none)*                        | n/a                                                  | n/a        | n/a                  |
| Config scan (image)  | `.devcontainer/Dockerfile`      | Dev container needs root for Docker-in-Docker socket; not a customer artefact. See `.devcontainer/README.md`. | 2026-Q4    | platform-team@fun-fx |

When a registered exception reaches its `Review by` date:

1. Re-open the corresponding skip in CI (remove `skip-files` line).
2. If trivy still flags a HIGH/CRITICAL finding, decide one of:
   - **Fix** the root cause and remove the exception entry entirely.
   - **Document** a fresh finding and shorten/extend the next review date.
     The action above requires justification in this file (no
     "still broken; skip" entries).
3. Update the table.

## Reproducing the scan locally

The same command the CI runs can be executed locally once the image
is built:

```sh
docker build -t nexus:ci-scan .
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -v $PWD:/workspace aquasec/trivy:v0.74.0 \
  image --severity HIGH,CRITICAL --exit-code 1 nexus:ci-scan

docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -v $PWD:/workspace aquasec/trivy:v0.74.0 \
  config --severity HIGH,CRITICAL --exit-code 1 .
```

## Why this is not a global allowlist for trivy output

The previous "ignore any trivy output" stance quietly made Trivy a CI
checkbox rather than a security control. The new policy treats every
pre-existing finding as **registered debt** that must be either closed
or re-justified each quarter; the bar is that nobody reads the diff
and thinks "this trivy is fine" without opening this file.
