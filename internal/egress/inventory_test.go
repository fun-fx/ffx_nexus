package egress_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This is the mechanism that catches the SIXTH egress path.
//
// The route-policy inventory in internal/console works because a new route with
// no declared policy fails the build rather than shipping unauthenticated. The
// same problem exists here in a worse form: adding an outbound call is one line,
// `&http.Client{Timeout: x}`, and nothing about writing that line prompts anyone
// to think about where the URL points. Five separate paths did it independently.
//
// So every construction of an HTTP client in the repository is declared below
// with the reason it is or is not routed through the guard. A new one fails this
// test with instructions. The failure is the point: it forces a decision at the
// moment the path is written, when the author knows whether the destination is
// operator or tenant controlled, instead of during the next security review.

// clientSite is one place a raw *http.Client comes into existence.
type clientSite struct {
	// Guarded marks the guard's own construction. Only egress.go sets it.
	Guarded bool
	// Why explains why this file is allowed to build a client directly instead
	// of calling egress.Client.
	Why string
}

// declaredSites lists every file that still constructs an http.Client itself,
// keyed by repo-relative path.
//
// A file that has been converted to egress.Client does NOT appear here, and that
// is the design: converting a path removes its composite literal, so it drops out
// of the detector's results and needs no entry. The list therefore shrinks as
// paths are converted, and only ever contains the exceptions plus the guard.
//
// An entry is an assertion that the destination can never be influenced by
// configuration or by an API caller. A hardcoded vendor constant qualifies. "It
// is only used internally" does not: a base_url override is precisely how an
// internal destination becomes a tenant-controlled one.
var declaredSites = map[string]clientSite{
	// ---- The guard itself --------------------------------------------------
	"internal/egress/egress.go": {Guarded: true,
		Why: "constructs the guarded client that every other path obtains " +
			"through egress.Client"},

	// ---- Exempt: not a configurable destination ----------------------------
	"internal/gateway/providers/pool.go": {Guarded: false,
		Why: "builds the shared pooled transport for LLM upstream calls. The " +
			"upstream IS the product's purpose and its destination is the org's " +
			"own credential base_url, checked separately at credential-save time " +
			"by the console. Routing user chat traffic through the guard's dialer " +
			"would add a per-connection policy check to the hot path for a " +
			"destination the org is authorised to reach with its own key."},

	"internal/gateway/providers/openai.go": {Guarded: false,
		Why: "same as pool.go; installs the cross-origin redirect auth strip for " +
			"OpenAI-compatible upstreams."},

	"internal/gateway/model_fetcher.go": {Guarded: false,
		Why: "model-list sync against the same upstreams as pool.go, enabled only " +
			"by the operator flag NEXUS_DYNAMIC_MODEL_SYNC."},

	"internal/semcache/embed.go": {Guarded: false,
		Why: "embedding calls for the semantic cache, to the operator-set " +
			"NEXUS_EMBEDDINGS_URL. Sends prompt text, so it is on the review list, " +
			"but the destination is operator-only and on the request hot path."},

	"cmd/nexus-evalbatch/main.go": {Guarded: false,
		Why: "a developer CLI that takes its gateway and service URLs from flags. " +
			"Not part of the server; runs with the operator's own privileges."},
}

// httpClientPackages are the import paths whose files this test walks. Adding a
// package that makes outbound calls means adding it here.
var scannedDirs = []string{
	"cmd", "internal",
}

func TestEveryHTTPClientConstructionIsDeclared(t *testing.T) {
	root := repoRoot(t)
	found := map[string][]int{} // relative path -> line numbers

	for _, dir := range scannedDirs {
		walkGoFiles(t, filepath.Join(root, dir), func(rel string, fset *token.FileSet, f *ast.File) {
			for _, line := range httpClientLines(fset, f) {
				found[rel] = append(found[rel], line)
			}
		})
	}

	var undeclared []string
	for rel, lines := range found {
		if _, ok := declaredSites[rel]; !ok {
			undeclared = append(undeclared, formatSite(rel, lines))
		}
	}
	sort.Strings(undeclared)

	if len(undeclared) > 0 {
		t.Fatalf(`%d file(s) construct an HTTP client without a declaration in
internal/egress/inventory_test.go:

%s

An outbound HTTP call whose destination comes from configuration must go through
egress.Guard.Client. The guard supplies:

  - a destination address check, applied after DNS resolution at connect time,
    so a hostname that resolves into the cluster or to the cloud metadata
    service is refused;
  - a mandatory timeout, because Go's zero value means wait forever;
  - a bounded redirect chain with credentials dropped across hosts.

Two existing paths POST prompt content to a tenant-supplied URL and persist the
response as an evaluation rationale, so an unguarded fetch there returns the
pod's IAM credentials to the console. That is why this is a build failure and
not a lint warning.

Converting a path removes its http.Client literal, so it needs no entry here —
it simply stops appearing. Only add to declaredSites if the destination genuinely
cannot be configured, e.g. a hardcoded vendor constant, and say why. "Internal use
only" is not a reason: a base_url override is how internal destinations become
tenant-controlled.`,
			len(undeclared), strings.Join(undeclared, "\n"))
	}

	// The inventory must not accumulate entries for files that no longer exist,
	// or it stops describing the code.
	for rel := range declaredSites {
		if _, ok := found[rel]; !ok {
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				t.Errorf("declaredSites lists %s, which does not exist", rel)
				continue
			}
			t.Errorf("declaredSites lists %s, but it no longer constructs an HTTP "+
				"client. Remove the entry so the inventory keeps matching the code.", rel)
		}
	}
}

