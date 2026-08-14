import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { Link, NavLink, useParams } from "react-router-dom";
import { fetchDocsIndex, fetchDocPage, type DocEntry, type DocsIndex, type DocPage } from "../api";
import { Icon } from "../components/icons";

// === Docs page ============================================================
//
// Mirrors the layout thegrid.ai/docs uses: a left-hand sidebar of
// foldable category groups, an "on this page" right rail that pulls
// from the rendered headings, and a main column whose hero / Quick
// Links grid is the same shape as the upstream product.
//
// The markdown itself is rendered by a small block-walker (see
// renderBody and friends). A full markdown parser is not pulled in
// because the docs tree is /docs on disk, the file size is bounded
// by an editor's discretion, and a regex pipeline beats shipping
// 200 kB of remark/rehype just to produce headings/lists/code.

async function loadIndex(): Promise<DocsIndex> {
  // Errors are caught by react-query and surfaced through `isError`,
  // not thrown out of the closure. Surfacing them lets the page
  // render a real "you need to sign in" panel rather than a generic
  // loading spinner of indeterminate length when the visitor's
  // session is unauthenticated (401) or the docs tree is unreachable
  // (5xx). Without this branch the page renders an empty hero that
  // looks identical to "everything is fine but blank" — the exact
  // layout we observed in the live cluster.
  return await fetchDocsIndex();
}

async function loadPage(path: string): Promise<DocPage | null> {
  return await fetchDocPage(path);
}

export function Docs() {
  // The route defines `/docs/:slug(.*)` (a splat). React Router v6
// returns the matched remainder under the special key `*`, not a
// named param, regardless of what the path string looks like. The
// splat carries the entire sub-path verbatim — including slashes —
// so a single string is sufficient to hit /api/docs/{slug} on the
// backend.
  const params = useParams();
  const slugPath = (params["*"] as string | undefined) ?? "";
  const isIndex = !slugPath;

  const {
    data: index,
    isLoading: indexLoading,
    isError: indexError,
    error: indexErrorValue,
  } = useQuery({
    queryKey: ["docs-index"],
    queryFn: loadIndex,
  });
  const {
    data: page,
    isLoading: pageLoading,
    isError: pageError,
  } = useQuery({
    queryKey: ["docs-page", slugPath],
    queryFn: () => loadPage(slugPath),
    enabled: !isIndex,
  });

  // The index failed (usually 401 when the visitor is unauthenticated).
  // Render a single sign-in panel rather than letting DocsIndexPage
  // sit on an empty `index` and produce a blank hero. The sidebar
  // also reads from `index`, so we render it in an empty placeholder
  // shape that still preserves the visual rhythm of the page (the
  // empty sidebar tells the user the layout is loading-the-content,
  // not the app is broken).
  if (indexError) {
    return <DocsSignInGate error={indexErrorValue} />;
  }

  return (
    <div className="docs-shell">
      <DocsSidebar index={index} active={slugPath || "quickstart"} />
      <div className="docs-main">
        {isIndex ? (
          <DocsIndexPage index={index} loading={indexLoading} />
        ) : (
          <DocsArticle page={page} loading={pageLoading} slugPath={slugPath} pageError={pageError} />
        )}
      </div>
      {!isIndex && page ? (
        <DocsTOC headings={headingsFromBody(page.body)} />
      ) : null}
    </div>
  );
}

