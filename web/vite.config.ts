import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// During development the dashboard talks to the console server.
// Set VITE_API_PROXY env var to override the default target (e.g. for demos).
const API_PROXY_TARGET = process.env.VITE_API_PROXY || "http://localhost:8081";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: API_PROXY_TARGET,
        changeOrigin: true,
        ws: true,
      },
    },
  },
});
