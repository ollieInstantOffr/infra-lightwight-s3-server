import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],

  build: {
    // Built straight into the Go module's embed directory, so `go build`
    // picks up whatever the last frontend build produced. There is no copy
    // step to forget.
    outDir: "../internal/web/dist",
    emptyOutDir: true,
    // Source maps would roughly double the embedded payload for a console
    // nobody debugs in production.
    sourcemap: false,
  },

  server: {
    port: 5173,
    // In development the SPA runs on Vite and the API on the Go process, so
    // API calls are proxied. Same-origin in both cases, which keeps the
    // session cookie working without CORS.
    proxy: {
      "/api": { target: "http://localhost:9001", changeOrigin: true },
      "/healthz": { target: "http://localhost:9001", changeOrigin: true },
      "/readyz": { target: "http://localhost:9001", changeOrigin: true },
    },
  },
});
