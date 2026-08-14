package docs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempDocs swaps the package root onto a temp dir for the
// lifetime of one test. Restoring the previous root matters because
// the package-level cache persists across tests; leaving the temp
// dir in place would leak files into the test runner's cwd on every
// subsequent run.
func withTempDocs(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", full, err)
		}
	}
	prev := rootDir
	t.Cleanup(func() {
		rootDir = prev
		BuiltReindex()
	})
	if err := SetSourceDir(dir); err != nil {
		t.Fatalf("SetSourceDir: %v", err)
	}
}

// BuiltReindex resets the cached index after a root change. The
// production SetSourceDir already reindexes the cached `built`
// value, so this is only needed after the cleanup function reverts
// rootDir back to the previous root — the swap leaves `built`
// pointing at the temp dir, so a manual refresh is required. The
// error is intentionally swallowed here: the cleanup path is
// only used after a successful SetSourceDir in the same test, so
// a transient walk failure would be a code change, not a test
// concern that this surface is meant to flag.
func BuiltReindex() {
	builtSet = false
	var err error
	built, err = Build()
	_ = err
	builtErr = nil
}

func TestIndexCategoryInference(t *testing.T) {
	withTempDocs(t, map[string]string{
		"quickstart.md":           "# Quickstart\n\nFive minutes to first call.\n",
		"onboarding.md":           "# Onboarding\n\nWelcome aboard.\n",
		"enterprise-model.md":     "# Enterprise model\n\nBYO keys, multi-tenant.\n",
		"model-benchmarks.md":     "# Model benchmarks\n\nDistributed eval.\n",
		"eval-plugins.md":         "# Eval plugins\n\nPer-trace scoring.\n",
		"packaging.md":            "# Packaging\n\nHelm chart walkthrough.\n",
		"observability/README.md": "# Observability\n\nOTLP pipelines.\n",
		"release-notes/v0.1.0.md": "# v0.1.0\n\nPilot release.\n",
		"adr/0001-foo.md":         "# 0001\n\nKeep model ids.\n",
	})

	idx := List()

	// Quick links subset: the six pinned titles should be promoted
	// when matches exist; the absence of any one ticket should
	// leave the others in place rather than dropping the whole
	// grid.
	wantQuick := []string{"Quickstart", "Onboarding", "Enterprise model", "Model benchmarks", "Eval plugins", "Packaging"}
	if len(idx.QuickLinks) != len(wantQuick) {
		t.Fatalf("quick links: want %d, got %d (%v)", len(wantQuick), len(idx.QuickLinks), quickTitles(idx))
	}
	for i, title := range wantQuick {
		if idx.QuickLinks[i].Title != title {
			t.Fatalf("quick link %d: want %q, got %q", i, title, idx.QuickLinks[i].Title)
		}
	}

	// Category inference: top-level files default to "concepts";
	// nested directories default to "operations" / "release-notes"
	// / "adr" based on the first segment.
	catBuckets := map[string][]string{}
	for _, c := range idx.Categories {
		for _, e := range c.Entries {
			catBuckets[c.Slug] = append(catBuckets[c.Slug], e.Path)
		}
	}
	wantMatch := func(want, got []string) bool {
		if len(want) != len(got) {
			return false
		}
		for i := range want {
			if !strings.Contains(strings.Join(got, ","), want[i]) {
				return false
			}
		}
		return true
	}
	gotConcepts := strings.Join(catBuckets["concepts"], ",")
	conceptsWant := []string{"quickstart", "onboarding", "enterprise-model", "model-benchmarks", "eval-plugins", "packaging"}
	if !wantMatch(conceptsWant, catBuckets["concepts"]) {
		t.Fatalf("concepts bucket missing a file: %s", gotConcepts)
	}
	if got := strings.Join(catBuckets["operations"], ","); got != "observability/README" {
		t.Fatalf("operations bucket: %s", got)
	}
	if got := strings.Join(catBuckets["release-notes"], ","); got != "release-notes/v0.1.0" {
		t.Fatalf("release-notes bucket: %s", got)
	}
	if got := strings.Join(catBuckets["adr"], ","); got != "adr/0001-foo" {
		t.Fatalf("adr bucket: %s", got)
	}
}

