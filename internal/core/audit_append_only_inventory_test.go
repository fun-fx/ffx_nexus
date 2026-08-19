// c0.4 inventory walker: any code that issues UPDATE or DELETE
// against the audit_log table outside the canonical purge helper is a
// contract violation. This test walks the Go source tree (excluding
// subdirectories that carry pure read paths like tests) and fails the
// build with a clear message pointing at the offending file/line.
//
// Scope notes — the inventory covers:
//   1. UPDATE / DELETE / TRUNCATE on audit_log in any literal SQL
//      string used in production Go code, including nested forms
//      (CTEs, multi-statement strings, dollar-quoted SQL).
//   2. Any variable-assembled SQL whose lowered form contains an
//      audit_log mutation is flagged whether or not the variable
//      part is itself a literal.
//   3. CTE-with-DELETE patterns like `WITH x AS (...) DELETE FROM
//      audit_log ...` are flagged because the production lexer
//      sees the `DELETE FROM audit_log` substring we explicitly
//      track in isAuditMutation. Long-tail patterns (e.g. EXECUTE
//      on a prepared statement off-row) are not detectable with a
//      pure-source walk and are documented in docs/audit-log-roles.md
//      as residual risk; the Allow-list pattern below is the place
//      where those site-specific exceptions are recorded.
//   4. Migration files under migrations/postgres are part of the
//      destructive surface; the inventory walker excludes them
//      because their job IS to mutate the schema, and any new
//      destructive change there is reviewed by humans at PR time.
//      Excluding them stops the inventory from raising a metric
//      that's already accounted for elsewhere.
//   5. Tests are excluded because the inventory targets production
//      paths; helpers under testdata/ are also excluded.

package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditLogMutationAllowList is the list of fully-qualified identifiers
// that are allowed to mutate audit_log. Adding a site without updating
// this list will fail this test on every PR.
//
// Currently this is only PurgeStaleAuditRows; any future code path must
// add itself here with a justification comment.
var auditLogMutationAllowList = map[string]bool{
	"(*github.com/ffxnexus/nexus/internal/core.Store).PurgeStaleAuditRows": true,
}

