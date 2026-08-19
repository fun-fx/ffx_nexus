# pinHitCount history — monotonic decrease evidence

The bypass-drift detector (`internal/apierr/bypass_drift_test.go`)
freezes a count of the free-form error-body sites that have not yet
been refactored through `apierr.Render` / `resp.HTTP` /
`(s *Server).fail` / `writeError`. The pin is held in source and
verified by CI; this file is the **monotonicity evidence** the
operator wants so the pin never silently grows back up.

A new entry below is required every time someone bumps the pin.
If a PR adds to the pin without a corresponding entry below, the
detector at PR time (see the workflow comment near
`.github/workflows/ci.yml#bypass-drift-pin`) flags the discrepancy
and rejects the change.

## Entries (newest first)

| Commit / Date | Old pin | New pin | Reason                                                                    |
|---------------|---------|---------|---------------------------------------------------------------------------|
| (this baseline) | (none)  | 139     | initial capture; matches the surface walking the production console files |

The pin is **expected** to drop monotonically. Any pin entry
that goes up is reviewed in the same PR; reviewers should ask
"which migration was undone?" before signing off.

The detector test reads this file path indirectly (a Go-side
const at the test imports). Adding entries here without
touching the const is a documentation-only change; touching
the const and not adding here is the violation.

## Why this is not pure self-discipline

The detector test fails if `len(hits)` exceeds the pin. A
silent bump of `pinHitCount = 140` (matching a `git pull`
that landed a new bypass) would push the test back below
threshold and CI would stay green. The history file is the
secondary check: if the pin is incremented, the operator
asks "what got added?" before merging; the answer lives here.

A future refactor goal pins the value to zero. That decouples
the test from the freeze entirely.
