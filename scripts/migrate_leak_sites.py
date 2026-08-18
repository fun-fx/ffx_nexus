#!/usr/bin/env python3
"""
Migrate the 38 leak sites in internal/console/*.go to use s.fail.

A "leak site" is a writeJSON call that puts err.Error() into the body. Each one
is replaced with s.fail(w, r, status, apierr.CodeXxx, err).

The replacement is mechanical but the CODING (which Code to use) is per case.
The mapping table at the top of this file is the human-edited policy and the
operational handle by which reviewers see what was changed.

After running this, go test ./internal/console/... must pass, and the existing
smoke/idor/authz tests need no rewrite — they're asserting on status codes,
not body shape.

USAGE: scripts/migrate_leak_sites.py
   (no arguments; reads the table at the top and edits files directly)
"""

import os
import re
import sys

# Per-pattern (old code) -> (status, code-name in apierr) replacements.
# status is the HTTP envelope; code-name must be a constant in apierr (validated
# at the end of the script). One row per (status, code) pair seen in the
# repositories. If a new pattern appears, it MUST be added here before the
# file is migrated, so the migration cannot grow new patterns silently.
PATTERNS = [
    # ListBenchmark/ListSchedules/inside List? -> InternalError 500
    (r'writeJSON\(w, http\.StatusInternalServerError, map\[string\]string\{"error": err\.Error\(\)\}\)',
     "apierr.CodeInternalError"),
    # benchmarkErrStatus(err) -> INTERNAL 500; map err to dependency_unavailable
    # for context cancellation, otherwise internal_error. We can't express
    # that without inspecting err at runtime, so we default to internal_error
    # and let postgreserr.Classify cover the context case via caller policy.
    (r'writeJSON\(w, benchmarkErrStatus\(err\), map\[string\]string\{"error": err\.Error\(\)\}\)',
     "apierr.CodeInternalError"),
    # BadRequest with err.Error()
    (r'writeJSON\(w, http\.StatusBadRequest, map\[string\]string\{"error": err\.Error\(\)\}\)',
     "apierr.CodeInvalidRequest"),
]

CODE_NAMES = set(p[1] for p in PATTERNS)
# Validate against the full qualified form used in the code replacement.
VALID_CODES = {
    "apierr.CodeInvalidRequest", "apierr.CodeUnauthorized", "apierr.CodeForbidden",
    "apierr.CodeNotFound", "apierr.CodeConflict", "apierr.CodeRateLimited",
    "apierr.CodeBudgetExceeded", "apierr.CodeUpstreamError",
    "apierr.CodeDependencyUnavailable", "apierr.CodeInternalError",
}
unknown = CODE_NAMES - VALID_CODES
if unknown:
    sys.exit(f"PATTERNS has codes that are not in apierr: {sorted(unknown)}. " +
             "Add the constant to internal/apierr/apierr.go first.")

# Files with leak sites.
FILES = [
    "internal/console/admin.go",
    "internal/console/eval_config.go",
    "internal/console/benchmarks.go",
    "internal/console/benchmark_history.go",
    "internal/console/schedule_handlers.go",
    "internal/console/eval_plugins.go",
    "internal/console/eval_profiles.go",
    "internal/console/benchmark_quality_gate.go",
    "internal/console/benchmark_quality.go",
    "internal/console/benchmark_leaderboard.go",
]

statuses = {"apierr.CodeInternalError": "http.StatusInternalServerError", "apierr.CodeInvalidRequest": "http.StatusBadRequest", "apierr.CodeNotFound": "http.StatusNotFound"}

def http_status_for(code: str) -> str:
    return statuses[code] if code in statuses else "http.StatusInternalServerError"

total = 0
for path in FILES:
    s = open(path).read()
    original = s
    for pat, code in PATTERNS:
        status = statuses[code]
        replacement = f"s.fail(w, r, {http_status_for(code)}, {code}, err)"
        s, n = re.subn(pat, replacement, s)
        total += n
    if s != original:
        # Ensure apierr import is present, sorted in the import block.
        if '"github.com/ffxnexus/nexus/internal/apierr"' not in s:
            # Insert after the last internal/* import.
            m = re.search(r'(\t"github\.com/ffxnexus/nexus/internal/[a-z]+")', s)
            if not m:
                sys.exit(f"{path}: could not find a place to insert the apierr import")
            s = s.replace(m.group(1), m.group(1) + '\n\t"github.com/ffxnexus/nexus/internal/apierr"', 1)
        open(path, "w").write(s)
        print(f"migrated {path}")
print(f"replacements: {total}")
