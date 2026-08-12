# Code health cleanup — CI fix follow-up

After the initial commit (518264c) added a `govulncheck` step to CI, the
first PR run (`31573828874`) failed on that step with **4 vulnerabilities**.
None were introduced by this PR — they were latent in `main` and surfaced
because we now scan every PR.

The second commit (`9f4d13a`) on `chore/code-health-cleanup` closes all
four with two minimal changes:

| Vuln | Source | Fix |
|------|--------|-----|
| GO-2026-5970 Infinite loop on invalid input | `golang.org/x/text@v0.38.0` (transitive via pgxpool/norm) | bump to **v0.39.0** |
| GO-2026-5856 ECH privacy leak | stdlib `crypto/tls@go1.26.3` | upgrade to **go1.26.5** |
| GO-2026-5039 un-escaped input in errors | stdlib `net/textproto@go1.26.3` | upgrade to **go1.26.5** |
| GO-2026-5037 Inefficient candidate hostname parsing | stdlib `crypto/x509@go1.26.3` | upgrade to **go1.26.5** |

The 3 stdlib issues collapse into a single Go patch bump (1.26.3 → 1.26.5).

## How the bump is wired

`go.mod` keeps the existing `go 1.26.3` directive (operator baseline) and
adds a `toolchain go1.26.5` directive. The `go` directive is a floor — it
says "I need at least 1.26.3 to compile", not "I want exactly 1.26.3".
The `toolchain` directive is the version `go` will auto-fetch when
GOTOOLCHAIN=auto (the default) is in effect. So:

- Local dev with `go1.26.3` already installed → still works; build
  fetches 1.26.5 transparently on first build.
- CI runner with `go1.26.3` already installed (via `setup-go@v6`'s
  `go-version-file`) → same, fetches 1.26.5 once, caches it
  thereafter (`~30 s` cost on first run only).
- Anyone running with the toolchain explicitly pinned via
  `GOTOOLCHAIN=local` → would need a separate bump; we deliberately
  don't restrict this because the directive keeps the project's
  baseline stable.

## Verification (locally, with the toolchain auto-downloaded)

| Step | Result |
|------|--------|
| `go vet ./...` | 0 |
| `go build ./...` | 0 |
| `go test -race -short ./internal/observability/... ./internal/console/... ./internal/benchmark/... ./internal/cron/... ./internal/router/... ./internal/evals/... ./internal/evalplugin/... ./internal/gateway/... ./internal/core/... ./cmd/nexus/...` | 12 packages pass |
| `govulncheck ./...` | **No vulnerabilities found.** (exit 0) |

## Why separately from the first commit

`518264c` introduced the govulncheck CI step. Splitting the bump into a
second commit keeps each commit independently reviewable:

- the *first* commit is "add a scanner" (small, mostly infra);
- the *second* commit is "what the scanner told us, fix it all" (small,
  mechanical, single concern).

Squashing them into one would have been acceptable; keeping them apart
makes the audit trail cleaner because each commit answers one reviewer
question.

## Out of scope

- React-router 6.x CVE (open-redirect): handled via app-layer defense
  on `RequireAuth.tsx` (already in the first commit). The
  `npm-audit --audit-level=high` step won't block this PR because we
  deliberately stay on 6.x.
- Dependabot's separately-flagged 5 alerts on `main` are npm-only and
  unrelated to this govulncheck run. They'll get picked up in their
  own follow-up PRs.
