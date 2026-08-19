# Secrets / SAST policy

This document is the policy of record for what runs in CI to keep
secrets and policy-violating patterns out of the repository. It pairs
with `.gitleaks.toml`, `.github/workflows/ci.yml`, and the `trivy-*`
job steps.

## Why we scan

Two failure modes drive this:

1. **A live credential lands in git.** A developer pastes it into a
   test fixture and forgets to redact it; the diff lands; the
   credentials are now in the public commit history.
2. **A configuration policy is broken in CI.** A Dockerfile ships as
   root; a Helm template hard-codes a default password; a Kubernetes
   manifest runs with `privileged: true`. These do not break the
   customer's TLS, but they leave a permanent regression window for
   the day a real attacker shows up.

Anything less than "every PR, every push" is a slot machine.

## Tools and what they cover

| Layer              | Tool           | Backing version | Scope                                   | Failure on fork? |
|--------------------|----------------|-----------------|-----------------------------------------|------------------|
| Secret scan        | gitleaks       | v7.4.0 (1)      | All committed blobs (PR) + new commits  | yes (exit 1)     |
| Image / config scan| trivy          | v0.74.0         | Dockerfile + K8s + image advisories     | yes (exit 1)     |
| Go test            | go test        | per repo        | Mutation + contract + behaviour guards  | yes              |

(1) The action `zricethezav/gitleaks-action@v1` pins to gitleaks
v7.4.0 (the maintained community edition). `gitleaks/gitleaks-action@v2`
ships gitleaks v8 but is gated behind a commercial license, so this
repo pins to v1. The v1-v2 rule differences are summarised in §
[known v1/v2 gaps](#known-v1v2-gaps) below; **the in-repo inventory
tests compensate for the gaps whose detection the default rule sets
do not cover**.

## Where allowlist lives, where it does NOT

Every allowlist entry must:

- Name the **specific path or regex** it relaxes.
- State the **specific reason** a finding there is not a secret.
- Have an **expiry** when the relaxation depends on a third-party
  workaround (e.g. .devcontainer/Dockerfile needing Docker-in-Docker
  root).

The previous file-shape — full paths in `[allowlist].paths` — was
**withdrawn**: a path-wide allowlist lets a real key pasted anywhere
on that path sail through the scanner. The replacement is per-line
`# gitleaks:allow (rule=NAME)` comments on the precise sentinel
lines, plus a tight `[allowlist].regexes` for self-evident placeholders
that can only ever be placeholders (`sk-test...`, `REPLACE_ME`, etc).
Future maintainers who copy-paste a real key into `internal/apierr/`
will trip the scanner immediately — the file's exemption does not
cover their new line.

## Known v1/v2 gaps

The maintained rule sets in gitleaks v7.4.0 (used by `gitleaks-action@v1`)
vs. v8.x (used by `gitleaks-action@v2`) drift in several detectable
ways. The differences that we have actually seen bite v8 detections
that v7 misses:

| Pattern family        | v7.4.0 behaviour | v8.x behaviour | On this repo today                                |
|-----------------------|------------------|----------------|---------------------------------------------------|
| Hugging Face tokens   | not detected     | detected       | Not stored; gap would surface, not covered        |
| Anthropic API keys    | partial          | full           | Not stored; default Slack/GH rules already cover  |
| GCP service-account   | per-resource     | file-pattern   | Not stored; future risk if Google OAuth surfaced  |
| AWS ASIA rotated      | older entropy    | improved       | `AKIA` regex matches both                        |
| Slack refresh tokens  | suffix mismatch  | matches both   | Not relevant (bot tokens via `xoxb-`)             |

The mitigation strategy is **not** to upgrade to v2 (the licence
gate), but to:

1. Add custom rules that close the gap locally (`nexus-virtual-key-live`
   in `.gitleaks.toml` is the example).
2. Run **direct gitleaks binary** on a quarterly cadence (see §
   "quarterly binary scan"), so a v8 finding we missed gets into our
   backlog rather than a customer's report.
3. Track any gitleaks rule the repo's maintainers add to v8 separately.

### Quarterly binary scan

Per-quarter we re-run the latest gitleaks release **as a binary, not
through the action**, to act as a check on v1 drift. The output is
committed to `docs/security-scan-quarterly-YYYY-Qn.md` so the rule
gap is visible to future operators. A binary-scan-only finding is
the trigger for adding an in-repo custom rule, not for moving to v2.

## Trivy baseline policy

See `docs/trivy-baseline.md` for the per-finding acceptance ledger.
The principle: `--severity HIGH,CRITICAL --exit-code 1` blocks any
new finding; pre-existing findings are registered debt and have an
explicit quarterly review date.

## Gitleaks history baseline

Per-quarter the repo is scanned with `gitleaks detect --log-opts=--all`
against the **entire git history** to surface any past commit that
landed a real credential. The output of the most recent full-history
run is recorded in `docs/gitleaks-history-baseline.md` so customer
security reviews (SOC2, ISO27001) can reconcile any gitleaks report
they run against ours. The baseline distinguishes:

- **False positives**: placeholders, test fixtures, demo scripts
  that match a secret rule by shape but cannot be a live credential.
  Each item names the file, line, commit hash, and the reason it
  cannot be live (a "sk-lf-…" prefix used as a test fixture is
  documented as a placeholder because no upstream vendor issues keys
  starting with `sk-lf-`.)
- **True positives**: anything past this row is an active secret and
  MUST trigger immediate credential rotation. Any new entry here
  without a rotation note is a release-blocking incident.

A blank True Positive section is the expected outcome — a populated
section is an incident. Re-baselining runs on every quarterly scan
(`docs/security-scan-quarterly-YYYY-Qn.md`).

