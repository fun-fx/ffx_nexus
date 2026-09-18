#!/usr/bin/env bash
# Rebuild language-neutral schema dumps from migrations/*.sql.
# Does not connect to a database. Live SHOW CREATE is optional when
# NEXUS_POSTGRES_URL / NEXUS_CLICKHOUSE_URL are set.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/priv/fixtures/contracts/schema"
mkdir -p "$out"

python3 - "$root" "$out" <<'PY'
import hashlib, json, sys
from pathlib import Path
root, out = Path(sys.argv[1]), Path(sys.argv[2])
ledger = []
for engine in ("postgres", "clickhouse"):
    parts = [
        f"-- Reconstructable empty-DB fixture for {engine}.\n",
        f"-- Concatenation of migrations/{engine}/*.sql in ordinal order.\n",
        "-- This is the schema Elixir must recreate. Do not invent a parallel ledger.\n",
        "-- Re-run scripts/dump_schema.sh after adding migrations.\n\n",
    ]
    files = sorted((root / "migrations" / engine).glob("*.sql"))
    for f in files:
        data = f.read_bytes()
        ledger.append({
            "id": f"{engine}/{f.name}",
            "engine": engine,
            "name": f.name,
            "sha256": hashlib.sha256(data).hexdigest(),
            "bytes": len(data),
        })
        parts.append(f"-- ===== {engine}/{f.name} =====\n")
        text = data.decode("utf-8")
        parts.append(text if text.endswith("\n") else text + "\n")
        parts.append("\n")
    dest = out / f"{engine}.sql"
    dest.write_text("".join(parts))
    print(f"wrote {dest.relative_to(root)} ({len(files)} files)")
(out / "ledger.json").write_text(json.dumps({"ledger_table": "schema_migrations", "migrations": ledger}, indent=2) + "\n")
print(f"wrote ledger.json ({len(ledger)} entries)")
PY

if [[ -n "${NEXUS_POSTGRES_URL:-}" ]]; then
  echo "optional live dump: psql to SHOW schema (not required for Phase 0)" >&2
fi
if [[ -n "${NEXUS_CLICKHOUSE_URL:-}" ]]; then
  echo "optional live dump: clickhouse-client SHOW CREATE TABLE (not required for Phase 0)" >&2
fi
