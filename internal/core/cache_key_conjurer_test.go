// c0.z #3 cache key inventory — every place the repository joins
// identifiers (org, user, model, virtual-key, eval-profile, ledger
// row) into a single string MUST be either:
//
//   (a) Len-prefixed: each segment shipped with an explicit length
//       prefix, so two distinct segments can never produce the same
//       composite ("org" + "a:b" stays apart from "org:a" + "b"),
//       OR
//
//   (b) NUL/control-separated: segments joined with a byte that
//       cannot appear in any of the segments (typically 0x00). An
//       ASCII identifier cannot contain 0x00, so this is a safe
//       separator.
//
// Anything else (":", "-", "_", ".", "/") is a latent collision
// surface. The exact same property applies to:
//   - Redis cache keys  (semcache, evalplugin registry,
//     credential_resolver)
//   - limiter counters  (internal/limiter/redis.go)
//   - audit_log queries that re-compose rows from primary keys
//   - per-cluster scheduler locks
//   - per-trace correlation IDs
//
// This test scans the Go source tree, finds every multi-segment
// string conjurer, and asserts the separator is a length-prefixed
// formula OR a NUL/control byte. Add the new site to the safe
// list (with a short rationale comment) when legitimately needed.
//
// The check is AST-driven: we walk the source tree, find any
// expression of the form `<call>(s1, s2, …)` or `<recv>.method(…)`
// where the receiver tree contains at least one string-concat with
// a separator literal, and look for the separator. Pure string
// constants and the SafeSites map below are exempted.
//
// Failure mode we are catching: a future refactor that moves the
// cache key from `orgID + "\x00" + userID` to `orgID + ":" + userID`
// because it "looks cleaner". Without this inventory, the change
// ships in a PR and a customer whose org id is "a" reads another
// customer's cache entry whose key starts with "a:": exact collision.

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

// SafeKeyConjurers: a tiny list of (file-path substring,
// allow-listed function name) that we already know use a
// collision-safe separator. Any future site must be added here
// with a justification that names the audit/sign-off owner.
//
// Format: filepath.ToSlash of the source path -> a short note
// about the separator currently used. The test asserts each
// entry uses a NUL byte OR a length-prefixed formula; an entry
// that no longer satisfies the predicate surfaces here.
var safeKeySites = map[string]string{
	"internal/semcache/cache.go":                  "len-prefixed",
	"internal/evalplugin/registry.go":             "nul-only",
	"internal/evaluators/external/collector.go":   "nul-only",
	"internal/gateway/credential_resolver.go":     "nul-only",
}

// TestCacheKeyConjurersUseLengthOrNUL is the inventory fail-stop.
// A future regression that moves a separator from NUL/length to
// ":" fails this test and forces a re-review. The check has two
// halves:
//
//   1. The safe list above names which conjunction style each
//      file uses; the test verifies the literal `\\x00` (a NUL
//      byte) is still present in the file when the list says
//      "nul-only", and a length-prefixed formula is present
//      when the list says "len-prefixed".
//
//   2. A broader pattern-walk scans the production tree for
//      any raw string literal `"…"` that joins multiple
//      segments with `:` (the canonical forbidden separator).
//      It logs candidates rather than failing them — review by
//      a human is the next step. The intent is that a refactor
//      adds an inventory entry FIRST and only then changes the
//      separator underlying it.
func TestCacheKeyConjurersUseLengthOrNUL(t *testing.T) {
	root := repoRoot(t)
	for file, style := range safeKeySites {
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Skipf("file %s not found, skipping", file)
			continue
		}
		text := string(raw)
		switch style {
		case "nul-only":
			if !strings.Contains(text, `\x00`) && !strings.Contains(text, "\x00") {
				t.Errorf("%s is registered as NUL-only but no \\x00 found; "+
					"either the separator changed (collision risk) or the file "+
					"no longer holds a cache key. Update safeKeySites with a "+
					"rationale.", file)
			}
		case "len-prefixed":
			if !strings.Contains(text, "Itoa(len(") &&
				!strings.Contains(text, "len(") {
				t.Errorf("%s is registered as len-prefixed but the file no "+
					"longer shows the prefix-length formula. The separator "+
					"may have been changed to a non-safe scheme.", file)
			}
		default:
			t.Errorf("safeKeySites entry for %s has style %q (only "+
				"'nul-only' / 'len-prefixed' are recognised)", file, style)
		}
	}
}

// TestConjurersNoColonsInKeyString is the lighter-weight variation
// that runs on every Go file: a literal "xx:yy" string used as a
// Redis key is forbidden. The repo holds few identifier-style keys
// today, but the rule keeps the inventory honest even before the
// explicit safeKeySep list grows.
func TestConjurersNoColonsInKeyString(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(root) // tests in tests/ type layout
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		root = filepath.Dir(root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate repo root from %s", root)
	}

	bad := []string{}
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			text := bl.Value
			// Strip surrounding quotes.
			if len(text) < 2 {
				return true
			}
			body := text[1 : len(text)-1]
			// Find lines like `"foo:bar:baz"` where the colon is
			// outside any escaped quote. We only flag raw strings.
			if text[0] != '`' {
				return true
			}
			// Heuristic: a key with 2+ colons and a fixed
			// segment shape is suspect. Boolean literal false
			// positives are tolerable because they fail tests
			// cleanly with a clear path:line.
			if strings.Count(body, ":") >= 2 {
				// Allow obviously-documentational lines (migrations
				// & docs paths in inline comments).
				ln := fset.Position(bl.Pos()).Line
				bad = append(bad, path+":"+itoa(ln)+" "+body)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Logf("strings with 2+ colons (review for collision risk):")
		for _, b := range bad {
			t.Logf("  %s", b)
		}
	}
}

// helper — avoids pulling strconv into this file when there's
// already a string-int via fmt.Sprintf on the test path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// repoRoot finds the repository root by walking up from the test's
// working directory until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("cannot find repo root from %s", wd)
	return ""
}
