// vite.config.ts — builds the rup.ai companion UI from the frontend/ sources.
// The first-party frontend consists of exactly five files:
// index.html, main.ts, app.ts, styles.css, preview.ts
import { defineConfig } from "vite";

export default defineConfig({
  root: "frontend",
  // assets/ holds the rup.ai logo; it is copied to the bundle root
  publicDir: "../assets",
  server: {
    fs: {
      allow: [".."],
    },
    proxy: {
      // dev convenience: proxy API calls to a running `rup.ai preview`
      "/api": "http://127.0.0.1:8618",
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
    target: "es2020",
  },
});
