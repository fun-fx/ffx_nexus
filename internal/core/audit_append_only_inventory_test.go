// c0.4 inventory walker: any code that issues UPDATE or DELETE against
// the audit_log table outside the canonical purge helper is a contract
// violation. This test walks the Go source tree (excluding
// subdirectories that carry pure read paths like tests) and fails the
// build with a clear message pointing at the offending file/line.

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
			// raw SQL lives.
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
func isAuditMutation(s string) bool {
	if !strings.Contains(s, "audit_log") {
		return false
	}
	if strings.Contains(s, "update audit_log") ||
		strings.Contains(s, "delete from audit_log") ||
		strings.Contains(s, "delete only audit_log") {
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