// Every exemption must carry a justification. A blank Why is how an exemption
// list becomes a place to make problems disappear.
func TestEveryExemptionExplainsItself(t *testing.T) {
	for rel, site := range declaredSites {
		if site.Guarded {
			continue
		}
		if len(strings.TrimSpace(site.Why)) < 40 {
			t.Errorf("%s is exempt from the egress guard with no substantive "+
				"justification (Why=%q). State why the destination cannot be "+
				"configured by an operator or an API caller.", rel, site.Why)
		}
	}
}

// The inventory is only worth having if it actually detects a new site. This
// proves the AST matcher sees the shapes it claims to.
func TestTheDetectorSeesEachClientShape(t *testing.T) {
	cases := map[string]string{
		"composite literal":        `package p; import "net/http"; var c = &http.Client{}`,
		"non-pointer literal":      `package p; import "net/http"; var c = http.Client{}`,
		"literal with fields":      `package p; import "net/http"; import "time"; var c = &http.Client{Timeout: time.Second}`,
		"DefaultClient reference":  `package p; import "net/http"; var c = http.DefaultClient`,
		"package-level Get":        `package p; import "net/http"; func f() { http.Get("http://x") }`,
		"package-level Post":       `package p; import "net/http"; func f() { http.Post("http://x", "", nil) }`,
		"inside a struct literal":  `package p; import "net/http"; type S struct{ C *http.Client }; var s = S{C: &http.Client{}}`,
		"returned from a function": `package p; import "net/http"; func f() *http.Client { return &http.Client{} }`,
	}
	for name, src := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if len(httpClientLines(fset, f)) == 0 {
			t.Errorf("the detector does not see %s; a new egress path written this "+
				"way would slip past the inventory", name)
		}
	}

	// And it must not fire on unrelated code, or the inventory becomes noise
	// that people disable.
	for name, src := range map[string]string{
		"a server":       `package p; import "net/http"; var s = &http.Server{}`,
		"a request":      `package p; import "net/http"; func f() { http.NewRequest("GET", "http://x", nil) }`,
		"a handler":      `package p; import "net/http"; var h http.HandlerFunc`,
		"a status code":  `package p; import "net/http"; var c = http.StatusOK`,
		"another Client": `package p; type client struct{}; var c = &client{}`,
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if lines := httpClientLines(fset, f); len(lines) > 0 {
			t.Errorf("the detector fires on %s (lines %v), which is a false positive", name, lines)
		}
	}
}

// ---- detector ------------------------------------------------------------

// httpClientLines reports the lines in f that bring an HTTP client into
// existence: an http.Client composite literal, a reference to http.DefaultClient,
// or one of the package-level convenience functions that use it.
func httpClientLines(fset *token.FileSet, f *ast.File) []int {
	var lines []int
	add := func(pos token.Pos) { lines = append(lines, fset.Position(pos).Line) }

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if isHTTPSelector(node.Type, "Client") {
				add(node.Pos())
			}
		case *ast.SelectorExpr:
			if isHTTPIdent(node.X) {
				switch node.Sel.Name {
				case "DefaultClient", "Get", "Post", "Head", "PostForm":
					add(node.Pos())
				}
			}
		}
		return true
	})
	sort.Ints(lines)
	return lines
}

func isHTTPSelector(expr ast.Expr, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && isHTTPIdent(sel.X) && sel.Sel.Name == name
}

func isHTTPIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "http"
}

// ---- walking -------------------------------------------------------------

func walkGoFiles(t *testing.T, dir string, fn func(rel string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	root := repoRoot(t)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "testdata" || d.Name() == "vendor" {
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
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fn(filepath.ToSlash(rel), fset, f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/egress -> repo root
	root := filepath.Dir(filepath.Dir(wd))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate the repository root from %s", wd)
	}
	return root
}

func formatSite(rel string, lines []int) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, itoa(l))
	}
	return "  " + rel + ":" + strings.Join(parts, ",")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
