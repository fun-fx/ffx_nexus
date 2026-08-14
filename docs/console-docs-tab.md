# Docs in the console

The console's Docs page mirrors [thegrid.ai/docs](https://thegrid.ai/docs):
a left-hand sidebar of foldable categories, an on-this-page rail on
the right, and a body that renders the verbatim Markdown we keep in
`/docs`. The same files that ship in the repo's git tree are what
the operator sees — no shadow copy, no copy-paste between the repo
and the on-line HTML.

## Two surfaces on one tree

The same Markdown tree drives both surfaces:

- The HTML page (this one) — for a human reader.
- The agent surfaces (`GET /api/docs`, `GET /api/docs/{slug}`,
  `GET /api/docs/llms.txt`) — for tooling that prefers raw Markdown
  or a flat index.

A file present in `/docs` is reachable on both without
configuration: front-matter overrides the inferred category, but
the file otherwise lands by path.

## What lives here

The Docs tree is the surface where Nexus describes itself:
what it does, why it does it that way, and how its three
operator-facing tabs behave. The vendor-specific recipe material —
Helm-rendered manifests, secret wiring, `values.yaml` overrides,
debug runbooks — is removed so this page reads like marketing,
not a manual.