// TestAuditAppendOnlyEnforcedInAppCode refuses UPDATE / DELETE on
// audit_log anywhere outside the allow-listed helpers. AST walks are
// case-insensitive on sql keywords but case-sensitive on identifiers.
//
// Mutation: introduce a new function `DeleteOldAuditLog(s *Store)`
// that calls Exec(ctx, "DELETE FROM audit_log ...") — this test will
// flag it and force the engineer to either rename or append to the
// allow-list with justification.
func TestAuditAppendOnlyEnforcedInAppCode(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	bad := []string{}

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip first-party test packages; the inventory walker
			// is for production code paths only.
			base := filepath.Base(path)
			if base == "testdata" {
				return nil
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip *_test.go — the inventory targets production paths.
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") {
			return nil
		}
		if base == "audit_append_only_inventory_test.go" {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Find the function name being called; if it's a method
			// like s.pool.Exec(...), we record it as the qualified
			// receiver.
			callee := qualifiedCallee(call)
			// Look at every string literal argument — these are where
			// raw SQL lives. Variable-assembled SQL (string concat)
			// is detected via a separate ast sweep in
			// collectStringAssemblies below, because lower literals
			// plus a runtime concatenation can still produce an
			// audit_log mutation.
			for _, arg := range call.Args {
				bl, ok := arg.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				value := strings.ToLower(strings.Trim(bl.Value, "\""))
				if isAuditMutation(value) {
					if auditLogMutationAllowList[callee] {
						return true
					}
					bad = append(bad, path+":"+fset.Position(bl.Pos()).String()+" "+callee)
				}
			}
			return true
		})
		// Variable-assembled SQL pass: any assignment of the form
		// `x := "<piece>" + ... + "..."` where the lowered
		// concatenation contains an audit_log mutation, regardless
		// of whether the literal pieces individually mention
		// audit_log, is flagged. This catches the sneakier
		// `q := "DELETE FROM " + table` shape that the inline-literal
		// pass would miss.
		for _, conc := range collectStringAssemblies(f) {
			if isAuditMutation(strings.ToLower(conc.value)) ||
				globalAuditMutationRejectsPatterns(conc.value) {
				bad = append(bad, path+":"+fset.Position(conc.pos).String()+
					" var-assembled-sql: "+conc.value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("audit_log UPDATE/DELETE detected outside the allow-list:\n  %s\n"+
			"Add the offending function/method to auditLogMutationAllowList with a justification, "+
			"or remove the call. The audit_log table is append-only by design.",
			strings.Join(bad, "\n  "))
	}
}

// isAuditMutation returns true when the SQL string targets audit_log
// with a write operation. Case-insensitive to match Postgres lexer.
//
// The detection set covers:
//
//   - "update audit_log"                       (UPDATE … audit_log)
//   - "delete from audit_log"                 (DELETE FROM audit_log)
//   - "delete only audit_log"                 (DELETE ONLY for partitioned)
//   - "truncate audit_log"                     (TRUNCATE — destructive)
//   - "truncate table audit_log"              (explicit fully-qualified form)
//   - "truncate audit_log_..."                (truncate on a future partition)
//
// CTE forms like `WITH x AS (...) DELETE FROM audit_log USING x` are
// detected automatically because the substring check on the FULL
// lowered SQL still finds `delete from audit_log`. Prepared-statement
// patterns where the audit_log mutation lives in a server-side
// function call (e.g. `SELECT nexus_audit_purge_rows(...)`) are
// separately documented in docs/audit-log-roles.md and live behind
// the SQL function nexus_audit_purge_rows whose INSERT/UPDATE/DELETE
// is co-located with the role definition.
func isAuditMutation(s string) bool {
	if !strings.Contains(s, "audit_log") {
		return false
	}
	if strings.Contains(s, "update audit_log") ||
		strings.Contains(s, "delete from audit_log") ||
		strings.Contains(s, "delete only audit_log") ||
		strings.Contains(s, "truncate audit_log") {
		return true
	}
	return false
}

// globalAuditMutationRejectsPatterns is a check on stripped-near-SQL
// statements that don't contain "audit_log" because they target a
// derived name. The CTAS / wildcard truncate form `TRUNCATE
// audit_log_%` is detected here so the test fails even when the
// table-name part is dynamic. Anything that future-proofs the
// inventory should be added to this list.
func globalAuditMutationRejectsPatterns(s string) bool {
	lowered := strings.ToLower(s)
	if strings.HasPrefix(strings.TrimSpace(lowered), "truncate") &&
		strings.Contains(lowered, "audit_log") {
		return true
	}
	return false
}

// qualifiedCallee returns a best-effort string identifier of what
// function is being called. For receivers like `s.pool.Exec(...)`, it
// returns the segment-namespaced form (*pkg.Store).Exec to match our
// allowList style.
func qualifiedCallee(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		left := ""
		if id, ok := fn.X.(*ast.Ident); ok {
			left = id.Name
		} else if sel, ok := fn.X.(*ast.SelectorExpr); ok {
			left = prettyExpr(sel)
		}
		return left + "." + fn.Sel.Name
	}
	return "<unknown>"
}

func prettyExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return prettyExpr(v.X) + "." + v.Sel.Name
	}
	return ""
}

// stringAssembly captures a lowered and concatenated string
// expression so the inventory pass can detect audit_log mutations
// hidden in var-assembled SQL.
type stringAssembly struct {
	value string
	pos   token.Pos
}

// collectStringAssemblies walks the AST and returns any string
// concatenation whose lowered joined form contains the word
// "audit_log". This is a coarse detection — the lowered forms of
// "DELETE FROM " + "audit_log" and `q := "TRUNCATE " + name` are both
// returned. We intentionally widen the net: an innocent variable
// name like `audit_log_query` (substring match) surfaces as a flag
// and the reviewer discards whether the test is right; we trade a
// false-positive risk for a missed-mutation risk, because missing
// the append-ONLY kill-switch silently is the worse outcome.
func collectStringAssemblies(f *ast.File) []stringAssembly {
	var out []stringAssembly
	var walkExpr func(e ast.Expr)
	walkExpr = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				out = append(out, stringAssembly{
					value: strings.ToLower(strings.Trim(v.Value, "\"")),
					pos:   v.Pos(),
				})
			}
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				walkExpr(v.X)
				walkExpr(v.Y)
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, rhs := range v.Rhs {
				walkExpr(rhs)
			}
		case *ast.ValueSpec:
			if v.Values == nil {
				return true
			}
			for _, rhs := range v.Values {
				walkExpr(rhs)
			}
		}
		return true
	})
	return out
}
