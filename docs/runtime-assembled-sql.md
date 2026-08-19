# Runtime-assembled SQL — the 5 fragments the contract can't statically prove

The schema contract (`internal/schemacontract/postgres_integration_test.go`)
extracts SQL literals and hands every **complete** one to Postgres as
`PREPARE stmt AS <the statement>`. The **incomplete** five are
runtime-assembled: the literal in the source has a format verb
(`%s` / `%d`) or trails off mid-clause, so the static text is
not the final SQL that runs against the database. They are
covered by the smoke test in `internal/schemacontract/clickhouse_smoke_test.go`
rather than by PREPARE.

The list below pins the WHO, WHERE, and WHY for each so a future
auditor can grep the source for a hex-string match without first
re-running the extractor.

## 1. `internal/observability/reader.go:664` — eval_scores summary

```go
rows, err := r.conn.Query(ctx, `
    SELECT evaluator, metric,
           avg(score) AS avg_score,
           avg(passed) AS pass_rate,
           toInt64(count()) AS samples
    FROM eval_scores
    WHERE timestamp >= now() - INTERVAL ? SECOND
      AND `+orgCond+`
```

- **Why incomplete**: trailing `AND ` followed by runtime-concatenated
  `orgCond` (`AND org_id = ?` / empty). The literal is the prefix;
  the per-org predicate is appended at runtime.
- **Smoke covers**: ClickHouse EXPLAIN SYNTAX against the live
  query, with the runtime assembly substituting a literal
  predicate. Any new column referenced inside this prefix surfaces
  in the EXPLAIN error.

## 2. `internal/observability/reader.go:732` — model_alignment

```go
rows, err := r.conn.Query(ctx, `
    WITH
      q AS (
        SELECT user_id, ... )
    SELECT ...
`+rollupCols+...
```

- **Why incomplete**: the `WITH` query sells a CTE that feeds a
  larger SELECT; the column list at the outer step is appended via
  `+rollupCols`. The per-metric columns are not statically knowable.
- **Smoke covers**: same EXPLAIN SYNTAX path; a missing column in
  the CTE surfaces during explanation.

## 3. `internal/observability/reader.go:879` — daily spend summary

```go
q := `
    WITH
      cur AS (
        SELECT ifNull(sum(cost_usd), 0) AS cost,
               toInt64(sum(input_tokens + output_tokens)) AS tokens,
```

- **Why incomplete**: `userFilter = " AND user_id = ?"` is appended
  to the WITH/SELECT body conditionally. The literal is the prefix
  before that append.
- **Smoke covers**: EXPLAIN with both shapes (admin sees all,
  user-filtered sees one row).

## 4. `internal/observability/reader.go:1025` — daily spend breakdown

```go
func buildDailySpendBreakdownQuery(orgID, userID string) string {
    q := `
        SELECT request_model,
               (` + buildEffectiveProviderExpr + `) AS provider,
```

- **Why incomplete**: `(...) AS provider` interpolates
  `buildEffectiveProviderExpr` at runtime — a multi-line CSV of
  `CASE WHEN parameters['x'] = … THEN …` clauses that the
  extractor cannot read as a single literal. The fragment is
  syntactically valid SQL once composed.
- **Smoke covers**: EXPLAIN SYNTAX with `buildEffectiveProviderExpr`
  inlined; without smoke we cannot prove the `CASE` chain parses
  end-to-end against the actual `gateway_traces.parameter_columns`.

## 5. `internal/observability/metabase.go:462` — error message format

```go
return 0, fmt.Errorf("update database status %d: %s",
    resp.StatusCode, string(raw))
```

- **Why incomplete**: this is **NOT SQL**. The string starts with
  `update database status %d:` — which the `sqlVerb` regex's
  `^\s*UPDATE\s` happens to match. The `interpolation` regex
  picks up `%d` and classifies it as assembly.
- **Smoke / contract coverage**: it is a `fmt.Errorf` format string,
  not a query. There is no schema to which it binds. The
  inventory test (`TestExtractorCoverageIsReported`) reports the
  shape so an extractor regression that loses the `interpolation`
  guard would surface immediately — the count would drop from 5
  to 4 and the line would reappear as a bogus COMPLETE entry.

## Why not statically extract these

Two reasons:

1. **Format verbs (`%s`, `%d`) cannot be inlined into a parseable
   statement**. Replacing them with placeholder `?` changes column
   types for some functions; a literal `now() - INTERVAL ? SECOND`
   binds the placeholder to Int64 but `now() - INTERVAL 60 SECOND`
   bakes the value into the query plan.
2. **String concatenation across `+` boundaries cuts the literal
   in half**. The extractor sees a fragment; the runtime sees the
   whole. The smoke test exercises the runtime path, so the two
   views reconcile.

The smoke test that exercises these lives in
`internal/schemacontract/clickhouse_smoke_test.go` and is described in
`docs/clickhouse-verification.md`.