// === sign-in gate ========================================================
//
// Surfaces 401 from /api/docs (and from any other docs endpoint) so the
// unauthenticated visitor sees a clear pane instead of an empty hero
// that reads as a black-screen render. Keeps the docs sidebar layout
// visible so the page composition is still recognizable as the docs
// UI rather than a generic error page.
function DocsSignInGate({ error }: { error: unknown }) {
  const status =
    (error as { status?: number } | null)?.status ??
    (typeof error === "object" && error && "message" in error ? 0 : 0);
  const needsLogin = status === 401 || status === 403;
  return (
    <div className="docs-shell">
      <DocsSidebar index={undefined} active="" />
      <div className="docs-main">
        <div className="docs-page">
          <div className="docs-hero">
            <div className="docs-hero-title">
              {needsLogin ? "Sign in to read the docs" : "Docs are temporarily unavailable"}
            </div>
            <div className="docs-hero-sub">
              {needsLogin
                ? "The Nexus documentation is bundled into every cluster but requires an authenticated session, same as the rest of the console."
                : "The docs endpoint returned a 5xx error. Check the console pod logs (`kubectl logs -l app.kubernetes.io/name=nexus`) for the underlying cause."}
            </div>
            <div className="docs-hero-cta">
              {needsLogin ? (
                <a className="docs-cta-primary" href="/login?next=%2Fdocs">
                  Sign in →
                </a>
              ) : (
                <button
                  className="docs-cta-primary"
                  type="button"
                  onClick={() => window.location.reload()}
                >
                  Reload page
                </button>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

// === sidebar =============================================================
function DocsSidebar({ index, active }: { index: DocsIndex | undefined; active: string }) {
  // Sidebar accordion pattern mirrors thegrid.ai: each category
  // header toggles, but the active page's category is always open
  // so the user does not lose their place after navigating.
  const [open, setOpen] = useState<Record<string, boolean>>({});
  useEffect(() => {
    if (!index) return;
    // Open whichever category the active page sits in. This collapses
    // when the user manually toggles a different category so a quick
    // re-expand still feels familiar.
    setOpen((prev) => {
      const next = { ...prev };
      for (const c of index.categories) {
        if (c.entries.some((e) => e.path === active || active.startsWith(e.path + "/"))) {
          next[c.slug] = true;
        }
      }
      return next;
    });
  }, [active, index]);

  if (!index) {
    return (
      <aside className="docs-sidebar" aria-label="Documentation navigation">
        <div className="docs-sidebar-empty">Loading…</div>
      </aside>
    );
  }

  return (
    <aside className="docs-sidebar" aria-label="Documentation navigation">
      <div className="docs-sidebar-brand">
        <span className="docs-sidebar-logo" aria-hidden>◆</span>
        <span>
          <div className="docs-sidebar-title">Nexus Docs</div>
          <div className="docs-sidebar-sub">Operator reference</div>
        </span>
      </div>
      {index.categories.map((cat) => {
        const isOpen = open[cat.slug] ?? false;
const ChevronIcon = isOpen ? Icon["chevron-down"] : Icon["chevron-right"];
            return (
              <div className="docs-sidebar-cat" key={cat.slug}>
                <button
                  type="button"
                  className="docs-sidebar-cat-toggle"
                  aria-expanded={isOpen}
                  onClick={() => setOpen((p) => ({ ...p, [cat.slug]: !p[cat.slug] }))}
                >
                  <ChevronIcon size={14} />
                  <span>{cat.title}</span>
                </button>
                {isOpen && cat.entries.length > 0 ? (
                  <ul className="docs-sidebar-list">
                    {cat.entries.map((e) => (
                      <li key={e.path}>
                        <NavLink
                          to={`/docs/${e.path}`}
                          end={false}
                          className={({ isActive: navActive }) =>
                            "docs-sidebar-link" + (navActive || e.path === active ? " is-active" : "")
                          }
                        >
                          <span>{e.title}</span>
                          {e.status && e.status !== "stable" ? (
                            <span className={"badge badge-" + e.status}>{e.status}</span>
                          ) : null}
                        </NavLink>
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
            );
      })}
    </aside>
  );
}

// === index page ===========================================================
function DocsIndexPage({
  index,
  loading,
}: {
  index: DocsIndex | undefined;
  loading: boolean;
}) {
  if (loading || !index) {
    return (
      <div className="docs-page">
        <div className="docs-hero">
          <div className="docs-hero-title">Loading documentation…</div>
        </div>
      </div>
    );
  }

  return (
    <div className="docs-page">
      <section className="docs-hero">
        <div className="docs-hero-eyebrow">{index.title}</div>
        <h1 className="docs-hero-title">{index.tagline || index.title}</h1>
        {index.tagline ? <p className="docs-hero-tag">{index.tagline}</p> : null}
        {index.quick_links.length > 0 ? (
          <div className="docs-quicklinks">
            {index.quick_links.map((e) => (
              <Link className="docs-quicklink" to={`/docs/${e.path}`} key={e.path}>
                <div className="docs-quicklink-title">{e.title}</div>
                {e.summary ? <div className="docs-quicklink-summary">{e.summary}</div> : null}
                <div className="docs-quicklink-more">
                  Read <span aria-hidden>→</span>
                </div>
              </Link>
            ))}
          </div>
        ) : null}
      </section>

      {index.categories.map((c) =>
        c.entries.length === 0 ? null : (
          <section className="docs-section" key={c.slug}>
            <h2 className="docs-section-title">{c.title}</h2>
            <div className="docs-grid">
              {c.entries.map((e) => (
                <Link className="docs-grid-card" key={e.path} to={`/docs/${e.path}`}>
                  <div className="docs-grid-card-title">
                    {e.title}
                    {e.status && e.status !== "stable" ? (
                      <span className={"badge badge-" + e.status}>{e.status}</span>
                    ) : null}
                  </div>
                  {e.summary ? <p className="docs-grid-card-summary">{e.summary}</p> : null}
                </Link>
              ))}
            </div>
          </section>
        ),
      )}
    </div>
  );
}

// === article (markdown page) ==============================================
// Each block is a normal {kind, ...} record; renderBlock turns it
// into JSX. Adding a new block shape later is one switch case plus
// a token in the parser — the frontend does not need a fresh
// dependency for a new heading style.
type Block =
  | { kind: "h1"; text: string; id: string }
  | { kind: "h2"; text: string; id: string }
  | { kind: "h3"; text: string; id: string }
  | { kind: "h4"; text: string; id: string }
  | { kind: "p"; inline: InlineNode[] }
  | { kind: "code"; lang: string; body: string }
  | { kind: "ul"; items: InlineNode[][] }
  | { kind: "ol"; items: InlineNode[][] }
  | { kind: "blockquote"; inline: InlineNode[] }
  | { kind: "table"; head: InlineNode[]; rows: InlineNode[][] }
  | { kind: "hr" };

type InlineNode =
  | { kind: "text"; text: string }
  | { kind: "code"; text: string }
  | { kind: "strong"; text: string }
  | { kind: "em"; text: string }
  | { kind: "link"; text: string; href: string };

function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^\p{Letter}\p{Number}\s-]/gu, "")
    .trim()
    .replace(/\s+/g, "-");
}

function parseBlocks(md: string): Block[] {
  const out: Block[] = [];
  const lines = md.replace(/\r\n/g, "\n").split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const trimmed = line.trim();

    // Code fence — passes through verbatim, language is optional.
    if (trimmed.startsWith("```")) {
      const lang = trimmed.slice(3).trim();
      const buf: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith("```")) {
        buf.push(lines[i]);
        i++;
      }
      i++;
      out.push({ kind: "code", lang, body: buf.join("\n") });
      continue;
    }

    // Heading. The slug is the text lowercased and hyphenated;
    // the on-this-page rail reads it back to anchor links.
    const h = /^(#{1,4})\s+(.*)$/.exec(trimmed);
    if (h) {
      const level = h[1].length as 1 | 2 | 3 | 4;
      const text = h[2].trim();
      const id = slugify(text);
      out.push({ kind: `h${level}` as Block["kind"], text, id } as Block);
      i++;
      continue;
    }

    if (/^---+$/.test(trimmed)) {
      out.push({ kind: "hr" });
      i++;
      continue;
    }

    // Tables: recognise \"header\\n|---|---|\\n|...\" shape, then
    // emit rows whose cells still parse inline spans so links and
    // **bold** survive inside a table cell.
    if (trimmed.startsWith("|") && /^\|\s*[-:|\s]+\|$/.test(lines[i + 1] ?? "")) {
      const head: InlineNode[] = [];
      for (const c of splitCells(lines[i])) {
        for (const n of parseInline(c)) head.push(n);
      }
      i += 2;
      const rows: InlineNode[][] = [];
      while (i < lines.length && lines[i].trim().startsWith("|")) {
        const row: InlineNode[] = [];
        for (const c of splitCells(lines[i])) {
          for (const n of parseInline(c)) row.push(n);
        }
        rows.push(row);
        i++;
      }
      out.push({ kind: "table", head, rows });
      continue;
    }

    // Blockquote: collect every contiguous `>` line and emit a
    // single blockquote with the joined inline.
    if (trimmed.startsWith(">")) {
      const buf: string[] = [];
      while (i < lines.length && lines[i].trim().startsWith(">")) {
        buf.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      out.push({ kind: "blockquote", inline: parseInline(buf.join("\n")) });
      continue;
    }

    // Lists.
    if (/^[-*]\s+/.test(trimmed)) {
      const items: InlineNode[][] = [];
      while (i < lines.length && /^[-*]\s+/.test(lines[i].trim())) {
        items.push(parseInline(lines[i].trim().replace(/^[-*]\s+/, "")));
        i++;
      }
      out.push({ kind: "ul", items });
      continue;
    }
    if (/^\d+\.\s+/.test(trimmed)) {
      const items: InlineNode[][] = [];
      while (i < lines.length && /^\d+\.\s+/.test(lines[i].trim())) {
        items.push(parseInline(lines[i].trim().replace(/^\d+\.\s+/, "")));
        i++;
      }
      out.push({ kind: "ol", items });
      continue;
    }

    // Blank line — just advance; paragraph collation happens
    // implicitly because the \"run of text\" branch below swallows
    // soft blanks into a single <p>.
    if (trimmed === "") {
      i++;
      continue;
    }

    // Default: paragraph made of one or more consecutive non-blank
    // lines joined with spaces. Inline spans parse across the join.
    const buf: string[] = [lines[i]];
    i++;
    while (i < lines.length && lines[i].trim() !== "" && !/^(#{1,4}\s|>|```|[-*]\s|\d+\.\s|\|)/.test(lines[i].trim())) {
      buf.push(lines[i]);
      i++;
    }
    out.push({ kind: "p", inline: parseInline(buf.join(" ")) });
  }
  return out;
}

function splitCells(line: string): string[] {
  return line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
}

const INLINE_TOKEN = /(\*\*[^*]+\*\*)|(`[^`]+`)|(\[[^\]]+\]\([^)]+\))|(\*[^*]+\*)/g;

function parseInline(s: string): InlineNode[] {
  const out: InlineNode[] = [];
  let i = 0;
  while (i < s.length) {
    const rest = s.slice(i);
    const m = /^(?:\*\*([^*]+)\*\*)|^(?:`([^`]+)`)|^(?:\[([^\]]+)\]\(([^)]+)\))|^(?:\*([^*]+)\*)/.exec(rest);
    if (!m) {
      // Take the next chunk up to the next special character; empties
      // get skipped after joining so a stray backtick doesn't split
      // the visible text into two half-runs.
      const next = INLINE_TOKEN.exec(rest);
      if (next && next.index !== undefined) {
        if (next.index > 0) out.push({ kind: "text", text: rest.slice(0, next.index) });
        i += next.index;
      } else {
        out.push({ kind: "text", text: rest });
        i = s.length;
      }
      continue;
    }
    if (m[1] !== undefined) out.push({ kind: "strong", text: m[1] });
    else if (m[2] !== undefined) out.push({ kind: "code", text: m[2] });
    else if (m[3] !== undefined) out.push({ kind: "link", text: m[3], href: m[4] });
    else if (m[5] !== undefined) out.push({ kind: "em", text: m[5] });
    i += m[0].length;
  }
  return mergeText(out);
}

// mergeText folds adjacent text nodes so `[a, b]` does not split
// the line on screen, which would also drop the user's spacing
// choices (e.g. extra spaces around * italics *).
function mergeText(nodes: InlineNode[]): InlineNode[] {
  const out: InlineNode[] = [];
  for (const n of nodes) {
    const prev = out[out.length - 1];
    if (prev && prev.kind === "text" && n.kind === "text") {
      prev.text += n.text;
    } else {
      out.push({ ...n });
    }
  }
  return out;
}

function renderInline(nodes: InlineNode | InlineNode[]): JSX.Element[] {
  const arr: InlineNode[] = Array.isArray(nodes) ? nodes : [nodes];
  return arr.map((n, idx) => {
    switch (n.kind) {
      case "text":
        return <span key={idx}>{n.text}</span>;
      case "code":
        return <code key={idx}>{n.text}</code>;
      case "strong":
        return <strong key={idx}>{n.text}</strong>;
      case "em":
        return <em key={idx}>{n.text}</em>;
      case "link":
        return (
          <a key={idx} href={n.href} target={n.href.startsWith("http") ? "_blank" : undefined} rel={n.href.startsWith("http") ? "noreferrer" : undefined}>
            {n.text}
          </a>
        );
    }
  });
}

function renderBlock(b: Block, idx: number): JSX.Element | null {
  switch (b.kind) {
    case "h1":
      return (
        <h1 key={idx} id={b.id}>
          {b.text}
        </h1>
      );
    case "h2":
      return (
        <h2 key={idx} id={b.id}>
          {b.text}
        </h2>
      );
    case "h3":
      return (
        <h3 key={idx} id={b.id}>
          {b.text}
        </h3>
      );
    case "h4":
      return (
        <h4 key={idx} id={b.id}>
          {b.text}
        </h4>
      );
    case "p":
      return <p key={idx}>{renderInline(b.inline)}</p>;
    case "code":
      return (
        <pre key={idx} className={b.lang ? "code lang-" + b.lang : "code"}>
          <code>{b.body}</code>
        </pre>
      );
    case "ul":
      return (
        <ul key={idx}>
          {b.items.map((it, i) => (
            <li key={i}>{renderInline(it)}</li>
          ))}
        </ul>
      );
    case "ol":
      return (
        <ol key={idx}>
          {b.items.map((it, i) => (
            <li key={i}>{renderInline(it)}</li>
          ))}
        </ol>
      );
    case "blockquote":
      return (
        <blockquote key={idx}>
          <p>{renderInline(b.inline)}</p>
        </blockquote>
      );
    case "table":
      return (
        <table key={idx}>
          <thead>
            <tr>
              {b.head.map((c, i) => (
                <th key={i}>{renderInline(c)}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {b.rows.map((row, i) => (
              <tr key={i}>
                {row.map((c, j) => (
                  <td key={j}>{renderInline(c)}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      );
    case "hr":
      return <hr key={idx} />;
  }
}

function headingsFromBody(md: string): { id: string; text: string; level: 2 | 3 }[] {
  const out: { id: string; text: string; level: 2 | 3 }[] = [];
  for (const b of parseBlocks(md)) {
    if (b.kind === "h2") out.push({ id: b.id, text: b.text, level: 2 });
    if (b.kind === "h3") out.push({ id: b.id, text: b.text, level: 3 });
  }
  return out;
}

function DocsArticle({
  page,
  loading,
  slugPath,
  pageError,
}: {
  page: DocPage | undefined | null;
  loading: boolean;
  slugPath: string;
  pageError: boolean;
}) {
  const blocks = useMemo(() => (page ? parseBlocks(page.body) : []), [page]);
  if (pageError) {
    // /api/docs/{slug} 401 — the visitor is unauthenticated. Redirect
    // to the sign-in gate so the page does not render the empty
    // skeleton + black background the cluster was surfacing before.
    return <DocsSignInGate error={{ status: 401 }} />;
  }
  if (loading) {
    return (
      <div className="docs-page">
        <div className="docs-hero">
          <div className="docs-hero-title">Loading {slugPath}…</div>
        </div>
      </div>
    );
  }
  if (!page) {
    return (
      <div className="docs-page">
        <section className="docs-hero">
          <div className="docs-hero-eyebrow">404</div>
          <h1 className="docs-hero-title">No such page</h1>
          <p className="docs-hero-tag">
            We could not find a doc at <code>{slugPath}</code>. The sidebar might have a stale link; check{" "}
            <Link to="/docs">the index</Link>.
          </p>
        </section>
      </div>
    );
  }
  return (
    <article className="docs-page docs-article">
      <div className="docs-article-head">
        <div className="docs-article-crumb">
          <Link to="/docs">Docs</Link>
          <span aria-hidden> / </span>
          <span>{page.category}</span>
        </div>
        <div className="docs-article-meta">
          {page.status && page.status !== "stable" ? (
            <span className={"badge badge-" + page.status}>{page.status}</span>
          ) : null}
          <code className="docs-article-source">{page.source_path ?? page.path}</code>
        </div>
      </div>
      {blocks.map((b, i) => renderBlock(b, i))}
    </article>
  );
}

function DocsTOC({ headings }: { headings: { id: string; text: string; level: 2 | 3 }[] }) {
  if (headings.length === 0) return null;
  return (
    <aside className="docs-toc" aria-label="On this page">
      <div className="docs-toc-title">On this page</div>
      <ul>
        {headings.map((h) => (
          <li key={h.id} className={h.level === 3 ? "docs-toc-sub" : ""}>
            <a href={"#" + h.id}>{h.text}</a>
          </li>
        ))}
      </ul>
    </aside>
  );
}

export type { DocEntry };
