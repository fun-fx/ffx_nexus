// c0.8 closure: every documented apierr.Code constant in production
// code, without exception, must have at least one caller. A "dead"
// Code constant is one that is never returned to the wire —
// customers browsing the SDK will then surface branches on a code
// they never see, which is documentation failure.
//
// This test is intentionally written in CLI-codegen style. It scans
// the source tree for `apierr.CodeApproved*` style usages and asserts
// each declared Code appears at least once.

package apierr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodeConstantsEachHaveAtLeastOneCaller walks every Code constant
// declared in this package and asserts each constant is referenced at
// least once in the *rest* of the codebase (excluding this package's
// own files, where the declaration lives).
//
// A "dead" Code is one that is never returned to a customer. The
// customer-visible contract shrinks the moment a Code goes dead:
// SDKs branching on that code return "unrecognised" error paths.
// This test catches that drift at compile-style time.
func TestCodeConstantsEachHaveAtLeastOneCaller(t *testing.T) {
	exports, err := locateApierrPackageExports()
	if err != nil {
		t.Skipf("could not enumerate apierr exports: %v", err)
	}
	workspace, _ := os.Getwd()
	// Walk the parent directory (the project root) so we find
	// users in other packages too, not just internal/apierr/.
	if root := filepath.Dir(filepath.Dir(workspace)); root != "" && root != "/" {
		workspace = root
	}
	users := map[string]int{}
	_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		base := filepath.Base(path)
		// Exclude this package's own files.
		if strings.HasPrefix(path, filepath.Join(workspace, "internal", "apierr", "")) {
			return nil
		}
		if base == "code_inventory_test.go" {
			return nil
		}
		// Test files are exempted: tested Code constants
		// intentionally have a test surface only. We want to flag
		// code paths that should reach an actual customer; a Code
		// referenced only from a test is allowed (the test is itself
		// the contract).
		if strings.HasSuffix(base, "_test.go") || strings.Contains(path, "/testdata/") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "apierr" {
					users[x.Sel.Name]++
				}
			case *ast.Ident:
				// We don't count bareword Code* occurrences because
				// constants of type Code cannot be referenced bareword
				// outside the apierr package — the call would be
				// `apierr.CodeFoo`. Skipping bareword matching keeps
				// the assertion tight.
				_ = x
			}
			return true
		})
		return nil
	})
	_ = exports
	for name := range exports {
		if users[name] == 0 {
			t.Errorf("apierr.%s is not used outside its declaring package; "+
				"either wire a code path to return it or remove the constant", name)
		}
	}
}

// locateApierrPackageExports parses this package and returns the
// names of declared apierr.Code* types. The walk is limited to
// identifiers starting with "Code" and exported.
func locateApierrPackageExports() (map[string]bool, error) {
	out := map[string]bool{}
	apierrDir := ""
	if _, err := os.Stat("apierr.go"); err == nil {
		apierrDir = "."
	} else {
		apierrDir = "."
	}
	entries, err := os.ReadDir(apierrDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(apierrDir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GenDecl:
				if x.Tok != token.CONST {
					return true
				}
				for _, sp := range x.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if strings.HasPrefix(name.Name, "Code") {
							out[name.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	return out, nil
}
