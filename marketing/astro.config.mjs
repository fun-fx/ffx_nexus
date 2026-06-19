// @ts-check
import { defineConfig } from "astro/config";
import tailwind from "@astrojs/tailwind";

// Public marketing site. Static output → Cloudflare Pages.
// Domain target: https://nexus.ffx.ai
export default defineConfig({
  site: "https://nexus.ffx.ai",
  output: "static",
  trailingSlash: "never",
  build: {
    format: "directory",
    inlineStylesheets: "auto",
  },
  integrations: [
    tailwind({ applyBaseStyles: true }),
  ],
  prefetch: {
    prefetchAll: true,
    defaultStrategy: "viewport",
  },
  compressHTML: true,
});
