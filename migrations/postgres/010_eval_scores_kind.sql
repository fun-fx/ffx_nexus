-- Allow the dispatcher to segment legacy vs plugin-originated scores
-- in Postgres-only deployments. `evaluator` is already TEXT, so we add
-- a generated `kind` computed column for cheap GROUP BY. Existing rows
-- are mapped to 'legacy' or 'heuristic' based on prefix; the runtime
-- uses 'plugin:<name>' so we leave a `kind = 'plugin'` partition.
ALTER TABLE eval_scores
    ADD COLUMN IF NOT EXISTS kind TEXT
    GENERATED ALWAYS AS (
        CASE
            WHEN evaluator LIKE 'plugin:%' THEN 'plugin'
            WHEN evaluator IN ('heuristic_pii', 'heuristic_completeness') THEN 'heuristic'
            ELSE 'legacy'
        END
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_eval_scores_kind_ts ON eval_scores (kind, timestamp DESC);
