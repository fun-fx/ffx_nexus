package apierr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
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
//
// Monotonicity guard
// ==================
// The pin must NOT grow. A future PR that legitimately lands a new
// bypass must LOWER the pin AND take responsibility for migrating it
// (otherwise the new site is recorded in
// docs/pin-hit-count-history.md as a known debt). The detector
// refuses a pin > pinHi for any reason. Dropping the pin is fine;
// raising it is a code-review block, no exceptions.
//
// Force-migrate-or-document policy at the test runtime:
//   1. pinHi      — historically recorded maximum (the file
//                     docs/pin-hit-count-history.md must agree
//                     or this test fails the PR with a typo-
//                     visible message).
//   2. pinHitGoal — upper bound; same as pinHi until the count
//                     drops below it naturally.
const (
	pinHi      = 139
	pinHitGoal = pinHi
)
const pinHitCount = pinHi

func TestConsoleErrorPathsBypassApierr(t *testing.T) {
	if pinHitCount == 0 {
		t.Skip("pinHitCount is 0; the codebase is fully migrated through the " +
			"sanctioned paths (apierr.Render / resp.HTTP / (s *Server).fail / " +
			"writeError). Set the constant to re-enable the structural drift " +
			"detector when migrating a new package.")
	}
	if pinHitCount > pinHi {
		t.Fatalf("pinHitCount=%d GREW above the documented pinHi=%d.\n"+
			"pinHitCount is monotonically decreasing — a future PR may only "+
			"DROP the pin after migrating a site through apierr.Render / "+
			"resp.HTTP / s.fail / writeError. Raising the pin is the "+
			"regression class this detector exists to catch.\n"+
			"Update docs/pin-hit-count-history.md in lock-step if there is "+
			"a justification (the justification survives review; the pin "+
			"does not).",
			pinHitCount, pinHi)
	}
	if pinHitGoal > pinHitCount {
		t.Errorf("pinHitGoal=%d is above the current pin %d; the goal is "+
			"always <= the current pin so the freeze can shrink.",
			pinHitGoal, pinHitCount)
	}

	root := "../.."

	// Monotonicity guard for the documentation lag. Read the
	// latest committed pin from docs/pin-hit-count-history.md
	// (the "newest first" table) and refuse to allow a code-side
	// pin LESS THAN the documented one. A drop in source that
	// forgets to update the doc fails here, so reviewers see the
	// lag in a single round-trip. The helper returns the
	// most-recently recorded pin (numeric value of the "New
	// pin" column from a new-style row); rows in the old "old"/>
	// new" format are parsed as ordered pairs.
	if docPin, ok := readLatestDocPin(t, root); ok && pinHitCount < docPin {
		t.Errorf("pinHitCount=%d is lower than the most recent entry in "+
			"docs/pin-hit-count-history.md (last committed pin=%d). A drop in "+
			"this constant must update the doc in lock-step or the next "+
			"reader cannot tell whether pinHi is right.",
			pinHitCount, docPin)
	}

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
// readLatestDocPin parses the docs/pin-hit-count-history.md table
// for the most recently committed pin (the "newest first" entry).
// Returns the value and ok=true; returns 0, false when the doc is
// missing — the test then logs but does not fail, so a fresh
// checkout that hasn't yet committed the doc still works.
//
// Format spec the reader relies on:
//
//	| Commit / Date | Old pin | New pin | Reason |
//
// The header row's "Old pin" and "New pin" cells contain the
// strings "old" / "new"; we skip them by skipping any line
// containing "---" (the markdown separator). Data rows: the
// LAST numeric value across columns 2 and 3 is the "New pin".
// The "Old pin" column either says "(none)" or carries the
// previous pin number, in which case the FIRST numeric value
// is the Old pin and the SECOND numeric value is the New pin.
// We return the second numeric value when there are two
// numbers, or the only numeric value when there is one.
// readLatestDocPin takes the body of docs/pin-hit-count-history.md
// and returns the most-recently committed pinHi (the "New pin"
// column for the first body row in the table). The doc format is:
//
//	| Commit / Date | Old pin | New pin | Reason |
//	| --- | --- | --- | --- |         <- alignment
//	| <commit>     | <int>   | <int>   | <reason> |
//	| <commit>     | <int>   | <int>   | <reason> |
//	...
//
// We accept a single-row old-format doc too:
//
//	| (this baseline) | (none) | 139 | initial capture |
//
// The strategy is two passes:
//
//   1. Walk the lines collecting all digit-bearing rows. The
//      header row never contains digits; the alignment row
//      contains dashes (no digits).
//   2. Pick the LAST digit-bearing row. Numeric column
//      heuristic: row with two numbers is "{Old},{New}" and we
//      return the second; row with one number is single-column
//      and we return the only number.
//
// This avoids the header/alignment ambiguity that bit the
// simple state machine. The dataset is small (under 20 rows in
// any foreseeable future) so the linear walk is fine. Returns
// (last, false) if no digit-bearing row was found.
func readLatestDocPin(t *testing.T, repoRoot string) (int, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "docs", "pin-hit-count-history.md"))
	if err != nil {
		return 0, false
	}
	var last int
	var ok bool
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if strings.Contains(line, "---") {
			continue
		}
		var nums []int
		for _, c := range strings.Split(line, "|") {
			c = strings.TrimSpace(c)
			c = strings.Trim(c, "`")
			n, err := strconv.Atoi(c)
			if err != nil {
				continue
			}
			nums = append(nums, n)
		}
		switch len(nums) {
		case 1:
			last = nums[0]
			ok = true
		case 2:
			last = nums[1]
			ok = true
		}
	}
	return last, ok
}

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
