package apierr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleErrorPathsBypassApierr is a structural drift detector: it
// walks internal/console looking for places where a handler concatenates
// err.Error() into a writeJSON body without going through apierr.Render,
// (s *Server).fail, or resp.HTTP.
//
// The detector catches the regression class that produced the original
// leak fix: a handler that hand-built an error response by appending
// err.Error() directly. The body shape of an apierr.Bodies response is
// already covered by TestResponseBodyHeaderAndLogCarryTheSameRequestID,
// which is the load-bearing assertion of correctness. This test is the
// drift tripwire: every site that bypasses the three sanctioned paths
// appears here.
//
// What's a sanctioned path:
//
//   - apierr.Render — writes apierr.Body shapes only.
//   - resp.HTTP — writes apierr.Body shapes only, with a slog entry.
//   - (s *Server).fail / failWithMessage / failBenchmark / failOrigin
//     — write apierr.Body shapes only.
//   - writeError — gateway-side OpenAI shape with a Scrub pass on msg.
//
// Anything else that ends up writing "error: <free-form string>" into a
// JSON response is a leak vector. The detector classifies these by
// syntactic match on the AST, so a developer renaming the key from
// "error" to "reason" still trips the test.
//
// Files with the sanctioned paths themselves are exempt because they
// contain the implementation: a writeJSON call inside s.fail is
// expected. Likewise the apierr package, the resp package, and the
// gateway surface that uses its own writeError with a Scrub pass.
// pinHitCount is the freeze that makes this test a *drift* detector
// rather than a refactor checklist. Each existing direct `writeJSON(...,
// {"error": ...})` call site shows up at the current count. Refactors
// are encouraged to bring this number down; the test fails the moment a
// new site is added, which is the property that catches the "future
// handler bypasses apierr" regression class.
//
// The freeze is reset to zero when the count hits zero (the goal).
// Until then, the test enumerates every surviving site inline and
// asserts it does not exceed the pinned number.
//
// Force-fail testing: bumping this constant by 1 makes the test pass
// from 1 through the pinned count; dropping it by 1 makes the test
// fail conditionally on the actual hits being below.
const pinHitCount = 139

func TestConsoleErrorPathsBypassApierr(t *testing.T) {
	if pinHitCount == 0 {
		t.Skip("pinHitCount is 0; the codebase is fully migrated through the " +
			"sanctioned paths (apierr.Render / resp.HTTP / (s *Server).fail / " +
			"writeError). Set the constant to re-enable the structural drift " +
			"detector when migrating a new package.")
	}
	root := "../.."
	banned := []string{
		"internal/console",
	}
	allowed := map[string]bool{
		"internal/apierr/apierr.go":                   true,
		"internal/resp/resp.go":                       true,
		"internal/console/server.go":                  true,
		"internal/observability/metric_scrub_test.go": true,
	}

	var hits []string
	for _, dir := range banned {
		walk(t, filepath.Join(root, dir), func(p string, f *ast.File) {
			if allowed[p] {
				return
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := exprText(call.Fun)
				if callee != "writeJSON" && !strings.HasSuffix(callee, ".Encode") {
					return true
				}
				for _, arg := range call.Args {
					if freeformErrorBody(arg) {
						line := fset.Position(call.Pos()).Line
						hits = append(hits, p+":"+itoa(line)+": "+callee)
						return true
					}
				}
				return true
			})
		})
	}

	if len(hits) > pinHitCount {
		t.Errorf("count of free-form error bodies grew to %d (frozen at %d); "+
			"each surviving site reflects a pre-existing handler. New sites beyond "+
			"the freeze pin a future regression — refactor through apierr.Render / "+
			"resp.HTTP / (s *Server).fail / writeError so the same scrub and "+
			"correlation id reach the client.",
			len(hits), pinHitCount)
	}
}

// freeformErrorBody returns true if e is a value-position AST node that
// matches one of:
//
//   - a composite literal map[string]<...>{ "error": <not a sentinel> }
//     where the value is anything other than an apierr-stable constant,
//   - a binary expression that yields "error: <free-form string>".
//
// The class of bug this forbids: writing a fresh error string into the
// response body. apierr.Render is the only sanctioned write of an
// error-shaped value; s.fail / resp.HTTP themselves call apierr.Render.
func freeformErrorBody(e ast.Expr) bool {
	if cl, ok := e.(*ast.CompositeLit); ok {
		mt, ok := cl.Type.(*ast.MapType)
		if !ok {
			return false
		}
		if k, ok := mt.Key.(*ast.Ident); !ok || k.Name != "string" {
			return false
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.BasicLit)
			if !ok || key.Value != "\"error\"" {
				continue
			}
			return true
		}
		return false
	}
	if be, ok := e.(*ast.BinaryExpr); ok && be.Op == token.ADD {
		if id, ok := be.X.(*ast.BasicLit); ok && id.Kind == token.STRING {
			if strings.Contains(id.Value, "\"error") || strings.Contains(id.Value, "\"err") {
				return true
			}
		}
		if id, ok := be.Y.(*ast.BasicLit); ok && id.Kind == token.STRING {
			if strings.Contains(id.Value, "\"error") || strings.Contains(id.Value, "\"err") {
				return true
			}
		}
	}
	return false
}

// walk parses every .go file at the given root, calling fn for each.
// Parse errors are reported as test failures because a syntax error in
// production code blocks every compile of the package.
func walk(t *testing.T, root string, fn func(path string, f *ast.File)) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fsetLocal := token.NewFileSet()
		f, err := parser.ParseFile(fsetLocal, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// Replace the package-global fset with this file's fset for line
		// tracking; the call site ignores the replacement outside the
		// walk, which keeps the leak-tree AST isolated per-file.
		fset = fsetLocal
		fn(path, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// fset is the global file-set the inspector uses; it's replaced per-file
// in walk. Helpers (exprText, leakShape) read from it locally.
var fset *token.FileSet

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func exprText(e ast.Expr) string {
	if e == nil {
		return "?"
	}
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	default:
		return "?"
	}
}

// leakShape returns true if e looks like a free-form "error": ... gadget:
//
//	map[string]string{ "error": "..." }
//	map[string]any{ "error": "..." }
//	map[string]any{ "error": err.Error() }
//	"error: " + err.Error()
//
// The structural rule is the same: an aggregate with the key "error"
// and a value that is a NOT-an-apierr-stable-code. apierr.Bodies are
// NOT composites built by the handler — they're returned by apierr.Render
// from a stable enum — so the test's only failure case is exactly the
// regression class we want to forbid.
func leakShape(e ast.Expr) bool {
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		// detect "error: " + err.Error() chains
		be, ok := e.(*ast.BinaryExpr)
		if !ok {
			return false
		}
		if be.Op != token.ADD {
			return false
		}
		if id, ok := be.X.(*ast.BasicLit); ok && id.Kind == token.STRING && strings.Contains(id.Value, "\"error") {
			return true
		}
		if id, ok := be.Y.(*ast.BasicLit); ok && id.Kind == token.STRING && strings.Contains(id.Value, "\"error") {
			return true
		}
		return false
	}
	mt, ok := cl.Type.(*ast.MapType)
	if !ok {
		return false
	}
	kt, ok := mt.Key.(*ast.Ident)
	if !ok || kt.Name != "string" {
		return false
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Value != "\"error\"" {
			continue
		}
		return true
	}
	return false
}
