import { defineConfig, devices } from "@playwright/test";

import { API_KEY, IS_REAL, NEXT_PORT, PROFILE, UPSTREAM, UPSTREAM_PORT } from "./tests/constants";

// TWO PROFILES, ONE SPEC SET (E19 T7). PALAI_CONSOLE_PROFILE selects the /v1 upstream the built console is
// pointed at; the spec files, the browser and the relay are identical in both. That constraint is the point:
// if a spec can only pass against one of them, that is a FINDING about the fixture or the API — recorded in
// tests/divergences.mjs and proven by tests/conformance.test.mjs — and never a reason to fork the specs.
//
//   fake (default) — the deterministic upstream (tests/fake-control-plane.mjs) on a loopback port, started
//                    here as a webServer. Fast, Docker-free, and the ONLY place a hostile artifact, a
//                    scripted recovery or a paused approval can be produced on demand. It is not going away.
//   real           — a compose control plane, already running. PALAI_BASE_URL and PALAI_API_KEY come from the
//                    ENVIRONMENT; tests/constants.ts throws if either is missing, so asking for the real
//                    profile without a real stack fails the whole run at config load rather than skipping
//                    into a green report.
//
// In BOTH profiles the browser talks only to the console's relay, never to the upstream and never with a
// key — the public-API-only and no-leak proofs assert exactly that, and on the real profile they assert it
// about a REAL bootstrap credential, which is the stronger form of the same claim.
export default defineConfig({
  testDir: "./tests",
  // The conformance sweep is not a browser test: it compares two HTTP servers and runs under `node --test`
  // (pnpm sweep). Naming it here keeps Playwright from trying to collect a .mjs it cannot drive.
  testIgnore: ["**/*.test.mjs"],
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: [["line"]],
  use: {
    baseURL: `http://127.0.0.1:${NEXT_PORT}`,
    trace: "off",
  },
  projects: [{ name: `chromium-${PROFILE}`, use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    // The fake upstream, only on the fake profile — the real profile's upstream is a stack this config does
    // not own and must not start, stop or assume.
    ...(IS_REAL
      ? []
      : [
          {
            command: "node tests/fake-control-plane.mjs",
            env: { FAKE_UPSTREAM_PORT: String(UPSTREAM_PORT) },
            url: `${UPSTREAM}/healthz`,
            timeout: 30_000,
            reuseExistingServer: !process.env.CI,
            stdout: "pipe" as const,
            stderr: "pipe" as const,
          },
        ]),
    {
      // `next build` runs in the test:e2e script before Playwright; this only serves it.
      command: `pnpm exec next start -p ${NEXT_PORT}`,
      env: {
        PALAI_API_KEY: API_KEY,
        PALAI_BASE_URL: UPSTREAM,
      },
      url: `http://127.0.0.1:${NEXT_PORT}`,
      timeout: 120_000,
      // On the real profile a stale console process would still be pointed at the FAKE upstream, and the
      // whole run would silently prove nothing. Never reuse a server whose upstream this run just changed.
      reuseExistingServer: !process.env.CI && !IS_REAL,
      stdout: "pipe",
      stderr: "pipe",
    },
  ],
});
