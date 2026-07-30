import { defineConfig, devices } from "@playwright/test";

import { API_KEY, consolePasswordHash, IS_REAL, NEXT_PORT, PROFILE, UNCONFIGURED_PORT, UPSTREAM, UPSTREAM_PORT } from "./tests/constants";

// The hash is derived ONCE here, by running the shipped scripts/hash-password.mjs, and handed to the
// configured console below. The un-configured console on UNCONFIGURED_PORT gets everything except this.
const PASSWORD_HASH = consolePasswordHash();

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
  // THE PER-TEST BUDGET, AND IT WAS A SILENT CAP (E25 T6). No `timeout` was set, so Playwright's default 30s
  // applied — while tests/profile.ts runToTerminal waits up to 60s for a real run's terminal, deliberately and
  // with that number written down. The shorter timeout won, so the 60s budget was UNREACHABLE and a compose
  // run slower than 30s failed as "Test timeout of 30000ms exceeded" inside an expect that had 30 seconds of
  // its own budget left. MEASURED on this box rather than reasoned about: a real console-driven run took 13s
  // and 20s with nothing else competing, and 81s (read off the ndjson frame timestamps: run.running.v1 at
  // 12:01:36, model_step.created.v1 at 12:02:57) while a full suite and a four-service stack shared the
  // machine. 90s lets the author's 60s actually happen and still reports a genuine hang. It changes nothing on
  // the fake profile, where every test finishes in under three seconds.
  timeout: 90_000,
  reporter: [["line"]],
  use: {
    baseURL: `http://127.0.0.1:${NEXT_PORT}`,
    trace: "off",
  },
  // TWO COLOUR SCHEMES, ONE SPEC SET — and the second one is a HOLE BEING CLOSED, not a nicety.
  //
  // There was one project and it named no `colorScheme`. The installed playwright-core@1.51.1 writes its own
  // default down in its types ("Passing `null` resets emulation to system defaults. Defaults to 'light'."),
  // so every axe scan this suite has ever run looked at the LIGHT palette — and `app/globals.css` carries a
  // whole `@media (prefers-color-scheme: dark)` block that redefines the text, border and accent colours.
  // The dark palette was outside automated accessibility coverage entirely, which is how a 2.63:1 skip link
  // (white text on the lightened dark-mode accent, first Tab stop on every page) stayed green.
  //
  // It runs the WHOLE spec set rather than a named list of the four files that call AxeBuilder today
  // (a11y, auth, config-journey, secret-never-returns). A hand-maintained list is the same shape as the
  // hand-written route list lib/routes.ts exists to abolish: the next spec to add a scan would be
  // light-only and nothing would say so. The cost is honest — the non-visual specs run twice and prove
  // nothing new the second time — and it is paid in seconds on the fake profile.
  projects: [
    { name: `chromium-${PROFILE}`, use: { ...devices["Desktop Chrome"], colorScheme: "light" } },
    { name: `chromium-${PROFILE}-dark`, use: { ...devices["Desktop Chrome"], colorScheme: "dark" } },
  ],
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
        PALAI_CONSOLE_PASSWORD_HASH: PASSWORD_HASH,
      },
      url: `http://127.0.0.1:${NEXT_PORT}`,
      timeout: 120_000,
      // On the real profile a stale console process would still be pointed at the FAKE upstream, and the
      // whole run would silently prove nothing. Never reuse a server whose upstream this run just changed.
      reuseExistingServer: !process.env.CI && !IS_REAL,
      stdout: "pipe",
      stderr: "pipe",
    },
    {
      // THE FAIL-CLOSED CONTROL (E25 T1). Same build, same key, same upstream — and NO
      // PALAI_CONSOLE_PASSWORD_HASH. tests/auth.spec.ts asserts this console serves no read, no write and no
      // sign-in, which is only a claim about the missing hash because nothing else about it differs.
      //
      // Its readiness is probed on /login rather than /, because an un-configured console is EXPECTED to
      // refuse data: the static shell still renders (the gate is in the relay, deliberately not in the
      // layout — §3.5 N5), and that is the honest shape of "does not serve" for a Next app.
      command: `pnpm exec next start -p ${UNCONFIGURED_PORT}`,
      env: {
        PALAI_API_KEY: API_KEY,
        PALAI_BASE_URL: UPSTREAM,
      },
      url: `http://127.0.0.1:${UNCONFIGURED_PORT}/login`,
      timeout: 120_000,
      reuseExistingServer: !process.env.CI && !IS_REAL,
      stdout: "pipe",
      stderr: "pipe",
    },
  ],
});
