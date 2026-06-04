import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// During development the dashboard talks to the console server on :8081.
// API and WebSocket calls are proxied so the browser only sees one origin.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://localhost:8081",
        changeOrigin: true,
        ws: true,
      },
    },
  },
});
