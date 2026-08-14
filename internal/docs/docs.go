// Package docs hosts the documentation tree served by the console.
// The source lives in /docs at the repo root (operational runbooks,
// design notes, release letters) and is read from disk at startup.
// The same files serve two audiences:
//
//   - The console UI renders them as The-Grid-docs-style markdown
//     pages with a sidebar, hero, \"Quick Links\" grid and an
//     on-this-page TOC. The binary stays in charge of source-of-truth;
//     the front-end just renders.
//
//   - External agents (CI bots, the docs-aware renderer) read the
//     same files verbatim via GET /api/docs/{path}, mirroring
//     /docs/<page>.md on thegrid.ai.
//
// Reading from disk (rather than //go:embed) keeps edits inside the
// repo authoritative without forcing a binary rebuild for typo
// fixes. The index and bodies are walked once at startup; a hot
// reload is not required because docs are reviewed in PRs, so a
// rolling deploy carries every change.
package docs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// SourceDir is the on-disk root the indexer walks. DefaultRoot
// falls back to a path relative to the binary that matches the
// layout of a `go run ./cmd/nexus` invocation; production builds
// set this through SetSourceDir from main.go after consulting
// environment variables.
const DefaultRoot = "docs"

// SetSourceDir rebinds the docs root and rebuilds the cached index.
// Call once at process startup; the package caches the result so
// subsequent requests do not re-walk the disk.
//
// An invalid root (missing or unreadable) keeps the previous
// cached index unchanged so a transient mount problem in a
// container does not silently leave the console serving a stale
// empty index. The most recent failure is also captured in
// builtErr below so the boot logs can surface the reason through
// Err() rather than disguising a config-time mistake as a
// successful empty index.
//
// built has the index the routes / List / Get read. walkErr
// captures the most recent walk failure so /api/docs can surface
// the failure reason through the response body and a future
// boot-time diagnostic can grep the log for it. Before the logs
// existed a missing docs directory silently published a zero-value
// Index and the console would render an empty page that looked
// identical to "everything is fine".
var (
	rootDir  = DefaultRoot
	built    Index
	builtSet bool
	builtErr error
)

// SetSourceDir rebinds SourceDir. main or the API conf loader calls
// this once at startup so a Helm-mounted /docs can override the
// repo-relative default without rebuilding the binary. BuiltErr is
// captured even when the failure is detected before walk() —
// config-time mistakes (missing dir, file instead of dir) are
// exactly what boot logs read back via Err() so an operator can
// diagnose a blank /api/docs response without grepping the pod
// listing for the right SHA.
func SetSourceDir(dir string) error {
	if dir == "" {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		builtErr = fmt.Errorf("docs: %q: %w", dir, err)
		return builtErr
	}
	if !st.IsDir() {
		builtErr = fmt.Errorf("docs: %q is not a directory", dir)
		return builtErr
	}
	rootDir = dir
	idx, werr := walk(rootDir)
	if werr != nil {
		// Roll back to the previous root so a transient failure
		// cannot leave the console serving a half-built index. Boot
		// paths that do not have a previous root live with empty
		// responses until SetSourceDir is retried.
		builtErr = werr
		return werr
	}
	built = idx
	builtSet = true
	builtErr = nil
	return nil
}

