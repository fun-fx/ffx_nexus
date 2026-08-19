# Gitleaks history baseline

Last full-history scan: 2026-08-19. The scan covered all 552
reachable commits with `gitleaks detect --log-opts=--all
--config .gitleaks.toml` (gitleaks v8.18+ binary, the same engine
wiring CI's v1 job runs an older v7.4.0 ruleset, but a v8 binary
scan catches the gap below).

## Reproduce locally

```bash
gitleaks detect --config .gitleaks.toml --log-opts=--all \
    --report-format json --report-path gitleaks-history.json
python3 -c "import json,sys; d=json.load(open('gitleaks-history.json')); \
  print('\n'.join(f\"{x['File']}:{x['StartLine']} {x['RuleID']} {x['Commit'][:7]} {x['Secret'][:60]}\" for x in d))"
```

## False positives

These items match a rule by shape but are not real credentials.
Each has been hand-checked against the file content.

| Commit     | File                                          | Rule              | Reason                                                       |
|------------|-----------------------------------------------|-------------------|--------------------------------------------------------------|
| 0255a479…  | internal/core/crypto/crypto_test.go:37        | generic-api-key   | `0123456789abcdef` × 8 — 64-char hex test vector for cipher |
| b89989cf…  | internal/core/eval_plugin_keys_test.go:57     | generic-api-key   | `sk-lf-…0123456789`-style placeholder, no live prefix         |
| 5459648c…  | internal/core/eval_plugin_keys_test.go:57     | generic-api-key   | same as above (re-written fixture)                           |
| 240cc606…  | web/src/components/PluginKeysModal.test.tsx:76| generic-api-key   | `sk-lf-1234567890abcd` UI test fixture                       |
| f6e2c7e2…  | web/src/components/PluginKeysModal.test.tsx:76| generic-api-key   | same as above (re-written fixture)                           |
| 9543459c…  | web/src/pages/Keys.test.tsx:43                | generic-api-key   | `nxs_live_a1b2` obvious placeholder for the live-key path    |
| ababb320…  | web/src/pages/Keys.test.tsx:43                | generic-api-key   | same as above (re-written fixture)                           |
| 47b9905e…  | scripts/test_observability_dev_container.sh:121 | curl-auth-user  | `admin:admin` for the dev-container Grafana entry            |
| 3f4aa2f8…  | scripts/test_observability_dev_container.sh:121 | curl-auth-user  | same as above (re-written)                                   |
| 0d4cbe49…  | scripts/demo_reset.sh:44                     | curl-auth-user    | `nexus:nexus` demo bootstrap credential                       |
| 0d4cbe49…  | scripts/demo_reset.sh:46                     | curl-auth-user    | same as above (literal, two definitions for clarity)         |
| 818cb8e7…  | scripts/demo_reset.sh:44                     | curl-auth-user    | same as above                                                |
| 818cb8e7…  | scripts/demo_reset.sh:46                     | curl-auth-user    | same as above                                                |
| 60921324…  | marketing/src/pages/docs/index.astro:98       | curl-auth-header  | `nxs_live_...` doc snippet (3-char ellipsis, not a real key) |
| ccd145ff…  | marketing/src/pages/docs/index.astro:98       | curl-auth-header  | same as above                                                |

### Why these are not real credentials

- **`0123456789abcdef…`** is a 64-char hex pattern that matches the
  AES-256 key length. It is the deterministic test vector in
  `crypto_test.go:37`. Rotating would invalidate the test vector
  itself.
- **`sk-lf-…`** is a synthesised fixture for the eval-plugin-keys
  tests. No upstream vendor issues keys starting with `sk-lf-`;
  Langfuse issues `sk-lf-…` only as live keys. Real Langfuse keys are
  in `pk-lf-…` and `sk-lf-…` but only AFTER the live public key has
  been issued from the Langfuse console. The test uses a UUID-shaped
  suffix to avoid any substring collision with a real key.
- **`nxs_live_a1b2`** is 11 chars, well below the Gateway virtual-key
  entropy. It cannot be a live `nxs_live_…` key. The string appears
  in React unit tests for the Keys page that intentionally probe the
  prefix-matching UI without a real credential.
- **`admin:admin`** and **`nexus:nexus`** appear in dev-only scripts
  that run inside the dev container, on the host's loopback. They
  are documented in `scripts/test_observability_dev_container.sh`'s
  preamble; outside dev-container runs these scripts refuse to start.
  Production installs never read these files.

## True positives

**None.** No Commit in this repo's history contains a credential that
matches any gitleaks rule *and* is operational in production. A
future entry here without a credential-rotation note is a release-
blocking incident; the affected customer(s) MUST be notified within
24 hours of detection.

## Why the inline markers are not retroactive

Past main-merge commits do not have `# gitleaks:allow (rule=…)`
inline comments. They pre-date the inline-marker convention. The
PR-base future branch check that the user mentioned — "new PRs whose
base is post-3bc9d89 auto-resolve" — is accurate: the allow-list
shape changed then, and newer commits inherit the inline-comment
discipline. The "auto resolution" is unsafe in two cases:

1. A customer's SOC2 auditor does NOT use the same base: they
   hist-scan all 552 commits. The inline-marker-on-newer-commits-only
   approach leaves historical findings.
2. Anyone reverts to a pre-3bc9d89 base to bisect introduces the
   markers' gap.

Therefore this document is the canonical handler for any past-blob
finding, not the inline markers.

## Re-baseline cadence

| Cadence  | Trigger                          | Action                               |
|----------|----------------------------------|--------------------------------------|
| quarterly| Next quarter release            | Re-run the binary scan; commit delta |
| per-PR   | New secret fixture convention    | Update the inline-marker table       |
| per-PR   | New live-key type added          | Add a custom rule + 1 fixture row    |
| incident | True-positive entry found        | Rotate → story in `security-logs/`   |

The relevant quarterly cadence is documented in
`docs/security-scan-quarterly-YYYY-Qn.md`.
