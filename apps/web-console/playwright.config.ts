import { defineConfig, devices } from "@playwright/test";

import { API_KEY, NEXT_PORT, UPSTREAM, UPSTREAM_PORT } from "./tests/constants";

// Two servers stand up the deterministic proof: the fake control-plane upstream (a real HTTP endpoint the
// SDK connects to over the network, replaying the scripted /v1/* contract — no live provider, no
// credential spend) and the built Next.js console. The console's server env carries the API key + the
// upstream base URL; the browser talks ONLY to the console's relay, never to the upstream and never with a
// key (the public-API-only + no-leak proofs assert exactly this).
export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: [["line"]],
  use: {
    baseURL: `http://127.0.0.1:${NEXT_PORT}`,
    trace: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    {
      command: "node tests/fake-control-plane.mjs",
      env: { FAKE_UPSTREAM_PORT: String(UPSTREAM_PORT) },
      url: `${UPSTREAM}/healthz`,
      timeout: 30_000,
      reuseExistingServer: !process.env.CI,
      stdout: "pipe",
      stderr: "pipe",
    },
    {
      // `next build` runs in the test:e2e script before Playwright; this only serves it.
      command: `pnpm exec next start -p ${NEXT_PORT}`,
      env: {
        PALAI_API_KEY: API_KEY,
        PALAI_BASE_URL: UPSTREAM,
      },
      url: `http://127.0.0.1:${NEXT_PORT}`,
      timeout: 120_000,
      reuseExistingServer: !process.env.CI,
      stdout: "pipe",
      stderr: "pipe",
    },
  ],
});
