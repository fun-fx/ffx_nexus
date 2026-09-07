# Development and release

## Running from source

```bash
# 1. (optional) start local datastores
docker compose -f deploy/docker-compose.yml up -d clickhouse

# 2. configure providers + trace store
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GEMINI_API_KEY=...
export NEXUS_CLICKHOUSE_URL="clickhouse://nexus:nexus@localhost:9000/nexus"

# 3. run the gateway + console
go run ./cmd/nexus
# → gateway on :8080  •  console on :8081

# 4. (dev) run the dashboard
cd web && npm install && npm run dev   # http://localhost:5173
```

The gateway boots with no API keys and no ClickHouse (traces are then
live-only). Set keys and URLs to enable providers and persistence.

The dashboard dev server on `:5173` hot-reloads and proxies `/api` to the
console on `:8081`. Skipping it is fine — the console already serves an SPA
embedded into the Go binary. Both URLs are fully functional, but the embedded
one is only as fresh as your last `npm --prefix web run build`, and
`web/dist` is committed for exactly that reason.

To work on the control plane without installing Postgres, `go run ./cmd/nexus
serve --local` starts one as a child process. It writes to `~/.nexus`;
`rm -rf ~/.nexus` resets it.

## Listen ports

| Path | Gateway | Console |
| --- | --- | --- |
| `npx @ffxnexus/nexus`, `install.sh`, `go run ./cmd/nexus` | `:8080` | `:8081` |
| Docker (`docker run ghcr.io/fun-fx/ffx_nexus`) | `:8080` | `:8081` |
| Helm chart | `:8080` | `:8081` |

Override with `NEXUS_GATEWAY_ADDR` / `NEXUS_CONSOLE_ADDR`, or with
`NEXUS_GATEWAY_PORT` / `NEXUS_CONSOLE_PORT` for `install.sh`.

## CI/CD

GitHub Actions workflows live in [`.github/workflows/`](../.github/workflows/).

| Workflow | Trigger | What it does |
| --- | --- | --- |
| **CI** | push / PR to `main` | `gofmt`, `go vet`, `go test -race`, Go build, `web/` TypeScript + Vite build |
| **Integration** | push / PR to `main`, manual | Docker Compose (Postgres, ClickHouse, Redis) + `./scripts/test_all.sh` |
| **Release** | tag `v*` (e.g. `v0.1.0`) | Build & push image to `ghcr.io/fun-fx/ffx_nexus` |

### Deploying

This repo publishes a container image to `ghcr.io/fun-fx/ffx_nexus` and ships a
generic Helm chart under [`deploy/helm/nexus`](../deploy/helm/nexus). Point the
chart at your own cluster, datastores, and provider policy — see the chart
`values.yaml` for the full surface. A minimal deploy:

```bash
helm upgrade --install nexus deploy/helm/nexus \
  --namespace nexus --create-namespace \
  --set image.tag=<APP_VERSION> \
  --set networkPolicy.profile=development \
  --set networkPolicy.mode=disabled
```

`image.tag` defaults to the chart's `appVersion`; pin it explicitly so the
image does not move on the next chart bump. The two `networkPolicy` flags are
what make this a *minimal* deploy — the chart defaults to a default-deny
policy set and refuses to install until every peer is named. See
[`docs/network-policy-prerequisites.md`](network-policy-prerequisites.md).

The chart's `version` and `appVersion` are kept in lock-step with the
gateway binary — bumping a gateway release is a single chart bump in
`deploy/helm/nexus/Chart.yaml` (currently `0.5.1` / `"0.5.1"`). Existing
deployments get the new gateway on the next `helm upgrade --reuse-values`
without touching Secrets.

> The specific on-prem production pipeline for the maintainers' cluster
> (Talos + Cozystack, in-cluster image build, prod values) lives in a separate
> private operations repo and is intentionally not part of this public release.

### Local parity

```bash
# Same checks as CI
gofmt -l .          # should print nothing
go vet ./...
go test -race ./...
go build ./cmd/nexus
cd web && npm ci && npm run build

# Same as Integration workflow
./scripts/test_all.sh
```

### Optional: full upstream tests in CI

Integration tests for rate limits (`429`) and budgets (`402`) need **no** provider keys.
For real Gemini/OpenAI completion, eval, and routing tests, add a repository secret:

- GitHub → **Settings → Secrets and variables → Actions**
- `GEMINI_API_KEY` (or `OPENAI_API_KEY`)

### Release a version

See [`CHANGELOG.md`](../CHANGELOG.md) for what changed in each release and
[`docs/release-notes/v0.1.0.md`](release-notes/v0.1.0.md) for the
current pilot handoff letter.

```bash
git tag v0.1.0
git push origin v0.1.0
```

A `v*` tag runs `.github/workflows/release.yml`, which publishes three things
from the same commit:

| Artefact | Where | Built by |
| --- | --- | --- |
| Container image | `ghcr.io/fun-fx/ffx_nexus:0.1.0` | `Dockerfile` |
| `darwin`/`linux` × `amd64`/`arm64` binaries plus `checksums.txt` | GitHub Release | `.goreleaser.yaml` |
| `@ffxnexus/nexus` | npm | `npx/` |

The npm package downloads its binary from the Release at run time, so the
`npm` job waits for the `binaries` job. Both installers construct the archive
name themselves; `scripts/test_release_naming.sh` runs on every PR to check
they still agree with what goreleaser produces.

To rehearse the whole build without a tag:

```bash
goreleaser release --snapshot --clean --skip=publish
./scripts/test_release_naming.sh
```

Run the image locally (datastores must be reachable separately):

```bash
docker run --rm -p 8080:8080 -p 8081:8081 \
  -e NEXUS_POSTGRES_URL=postgres://nexus:nexus@host.docker.internal:5433/nexus?sslmode=disable \
  -e NEXUS_CLICKHOUSE_URL=clickhouse://nexus:nexus@host.docker.internal:9000/nexus \
  -e NEXUS_REDIS_URL=redis://host.docker.internal:6379/0 \
  -e NEXUS_MASTER_KEY="$(openssl rand -hex 32)" \
  ghcr.io/fun-fx/ffx_nexus:0.1.0
```