// walk re-derives the index from a directory on disk. Used by
// SetSourceDir and the test harness; production code goes through
// the cached `built` value.
func walk(root string) (Index, error) {
	var idx Index
	idx.Title = "Nexus documentation"
	idx.Tagline = "Everything we know about running an AI gateway " +
		"that holds the line on quality and cost at the same time."
	// bucket maps slug -> index in idx.Categories. We use the index
	// rather than a pointer to the slot because appending to
	// idx.Categories reallocates its backing array and a pointer
	// into the old array would still produce entries, but would not
	// surface inside idx.Categories when the caller reads it. The
	// map keeps the layout flat and the loop in WalkDir stays a
	// single bucket lookup instead of a nested search.
	//
	// Each Category is initialised with an explicit empty Entries slice
	// rather than relying on Go's nil-slice default. json.Marshal emits
	// `null` for a nil slice, and at least one downstream consumer (the
	// React docs UI's `.map((e) => …)` call) crashes when it hits a null
	// instead of `[]`. The wire port "always empty array" matches the
	// "do not pretend a category is missing entries when it's just empty"
	// expectation easy to defend in code review.
	bucket := map[string]int{}
	for i := range CategoryOrder {
		c := CategoryOrder[i]
		idx.Categories = append(idx.Categories, Category{Slug: c.Slug, Title: c.Title, Entries: []Entry{}})
		bucket[c.Slug] = i
	}
	// Fallback for any front-matter category we did not register.
	miscIdx := bucket["misc"]

	var walkErr error
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "_index.md" {
			return nil
		}
		raw, rerr := os.ReadFile(abs)
		if rerr != nil {
			return nil
		}
		text := string(raw)
		fm := parseFrontMatter(text)
		title := fm.Title
		if title == "" {
			title = titleFromBody(rel, text)
		}
		summary := fm.Summary
		if summary == "" {
			summary = summaryFromBody(text)
		}
		category := fm.Category
		if category == "" {
			category = DefaultCategoryFromPath(rel)
		}
		status := fm.Status
		if status == "" {
			status = "stable"
		}
		entry := Entry{
			Path:       strings.TrimSuffix(rel, ".md"),
			Title:      title,
			Summary:    summary,
			Category:   category,
			Order:      fm.Order,
			Status:     status,
			SourcePath: rel,
			Bytes:      len(raw),
		}
		ci, ok := bucket[category]
		if !ok {
			ci = miscIdx
		}
		idx.Categories[ci].Entries = append(idx.Categories[ci].Entries, entry)
		return nil
	})
	if err != nil {
		walkErr = err
	}

	// Promote a small set of \"start here\" entries to the Quick Links
	// grid that headlines the page. Names match titles so editors
	// can rename without breaking the boost logic only by accident;
	// in that case the grid degrades to whatever matching entry is
	// present.
	// Beginner's tour: the six pinned titles promote the docs a
	// first-time reader reaches for. Order matters because the
	// Quick Links grid renders left-to-right and the editor of the
	// docs folder cannot reorder the headings without changing
	// filenames; pinning the order here keeps editorial control.
	quickWant := []string{
		"Quickstart",
		"Onboarding",
		"Enterprise model",
		"Model benchmarks",
		"Eval plugins",
		"Packaging",
	}
	// Quick-want loop walks each desired title in the order the
	// sidebar should display them, finds the first matching entry
	// across all categories, and appends it to the grid. A title
	// absent from the index drops that slot so the grid stays a
	// faithful reflection of /docs rather than a fixed list that
	// lies when a category has fewer entries than six. Walking the
	// flat list of wants — rather than re-sorted index entries —
	// keeps the order editor-controlled: the grid is the order the
	// editor wrote in `quickWant`, not the alphabetical sort that
	// the sidebar itself applies.
	for _, want := range quickWant {
		var best *Entry
	bestSearch:
		for i := range idx.Categories {
			for j := range idx.Categories[i].Entries {
				if strings.EqualFold(idx.Categories[i].Entries[j].Title, want) {
					e := idx.Categories[i].Entries[j]
					best = &e
					break bestSearch
				}
			}
		}
		if best == nil {
			continue
		}
		idx.QuickLinks = append(idx.QuickLinks, *best)
	}

	for i := range idx.Categories {
		sort.SliceStable(idx.Categories[i].Entries, func(a, b int) bool {
			ea, eb := idx.Categories[i].Entries[a], idx.Categories[i].Entries[b]
			if ea.Order != eb.Order {
				return ea.Order < eb.Order
			}
			return strings.ToLower(ea.Title) < strings.ToLower(eb.Title)
		})
	}
	if walkErr != nil {
		return idx, walkErr
	}
	return idx, nil
}

// Entry is one row of the docs index. Path is the URL slug the UI
// navigates and the API serves; the binary is the canonical source.
//
// Status doubles the role of the field found on thegrid.ai's
// release-letter pages: a small \"v1beta\" / \"in review\" pill next
// to each title. Editors set it via front-matter; absent it, the
// builder defaults to \"stable\" so a forgotten tag never hides the
// page from the sidebar.
//
// Order is the relative sort weight within a category. Lower
// numbers come first; equal numbers fall back to title alphabetical
// so a freshly-added page lands at a sensible place without
// renumbering everything.
type Entry struct {
	Path       string  `json:"path"`
	Title      string  `json:"title"`
	Summary    string  `json:"summary,omitempty"`
	Category   string  `json:"category"`
	Order      int     `json:"order"`
	Status     string  `json:"status,omitempty"`
	UpdatedAt  string  `json:"updated_at,omitempty"`
	SourcePath string  `json:"source_path,omitempty"`
	Bytes      int     `json:"bytes"`
	Children   []Entry `json:"children,omitempty"`
}