func TestFrontMatterOverrides(t *testing.T) {
	withTempDocs(t, map[string]string{
		`a.md`: "<!--\ncategory: reference\ntitle: Override A\norder: 9\n-->\n# Override A\n\nFirst para.\n",
		"b.md": "# Override B\n\nSecond one.\n",
	})

	idx := List()
	var a Entry
	for _, c := range idx.Categories {
		for _, e := range c.Entries {
			if e.Path == "a" {
				a = e
			}
		}
	}
	if a.Category != "reference" {
		t.Fatalf("front-matter category override not honoured: got %q", a.Category)
	}
	if a.Order != 9 {
		t.Fatalf("front-matter order not honoured: got %d", a.Order)
	}
	if a.Title != "Override A" {
		t.Fatalf("front-matter title not honoured: got %q", a.Title)
	}
}

func TestGetPageReturnsBody(t *testing.T) {
	withTempDocs(t, map[string]string{
		"hello.md": "# Hello\n\nWorld.\n",
	})
	page, err := Get("hello")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if page.Title != "Hello" {
		t.Fatalf("title: %q", page.Title)
	}
	if !strings.Contains(page.Body, "World.") {
		t.Fatalf("body missing text: %q", page.Body)
	}
}

func TestGetPageMissingIsError(t *testing.T) {
	withTempDocs(t, map[string]string{})
	if _, err := Get("does-not-exist"); err == nil {
		t.Fatal("expected error for missing slug")
	}
}

func TestRoutesJSONShape(t *testing.T) {
	withTempDocs(t, map[string]string{
		"a.md": "# A\n\nEntry A.\n",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var idx Index
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(idx.Categories) == 0 {
		t.Fatal("categories missing")
	}
}

func TestRoutesSlugReturnsBody(t *testing.T) {
	withTempDocs(t, map[string]string{
		"a.md": "# A\n\nEntry A body.\n",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a", nil)
	Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Entry A body.") {
		t.Fatalf("body mismatch: %s", rec.Body.String())
	}
}

func TestRoutesNotFoundReturnsErrorJSON(t *testing.T) {
	withTempDocs(t, map[string]string{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no entry") {
		t.Fatalf("error body: %s", rec.Body.String())
	}
}

// quickTitles turns a slice of entries into the bare titles, so
// assertion failure messages say "want Onboarding, got Onboarding"
// instead of dumping whole Entry records.
func quickTitles(idx Index) []string {
	out := make([]string, len(idx.QuickLinks))
	for i, e := range idx.QuickLinks {
		out[i] = e.Title
	}
	return out
}

// TestBootReportsMissingRoot documents the contract main.go
// relies on: when the configured docs root is missing, Err()
// returns a non-nil error so the boot logs print
// "docs: NEXUS_DOCS_DIR is set but not walkable" exactly once. A
// silent failure used to expose an empty /api/docs response that
// looked identical to a healthy response with no docs in the
// tree. The test catches both regressions: Err returning nil
// when walk failed, and Err returning an error when walk
// succeeded.
func TestBootReportsMissingRoot(t *testing.T) {
	// Start from a clean cache so SetSourceDir's walk failure is
	// the sole contribution to builtErr.
	prev := rootDir
	t.Cleanup(func() {
		rootDir = prev
		BuiltReindex()
	})

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := SetSourceDir(missing); err == nil {
		t.Fatal("SetSourceDir against a missing dir should fail")
	}
	if Err() == nil {
		t.Fatal("Err() should be non-nil when the walk fails — this is what the boot log greps for")
	}
}

// TestBootReportsSuccess documents the happy-path complement:
// when the docs root is walkable, Err() returns nil so the boot
// log prints "serving from …" rather than the error banner.
func TestBootReportsSuccess(t *testing.T) {
	withTempDocs(t, map[string]string{
		"hello.md": "# Hello\n",
	})
	if Err() != nil {
		t.Fatalf("Err() should be nil when walk succeeded, got %v", Err())
	}
}

// TestEntriesAlwaysJSONArray locks the wire contract: a Category whose
// directory contributed zero files must still serialize with an empty
// `entries` array rather than JSON `null`. A nil Go slice marshals to
// null, and the React docs page crashes on `.map((e) => …)` against a
// null entries value with `Cannot read properties of null (reading
// 'length')` — which presented as a black screen on the live cluster
// the first time a freshly indexed page was opened. Initialising with
// `[]Entry{}` in walk() pins the shape so a downstream type-narrowing
// the absence of files is enough; the consumer never has to special-case
// null again.
func TestEntriesAlwaysJSONArray(t *testing.T) {
	withTempDocs(t, map[string]string{
		"only-in-concepts.md": "# Concepts only\n",
	})

	idx, err := walk(rootDir)
	if err != nil {
		t.Fatalf("walk() failed: %v", err)
	}

	for _, c := range idx.Categories {
		if c.Entries == nil {
			t.Errorf("category %q has nil Entries — should be an empty slice", c.Slug)
		}
	}
}
