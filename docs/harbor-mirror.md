# Harbor mirror — automation

## What runs on every release

When a tag matching `v*` is pushed, the `Release` workflow (`.github/workflows/release.yml`):

1. Builds and pushes the image to `ghcr.io/fun-fx/ffx_nexus:$TAG` (the public registry).
2. Mirrors the same image to `harbor.tail7d361a.ts.net/ffx/nexus:$TAG` and `:$MAJOR.$MINOR` (the production registry the Talos kubelet pulls from).

Step 2 uses `actions/runner`'s built-in docker engine with `docker login → pull → tag → push`. It is non-blocking: if the harbor credentials are not configured the step prints a `::warning::` and exits 0. Pods continue running the previous tag without interruption (we never break a release on harbor issues).

## One-time operator setup

1. **Create a Harbor robot account.**
   - Browse Harbor → `User settings` → `Robot accounts` → `New robot account`.
   - Name: e.g. `ci-mirror`.
   - Expiration: 1 year (Harbor prints the JWT-style secret **once** at creation — copy it now; re-issuing requires creating a new robot).
   - Resource scope: project `ffx`. Permission: `project admin` (push, pull, list tags).
   - The full robot username looks like `robot$ffx+ci-mirror`.

2. **Store the credentials as GitHub repository secrets.**
   - Repo → Settings → Secrets and variables → Actions → New repository secret.
     - `HARBOR_ROBOT_USERNAME` → the robot username from Harbor (e.g. `robot$ffx+ci-mirror`).
     - `HARBOR_ROBOT_SECRET` → the JWT-style secret printed once when the robot was created.

3. **Rotate on personnel change.**
   Harbor robot secrets are tied to a single robot account. When the operator who knows the secret leaves, delete and recreate the robot and update the GitHub secret.

## Verifying a fresh release

After pushing `vX.Y.Z`:

```bash
gh run list --workflow release.yml --limit 1
# pick the most recent run and follow the "Mirror ghcr → Harbor" step
```

Within ~30s of a successful mirror, `kubectl -n tenant-nexus get deploy,po -l app.kubernetes.io/name=nexus` will still show the previous image tag — **that is expected**, because `cd-prod.yml`'s chart image tag is bumped separately (next section).

## Helm chart image tag bump

Until a follow-up PR automates this:

```bash
# 1. Branch off main
git checkout main && git pull --ff-only
git checkout -b chore/bump-image-X.Y.Z
# 2. edit deploy/cozystack/values-prod.yaml:  tag: PREV  →  tag: X.Y.Z
$EDITOR deploy/cozystack/values-prod.yaml
git commit -am "chore(deploy): bump image to X.Y.Z"
gh pr create --title "chore(deploy): bump image to X.Y.Z" --body "Tracks Harbor image tag bump for vX.Y.Z."
gh pr merge --squash --delete-branch   # or hit the squash button
```

`cd-prod.yml` will then helm-upgrade automatically (~16 s) and the pod will roll to the new image.

## Manual fallback (operator laptop)

If the harbor robot credentials are missing or the mirror step fails, this one-shot sequence on your laptop does the same copy skip-and-copy-out-of-band. Then the Helm chart tag bump above finishes the rollout.

```bash
docker pull ghcr.io/fun-fx/ffx_nexus:X.Y.Z
docker tag  ghcr.io/fun-fx/ffx_nexus:X.Y.Z \
            harbor.tail7d361a.ts.net/ffx/nexus:X.Y.Z
docker login harbor.tail7d361a.ts.net   # operator-level credentials
docker push harbor.tail7d361a.ts.net/ffx/nexus:X.Y.Z
```

## Why two registries?

- `ghcr.io` — public, GitHub-built, untainted by internal credentials. Anyone external (CI, third-party devs) can pull here.
- `harbor.tail7d361a.ts.net` — Tailscale-only, connected to the production kubelet. Required because the Talos node certificates trust the Tailscale CA but **not** the public-internet CA chains for arbitrary hosts.

The kubelet ignores `ghcr.io` because the cluster-local imagePullSecrets only contain the Harbor credential; pinning the chart's `image.repository` to a ghcr URL would require every node to also carry a github-packages pull secret, which we don't want to ship.