// Page is the body returned by /api/docs/{path}. SourcePath is the
// absolute path of the underlying markdown file inside the embedded
// tree so an operator inspecting the binary can locate it from the
// API alone.
type Page struct {
	Entry
	Body string `json:"body"`
}

// Category is the sidebar bucket. The frontend renders each as a
// collapsed dropdown that opens onto its entries; matching the
// thegrid.ai/docs layout, where every header is a fold rather than
// a flat list.
type Category struct {
	Slug    string  `json:"slug"`
	Title   string  `json:"title"`
	Entries []Entry `json:"entries"`
}

// Index is the response of GET /api/docs. Categories appear in the
// order determined by CategoryOrder below so the sidebar opens in
// the same sequence on every deployment.
type Index struct {
	Title      string     `json:"title"`
	Tagline    string     `json:"tagline,omitempty"`
	Categories []Category `json:"categories"`
	QuickLinks []Entry    `json:"quick_links"`
}

// CategoryOrder pins the sidebar order. The choice mirrors
// thegrid.ai's \"Start here · Concepts · Operations · Reference\"
// rhyme: a newcomer starts at Quick Links, then walks Concepts
// (the why), Operations (the how), Reference (the lookup tables),
// and finally any Runbooks directory the operator creates.
var CategoryOrder = []struct {
	Slug  string
	Title string
}{
	{"concepts", "Concepts"},
	{"operations", "Operations"},
	{"reference", "Reference"},
	{"runbooks", "Runbooks"},
	{"release-notes", "Release notes"},
	{"adr", "Architecture decisions"},
	{"misc", "Misc"},
}

// DefaultCategoryFromPath maps a /docs/<dir>/... path to one of the
// category slugs above. Top-level files land in \"concepts\"; deeper
// directories are matched by their first segment so a reader can
// drop something into /docs/operations and have it appear in the
// right sidebar without editing any registry.
func DefaultCategoryFromPath(rel string) string {
	seg := strings.Split(strings.TrimPrefix(rel, "/"), "/")[0]
	switch seg {
	case "operations", "observability":
		return "operations"
	case "adr":
		return "adr"
	case "release-notes":
		return "release-notes"
	case "runbooks":
		return "runbooks"
	}
	if strings.Contains(rel, "/") {
		return "operations"
	}
	return "concepts"
}

// frontMatter parses the optional <!-- ... --> comment block at the
// top of a markdown file. Editors use it to override the inferred
// category / status / order without moving the file. The format is
// a constrained subset of YAML so editors can keep the comments
// readable:
//
//	<!--
//	category: reference
//	title: Benchmark reference
//	summary: Everything the router does not decide.
//	order: 5
//	status: v1beta
//	-->
//
// Anything that does not parse is silently ignored so a typo in
// front-matter does not blank the index.
type frontMatter struct {
	Title    string `yaml:"title"`
	Summary  string `yaml:"summary"`
	Category string `yaml:"category"`
	Status   string `yaml:"status"`
	Order    int    `yaml:"order"`
}

func parseFrontMatter(raw string) frontMatter {
	var fm frontMatter
	raw = strings.TrimPrefix(raw, "\ufeff")
	if !strings.HasPrefix(raw, "<!--") {
		return fm
	}
	end := strings.Index(raw, "-->")
	if end < 0 {
		return fm
	}
	inner := raw[len("<!--"):end]
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return fm
	}
	_ = yaml.Unmarshal([]byte(inner), &fm)
	return fm
}

// titleFromBody pulls the first H1 of a markdown file. The /docs
// convention is exactly one H1 per file; a file without one falls
// back to the basename so the sidebar never reads \"(untitled)\".
func titleFromBody(rel, raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "# "))
		}
	}
	return strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
}

// summaryFromBody returns the first paragraph of a markdown file,
// trimmed of headline artefacts (>", *>"). Lets the sidebar render a
// one-line teaser without each editor having to set the field
// explicitly.
func summaryFromBody(raw string) string {
	seen := false
	for _, line := range strings.Split(raw, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			if seen {
				break
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "#"),
			strings.HasPrefix(l, "<!--"),
			strings.HasPrefix(l, ">"),
			strings.HasPrefix(l, "*"),
			strings.HasPrefix(l, "-"),
			strings.HasPrefix(l, "```"),
			strings.HasPrefix(l, "|"):
			continue
		}
		seen = true
		l = strings.TrimSuffix(l, ".")
		if len(l) > 160 {
			l = strings.TrimSpace(l[:160]) + "…"
		}
		return l
	}
	return ""
}

