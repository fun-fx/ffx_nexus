# Docs tab in the console

The console's Docs page mirrors [thegrid.ai/docs](https://thegrid.ai/docs):
a left-hand sidebar of foldable categories, an on-this-page rail on
the right, and a body that renders the verbatim Markdown we
already keep in `/docs`. The same files that ship in the repo's
git tree are what the operator sees — no shadow copy, no
copy-paste between the repo and the on-line HTML.

## Source-of-truth contract

* The walking, indexing, and serving live in `internal/docs`.
* Front-matter is honoured: `<!-- category: foo -->` /
  `<!-- title: ... -->` / `<!-- order: N -->` / `<!-- status: v1beta -->`.
* Without front-matter a file's category is inferred from its
  path: top-level files → Concepts, files under `docs/operations*`/
  `docs/observability*` → Operations, `docs/adr/*` → Architecture
  decisions, `docs/release-notes/*` → Release notes.

## Agent surfaces

Two non-HTML endpoints expose the same data for tooling that
prefers raw Markdown:

* `GET /api/docs` returns the sidebar index JSON.
* `GET /api/docs/{slug}` returns a single page JSON with the
  markdown body.
* `GET /api/docs/llms.txt` returns the human-typed plain-text
  index, matching [thegrid.ai/docs/llms.txt](https://thegrid.ai/docs/llms.txt).

## Customising the docs root

The package reads from `./docs` by default. Operators that mount
a different tree (for example, a Helm ConfigMap that overlays the
cluster's own runbooks) point `NEXUS_DOCS_DIR` at it. The
console logs the resolved path at boot.

## Defining the Quick Links grid

`internal/docs/docs.go` carries a fixed six-tile list (Quickstart,
Onboarding, Enterprise model, Model benchmarks, Eval plugins,
Packaging). A file present in `/docs` gets matched by title — case
insensitive — and surfaces on the index page. Re-order by editing
the slice; the order there governs the grid on the index page.
