# Mirror ghcr image to Harbor (one-shot)

The CD pipeline (`cd-prod.yml`) pulls from `harbor.<tailnet>.ts.net/ffx/nexus`,
but the release workflow (`release.yml`) builds and pushes to `ghcr.io`.
Until we automate harbor mirroring in CI, the operator has to copy the new tag
across after every release.

## One-shot commands

When a `vX.Y.Z` tag is pushed, run this on a machine with docker credentials
for both registries (e.g. your laptop with the operator profile):

```bash
# Pull the freshly-built ghcr image.
docker pull ghcr.io/fun-fx/ffx_nexus:X.Y.Z

# Re-tag for harbor.
docker tag  ghcr.io/fun-fx/ffx_nexus:X.Y.Z \
            harbor.<tailnet>.ts.net/ffx/nexus:X.Y.Z

# Push to harbor. (kubelet pulls from here.)
docker login harbor.<tailnet>.ts.net
docker push harbor.<tailnet>.ts.net/ffx/nexus:X.Y.Z
```

`X.Y.Z` here is the released version (currently `v0.3.6`). The image is
`linux/amd64` (the cluster is amd64) and the `linux/arm64` ship-on-laptop
image is **not** what the kubelet can run.

## After pushing

Bump the image tag in `deploy/cozystack/values-prod.yaml` and push to main;
the CD workflow will then helm-upgrade the deployment:

```bash
# 1) edit deploy/cozystack/values-prod.yaml: tag: 0.3.5  →  0.3.6
git checkout -b chore/bump-image
# edit the file
git commit -am "chore(deploy): bump image to 0.3.6 (v0.3.6 — embeddings+responses)"
gh pr create --title "chore(deploy): bump image to 0.3.6"
# 2) merge → cd-prod.yml triggers → pod rolls
```

The roll is fast (~16 s) because `cd-prod.yml` already runs in-cluster smoke
probes (PR #41) and `kubectl` is pre-installed (PR #40).

## Long-term plan

Add a `harbor-mirror` step to `.github/workflows/release.yml` (after the
`docker/build-push-action@v6` step) using `docker pull ghcr.io/... &&
docker push harbor.<tailnet>.ts.net/...` with a Harbor robot account stored
as `secrets.HARBOR_ROBOT_TOKEN`. Tracked separately; not blocking v0.3.6
release validation.
