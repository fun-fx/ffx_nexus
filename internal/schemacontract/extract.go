// Package schemacontract extracts the SQL a package's Go source contains, so that
// tests can verify every table and column it names actually exists in the schema
// the migration set produces.
//
// # Why this exists
//
// Three times now, production code has queried something the migration set never
// created: invite_tokens (the table was absent entirely), a benchmark_runs column,
// and benchmark_schedules.last_run_id together with last_launched_at. Each one
// worked in unit tests against a fake store, compiled cleanly, and failed with
// SQLSTATE 42P01 or 42703 the first time a customer clicked the feature.
//
// Nothing in the Go type system connects a string containing "last_run_id" to a
// CREATE TABLE in a .sql file, so the compiler cannot help and reviewers reliably
// do not notice.
//
// # Why extraction plus PREPARE, rather than parsing SQL ourselves
//
// The obvious approach is to parse the SQL and compare identifiers against
// information_schema. That means writing a SQL parser that understands aliases,
// CTEs, subqueries, casts and function calls — and one that fails OPEN on
// anything it cannot read, which is precisely the case where an identifier is
// wrong.
//
// Instead this package extracts the statement text and hands it to the database:
//
//	PREPARE stmt AS <the statement>
//
// Postgres parses and validates the statement — every table, every column, every
// type — WITHOUT executing it. No rows are read, no rows are written, and the
// answer comes from the same parser that will run in production. There is no
// approximation to get wrong.
//
// # The honest limitation
//
// Some queries are assembled at runtime:
//
//	q += " WHERE " + strings.Join(conds, " AND ")
//
// A statically extracted literal is then a fragment that cannot be prepared. This
// package classifies those separately and REPORTS the count rather than ignoring
// them, so the coverage figure is visible instead of implied. Fragments are
// covered by the smoke test that executes the real read paths against an empty
// migrated database.
package schemacontract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Statement is one SQL statement found in Go source.
type Statement struct {
	File string
	Line int
	SQL  string
	// Complete reports whether the text looks like a standalone statement that a
	// database could parse. Incomplete text is a runtime-assembled fragment.
	Complete bool
	// Why explains an incomplete classification, for the coverage report.
	Why string
}

var (
	// A literal is SQL if it contains a leading verb. Anchoring on the verb keeps
	// error messages, log lines and column-name constants out of the results.
	sqlVerb = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT\s+INTO|UPDATE|DELETE\s+FROM|WITH)\s`)

	// Text that is plainly a fragment rather than a statement.
	leadingFragment = regexp.MustCompile(`(?is)^\s*(AND|OR|WHERE|ORDER\s+BY|LIMIT|OFFSET|JOIN|,|\))`)

	// Go's fmt verbs and string concatenation leave holes a parser cannot fill.
	interpolation = regexp.MustCompile(`%[sdvqft]|%\+v`)
)

// Extract walks dir for .go files (excluding tests) and returns the SQL in them.
func Extract(dir string, skipDirs ...string) ([]Statement, error) {
	skip := map[string]bool{"testdata": true, "vendor": true, "node_modules": true}
	for _, d := range skipDirs {
		skip[d] = true
	}

	var out []Statement
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text := unquote(lit.Value)
			if !sqlVerb.MatchString(text) {
				return true
			}
			st := Statement{
				File: path,
				Line: fset.Position(lit.Pos()).Line,
				SQL:  strings.TrimSpace(text),
			}
			st.Complete, st.Why = classify(st.SQL)
			out = append(out, st)
			return true
		})
		return nil
	})
	return out, err
}

// classify decides whether a statement can be handed to a database as-is.
func classify(sql string) (bool, string) {
	switch {
	case interpolation.MatchString(sql):
		// fmt.Sprintf assembles the final text; the literal has holes.
		return false, "contains a format verb, so the final text is assembled at runtime"
	case leadingFragment.MatchString(sql):
		return false, "begins mid-statement, so it is concatenated onto another string"
	case strings.Count(sql, "(") != strings.Count(sql, ")"):
		return false, "unbalanced parentheses, so the literal is part of a larger statement"
	case endsMidClause(sql):
		return false, "ends mid-clause, so a runtime fragment is appended"
	}
	return true, ""
}

// endsMidClause catches a literal that trails off where more text is appended,
// e.g. `SELECT ... FROM t` followed by ` WHERE ` + conds.
func endsMidClause(sql string) bool {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	upper := strings.ToUpper(trimmed)
	for _, suffix := range []string{
		" WHERE", " AND", " OR", " ORDER BY", " GROUP BY", " SET", " VALUES",
		" IN", " ON", " =", ",", "(",
	} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// unquote turns a Go string literal into its value. Raw (backquoted) literals
// hold the bulk of the SQL in this codebase; interpreted literals need escape
// handling, and only the escapes that appear in SQL text matter.
func unquote(lit string) string {
	if len(lit) < 2 {
		return ""
	}
	body := lit[1 : len(lit)-1]
	if lit[0] == '`' {
		return body
	}
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`)
	return r.Replace(body)
}

// Coverage summarises how much of the extracted SQL a database can validate.
type Coverage struct {
	Total      int
	Complete   int
	Incomplete []Statement
}

// Summarise groups statements for reporting.
func Summarise(stmts []Statement) Coverage {
	c := Coverage{Total: len(stmts)}
	for _, s := range stmts {
		if s.Complete {
			c.Complete++
			continue
		}
		c.Incomplete = append(c.Incomplete, s)
	}
	return c
}
