import { defineConfig, devices } from "@playwright/test";

// End-to-end tests run against the Compose stack (make e2e), inside the
// official Playwright container on the stack's network.
export default defineConfig({
  testDir: "./e2e",
  timeout: 90_000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:3000",
    trace: "retain-on-failure",
  },
  // The reference device is a cheap phone (docs/DESIGN.md §1).
  projects: [{ name: "mobile", use: { ...devices["Pixel 5"] } }],
});
