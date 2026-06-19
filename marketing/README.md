# Nexus marketing site (nexus.ffx.ai)

Public marketing site for Nexus. Static output, deployed to **Cloudflare Pages**.

## Stack

- **[Astro 4](https://astro.build/)** — static site, zero JS by default
- **[TailwindCSS 3](https://tailwindcss.com/)** — utility-first styling
- **[@astrojs/sitemap](https://docs.astro.build/en/guides/integrations-guide/sitemap/)** — auto-generated sitemap
- TypeScript strict

## Pages

| Path | Purpose |
|---|---|
| `/` | Hero, features, metrics, how-it-works, FAQ, CTA |
| `/enterprise` | SSO, multi-tenant, audit, self-hosted |
| `/pricing` | OSS (free) / Cloud / Enterprise tiers |
| `/docs` | Quickstart, concepts, deployment, API reference |
| `/404` | Off-the-map |

## Local development

```bash
cd marketing
npm install
npm run dev      # http://localhost:4321
```

## Production build

```bash
npm run build    # static output in dist/
npm run preview  # serve dist/ locally
```

The `dist/` directory is fully static and CDN-friendly.

## Deploy (Cloudflare Pages)

This site is configured for **Cloudflare Pages** with build output `dist/`.

### One-time setup

1. Go to Cloudflare Dashboard → Pages → Create a project → Connect to GitHub.
2. Select the `fun-fx/ffx_nexus` repo, branch `main`, root directory `marketing`.
3. Build command: `npm run build`
4. Build output: `dist`
5. Add custom domain `nexus.ffx.ai` (CNAME to `<project>.pages.dev`).

### DNS

In Cloudflare DNS for `ffx.ai`:

| Type | Name | Target |
|---|---|---|
| CNAME | `nexus` | `<project>.pages.dev` (proxied) |

### CI

`.github/workflows/marketing-pages.yml` will auto-deploy on every push to `main`
(after the workflow is added in step `mkt-cf-pages`).

## Project layout

```
marketing/
├── astro.config.mjs        # Astro + Tailwind + sitemap
├── tailwind.config.mjs     # brand palette, animations
├── tsconfig.json
├── package.json
├── public/
│   ├── favicon.svg
│   ├── og.svg              # OG image (SVG; CF Pages will serve as-is)
│   └── robots.txt
└── src/
    ├── layouts/
    │   └── BaseLayout.astro
    ├── pages/
    │   ├── index.astro
    │   ├── enterprise.astro
    │   ├── pricing.astro
    │   ├── 404.astro
    │   └── docs/
    │       └── index.astro
    ├── components/
    │   ├── Header.astro
    │   ├── Footer.astro
    │   ├── Hero.astro
    │   ├── Features.astro
    │   ├── Metrics.astro
    │   ├── HowItWorks.astro
    │   ├── FAQ.astro
    │   ├── FaqItem.astro
    │   ├── CodeBlock.astro
    │   └── CTA.astro
    └── styles/
        └── global.css
```

## Design notes

- **Dark mode by default** — the brand is built around the cool-slate + violet accent.
- **No client JS by default** — all components are pure HTML/CSS. No analytics yet (deliberate; v1).
- **Inter** loaded from `rsms.me` with `preconnect` for fast first paint.
- **Bifrost-style** hero — gradient text, code preview, metrics, FAQ, CTA.
- **Accessible** — semantic HTML, `aria-*` on icons, focus rings, prefers-reduced-motion respected (via Tailwind defaults).
