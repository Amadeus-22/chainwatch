/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// In development the app runs on http://localhost:5173 and proxies /v1 to the
// chainwatch API, so the session cookie stays first-party (same origin). Set
// CHAINWATCH_SIWE_DOMAIN=localhost:5173 on the API.
const api = process.env.CHAINWATCH_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: { "/v1": { target: api, changeOrigin: false } },
  },
  build: { sourcemap: false },
  test: { environment: "node", include: ["src/**/*.test.ts"] },
});
