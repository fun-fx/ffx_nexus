# Contributing to Nexus

Thanks for taking an interest in Nexus. This document covers the practical
mechanics of opening issues, sending pull requests, and keeping changes
shippable.

## License

Nexus is dual-licensed:

- **Gateway (Go) and core libraries**: Apache License 2.0 — see [`LICENSE`](LICENSE).
- **Console dashboard (web/)**: MIT — see [`LICENSE-MIT`](LICENSE-MIT).

By submitting a contribution, you agree that your work will be licensed under
the same terms as the file(s) it touches. The full license texts are the
authoritative source; this document is not a substitute.

## Code of conduct

Be welcoming, assume good faith, and keep the discussion focused on the
problem being solved. Constructive critique is encouraged; personal attacks
are not.

## Reporting issues

- **Bugs** — describe the exact reproduction, including the Nexus version
  (`git rev-parse v0.x.y` or `git describe`), the route (`/v1/chat/completions`,
  `/v1/models`, console API), the provider involved, the request body, the
  response (truncate anything sensitive), and any trace row IDs.
- **Security** — please **do not** open a public issue for security bugs.
  Use the GitHub *Report a vulnerability* tab on the repository. Encrypt the
  report if the issue is sensitive.

## Development setup

```bash
go test ./...
cd web && npm ci && npm run build
```

The Go side uses the standard [`go test`](https://pkg.go.dev/cmd/go#hdr-Test_packages)
runner. Several suites (`scripts/test_*.sh`) shell out to a hermetic harness
that spins up a fake Python `eval-service` (`scripts/lib/fake_eval_service.py`)
on a local port and runs the Go binary against it — there is no external
network dependency at test time.

## Pull requests

1. Branch off `main`:
   ```bash
   git checkout main
   git checkout -b feat/<short-description>
   ```
   Types: `feat/`, `fix/`, `chore/`, `docs/`, `refactor/`, `test/`.

2. Keep commits focused. One logical change per commit. Avoid mixing
   formatting-only commits with behavior changes.

3. Before opening the PR, locally confirm:
   - `gofmt -l ./...` returns empty (or run `gofmt -w .`).
   - `go test ./...` passes.
   - `cd web && npm ci && tsc --noEmit && npm run build` passes.
   - For backend behavior change: `bash scripts/test_all.sh` passes.

4. Push your branch and open a PR **against `main`**. The PR description
   should call out:
   - the user-visible behavior change
   - the smallest reproduction (curl, model id, headers)
   - linked issues / context
   - any rollout or flag considerations (`NEXUS_*` env, helm values, ops
     repo `cd-prod.yml` pin)

5. **CI is mandatory.** `CI` (Go + web dashboard), `E2E` (synthesized
   Postgres + ClickHouse + Redis + fake eval-service), `Eval regression
   gate`, and `Eval service (Python)` all run on every PR. Merges happen
   only when these pass.

6. Releases are cut on annotated tags. `Release` workflow (`.github/workflows/release.yml`)
   pushes the image to `ghcr.io/fun-fx/ffx_nexus:<tag>` on `v*` tag push.
   Prod promotion lives in `fun-fx/ffx_nexus_ops` (private) — see
   `docs/packaging.md` for the OSS/commercial boundary.

## Style

- **Go** — standard `gofmt`. Internal packages live under `internal/`;
  public re-usable types under `pkg/`. Avoid naked package-init side effects.
- **React / TypeScript** — keep types in `web/src/api.ts` next to the
  endpoint; components stay skinny. Datalist-driven autocompletion is
  the standard way to expose dynamic catalogs (e.g. `user/<provider>/<model>`).
- **Comments** — in English. Describe *why*, not what; the code shows what.
- **Console server** — uses the in-tree HTTP router (`internal/console`).
  Endpoints follow `/api/<area>/<verb>`.

## Database changes

Schema changes live under `migrations/postgres/` and are applied at startup
by `cmd/nexus/main.go`. Additive migrations only (`ALTER TABLE ... ADD
COLUMN IF NOT EXISTS ...`) — destructive changes (drop column, drop table,
rename column) require a deprecation pass through a prior minor release.

## Adding a provider

For 1st-party providers (where you own the adapter code), extend the
existing providers registry under `internal/gateway/providers/<name>.go`
and the corresponding catalog list. **For most cases, you do not need a
new Go adapter** — register an OpenAI-compatible upstream through the
console's *Account → My provider keys → Custom (OpenAI-compatible)…* and
Nexus will wrap it as a `UserCompat` adapter automatically. This is the
intended path for OpenRouter, Together, Fireworks, private gateways, etc.

## Documentation

- `docs/` is the canonical home for design docs.
- `README.md` is the entry point for newcomers — keep it concrete, with
  runnable commands whenever possible.
- `CHANGELOG.md` follows Keep a Changelog. Every release gets a top-level
  `## [vX.Y.Z]` section. The release commit (cut by maintainers) is the
  last commit that touches `CHANGELOG.md` before the tag.
