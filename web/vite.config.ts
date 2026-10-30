import { fileURLToPath } from "node:url"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vitest/config"

// Same policy as internal/webui, so the preview used by the e2e tests
// fails on anything the production server would block.
const csp = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    // The Go binary embeds this directory (internal/webui).
    outDir: "../internal/webui/dist",
    emptyOutDir: false,
    // No data: URIs: the CSP only allows files from the same origin.
    assetsInlineLimit: 0,
  },
  server: {
    // make web-dev: a local forecrewd on localhost, where the Secure cookie works.
    proxy: { "/api": { target: process.env.FORECREW_URL ?? "http://localhost:8080", changeOrigin: false } },
  },
  preview: { headers: { "Content-Security-Policy": csp } },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    coverage: {
      provider: "v8",
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/components/ui/**", "src/test/**", "src/**/*.test.{ts,tsx}", "src/main.tsx", "src/**/*.d.ts"],
      reporter: ["text-summary", "text"],
      thresholds: { lines: 80 },
    },
  },
})