// Build walks the docs tree and produces the index. The result is
// cached at first call; otherwise callers re-read `built` after
// SetSourceDir replaces it. Reading off disk every request would
// let a typo in a single file under a 10k-line docs tree allocate
// string copies in the GC by the minute, which a hot path through
// /api/docs cannot afford.
//
// If the boot-time walk never succeeded, Build returns the zero
// Index alongside `builtErr` so callers can both log the failure
// and publish the empty category list rather than crash the
// console. /api/docs reads `built` directly because main.go logs
// builtErr at startup; this function exists for the testable
// surface only.
func Build() (Index, error) {
	if !builtSet {
		idx, err := walk(rootDir)
		if err == nil {
			built = idx
			builtSet = true
			builtErr = nil
		} else {
			builtErr = err
		}
	}
	return built, builtErr
}

// List is the response of GET /api/docs. Reads through `built`
// rather than a separate cache so SetSourceDir's in-place update
// is visible to callers without needing a second reindex call.
func List() Index { return built }

// Err returns the most recent walk failure or nil when the index is
// healthy. main.go calls this before the console starts listening
// so a missing NEXUS_DOCS_DIR or empty /docs/ mount is loud at boot
// instead of disguising itself as a blank page in the renderer.
func Err() error { return builtErr }

// Get reads the body of /docs/<slug>.md and packages it with the
// matching Entry from the index. A missing slug returns an error
// rather than an empty body so the console can show a real 404
// panel instead of an empty page.
func Get(slug string) (Page, error) {
	slug = strings.TrimPrefix(slug, "/")
	slug = strings.TrimSuffix(slug, "/")
	if slug == "" {
		return Page{}, fmt.Errorf("docs: empty slug")
	}
	rel := slug + ".md"
	for _, cat := range built.Categories {
		for _, e := range cat.Entries {
			if e.Path != slug {
				continue
			}
			body, err := os.ReadFile(filepath.Join(rootDir, rel))
			if err != nil {
				return Page{}, fmt.Errorf("docs: read %q: %w", rel, err)
			}
			page := Page{
				Entry: e,
				Body:  string(body),
			}
			return page, nil
		}
	}
	return Page{}, fmt.Errorf("docs: no entry for %q", slug)
}

// Routes wires /api/docs onto a chi router. The two endpoints
// mirror thegrid.ai/docs's \"index for humans, slug for everything\"
// surface: GET /api/docs returns the sidebar catalogue, GET
// /api/docs/{slug} returns the same markdown the git tree has.
//
// The handler is admin-light: any authenticated console reader is
// fine. Documentation is not a secret and the path surface is
// short, so we leave auth on the surrounding Server middleware.
func Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", writeJSONHandler(built))
	r.Get("/llms.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var buf bytes.Buffer
		buf.WriteString("# Nexus documentation index\n\n")
		for _, cat := range built.Categories {
			if len(cat.Entries) == 0 {
				continue
			}
			fmt.Fprintf(&buf, "## %s\n", cat.Title)
			for _, e := range cat.Entries {
				fmt.Fprintf(&buf, "- [%s](%s)\n", e.Title, e.Path)
			}
			buf.WriteString("\n")
		}
		_, _ = w.Write(buf.Bytes())
	})
	r.Get("/{slug:.+}", func(w http.ResponseWriter, req *http.Request) {
		slug := chi.URLParam(req, "slug")
		page, err := Get(slug)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONHandler(page)(w, req)
	})
	return r
}

// writeJSONHandler emits a JSON document the same way the rest of
// the console does (X-Nexus-Source header, application/json content
// type). Re-implementing the helper rather than calling the
// console package keeps internal/docs free of a back-import on the
// admin layer, which would otherwise re-introduce the cycle the
// build tag used elsewhere to break.
func writeJSONHandler(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexus-Source", "nexus-console")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeJSONError writes a JSON {\"error\": msg} envelope with the
// given status. Distinct from writeJSONHandler so a 4xx response
// stops encoding an empty object after the error line; the shape
// is what the front-end's fetch wrapper expects.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Nexus-Source", "nexus-console")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
