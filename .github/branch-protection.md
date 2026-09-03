# Branch protection on `main`

This file records what is configured, and why the obvious configuration is
the wrong one. Read it before adding a check to the required list.

Protection is live on `main`. Query it rather than trusting this file if the
two disagree:

```bash
gh api repos/fun-fx/ffx_nexus/branches/main/protection \
  --jq '.required_status_checks.contexts'
```

## Required checks

Ten, all from `ci.yml` and `integration.yml`:

| Check | Workflow |
| --- | --- |
| `Go` | `ci.yml` |
| `Helm chart` | `ci.yml` |
| `Schema migrations (real Postgres)` | `ci.yml` |
| `Eval regression gate` | `ci.yml` |
| `Eval service (Python)` | `ci.yml` |
| `Secret and image scanning` | `ci.yml` |
| `Web dashboard` | `ci.yml` |
| `Policy contracts (offline)` | `ci.yml` |
| `E2E (full suite)` | `integration.yml` |
| `Bench live contracts (PG/CH)` | `integration.yml` |

Also set: force pushes and branch deletion are blocked, and conversation
resolution is required. Review approval is **not** required, so a maintainer
can merge their own pull request once the checks pass.

`enforce_admins` is **off**, so an administrator can override. That is a
deliberate escape hatch for the case where a required check breaks in a way
that cannot be fixed through a pull request. Turning it on is one call:

```bash
gh api -X PATCH repos/fun-fx/ffx_nexus/branches/main/protection/enforce_admins
```

## Why the CNI checks are not required

`CNI lightweight gate (always)` and `CNI enforcement gate (3× runs on pinned
SHA)` are deliberately excluded, and this is the part most likely to be
"fixed" by someone who has not hit the failure mode.

A required status check must report a conclusion on **every** pull request.
Both CNI jobs live in `cni-nightly.yml` behind a `paths:` filter, so on a
pull request that touches neither the chart nor the policy code the job never
starts, never reports, and the pull request waits for a check that will never
arrive. It does not fail — it hangs, with no way to proceed except an
administrator override. The name "(always)" refers to it always producing a
conclusion *when it runs*, not to it running on every pull request.

The enforcement gate additionally cannot be a merge gate by construction: it
runs against a SHA the operator pins by hand, three times, and its evidence
is those three runs. A `pull_request` trigger has no pinned SHA to attest to.

`Policy contracts (offline)` exists for this reason. It carries the hermetic
subset of the same contracts — chart fail-closed behaviour, artifact routing,
trigger surface — with no cluster, so it can run unconditionally on every
pull request and therefore can be required.

## What this does and does not prove

A required check proves the check passed. It does not prove the check is the
gate anyone intended, and this repository has produced both failure modes
worth naming:

- **A green check that validated nothing.** The `kubeconform` step piped
  `helm template` into the validator without `pipefail`. Neither example
  values file rendered, so kubeconform read empty input, reported "0 resource
  found parsing stdin", and exited 0. It was green for as long as it existed.
- **A suite that was never invoked.** The `web` job ran `npm build` but not
  `npm test`, so 183 console tests went unrun and rotted until 125 of them
  failed. Eight offline policy harnesses had the same problem: committed,
  referenced only in path filters, executed by nothing.

Both were invisible from the pull request page, because a check that does not
run and a check with nothing to say look identical there. So when adding a
gate, verify it fails on a deliberately broken input before trusting it.

## Adding a required check

1. Merge the workflow change first, so the check has run at least once on
   `main` and its exact name is known.
2. Confirm it reports on a pull request that touches nothing related to it.
   If it does not, it cannot be required — see above.
3. Add it by name:

```bash
gh api -X PATCH repos/fun-fx/ffx_nexus/branches/main/protection/required_status_checks \
  -f 'contexts[]=<exact check name>'
```

The name is the job's `name:` value, not its key under `jobs:`. GitHub
matches it literally, so renaming a job removes the required check and
silently unblocks merges rather than failing them.
