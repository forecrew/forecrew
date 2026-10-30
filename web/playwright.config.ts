import { defineConfig, devices } from "@playwright/test"

// The tests run against the production build (make web), served by vite
// preview with the same Content-Security-Policy as forecrewd. The API is
// answered from the Go golden fixtures with page.route().
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: { baseURL: "http://localhost:4173", trace: "retain-on-failure" },
  projects: [
    {
      name: "chromium",
      // PLAYWRIGHT_CHROMIUM_PATH uses a Chromium you already have instead of the one `playwright install` fetches.
      use: { ...devices["Desktop Chrome"], launchOptions: { executablePath: process.env.PLAYWRIGHT_CHROMIUM_PATH || undefined } },
    },
  ],
  webServer: {
    command: "pnpm exec vite preview --port 4173 --strictPort",
    url: "http://localhost:4173",
    reuseExistingServer: !process.env.CI,
  },
})
