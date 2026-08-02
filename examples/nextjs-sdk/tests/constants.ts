// Shared between the Playwright config (which injects them into the servers) and the
// browser test (which scans for the sentinel). The API key is a distinctive sentinel so
// the browser-surface secret scan is meaningful: this exact string is the server-only
// credential, and it must appear in NO browser surface (request headers, source maps,
// static chunks). The Route Handler is its only holder.
export const API_KEY = "palai-sk-live-proof-DO-NOT-LEAK-7f3c9a1e2b8d4056";
// THE SUITE OWNS ITS OWN PORTS, AND 3100 IS NOT ONE OF THEM.
//
// It used to be. `pnpm dev` and `pnpm start` serve the demo on 3100 for an operator to look at, the
// suite ran on 3100 too, and playwright.config.ts sets `reuseExistingServer: !process.env.CI`. So on
// any machine where somebody had the demo open — which is every machine where somebody is working on
// this demo — the suite silently ADOPTED that server instead of starting its own.
//
// MEASURED, 2026-08-02: with the demo running on 3100 against the LIVE control plane, all twelve
// specs in ios-chat.spec.ts failed with the run stuck at "run queued", because they were driving a
// real stack while asserting against the deterministic fake's fixtures. In CI, where no server is
// running, the same commit is green. That is the exact shape CLAUDE.md names — a test whose result
// belongs to the harness rather than to the product — and it points the wrong way here: it hides
// failures locally and would have hidden them from every agent, since we all work outside CI.
//
// A test must OWN the condition it depends on. These ports are the suite's alone.
export const NEXT_PORT = 3110;
export const UPSTREAM_PORT = 3111;
