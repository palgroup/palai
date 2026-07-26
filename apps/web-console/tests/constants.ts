// Shared between the Playwright config (which injects them into the servers) and the tests (which scan
// for the credential + assert the relay origin).
//
// TWO PROFILES, ONE SPEC SET (E19 T7). `PALAI_CONSOLE_PROFILE` selects which /v1 upstream the built console
// is pointed at, and nothing else about the run changes — the same spec files execute against both:
//
//   fake (default) — tests/fake-control-plane.mjs on a loopback port, started by the Playwright webServer.
//                    Deterministic, fast, Docker-free. This layer is NOT going away: it is the only place a
//                    hostile artifact, a scripted recovery, or a paused approval can be produced on demand.
//   real           — a compose control plane. PALAI_BASE_URL and PALAI_API_KEY come from the ENVIRONMENT
//                    ($PALAI_HOME/config.json .base_url and $PALAI_HOME/api-key, read by the caller), never
//                    from a command line and never committed.
//
// ABSENCE IS A FAILURE, NEVER A SKIP. Asking for the real profile without a real stack throws here, which
// fails the whole run loudly at config load. A per-test skip in that situation is the exact trap this
// repo has now been caught by eight times: a suite that reports green for the condition it exists to detect.
export type ConsoleProfile = "fake" | "real";

export const PROFILE: ConsoleProfile = (process.env.PALAI_CONSOLE_PROFILE ?? "fake") === "real" ? "real" : "fake";
export const IS_REAL = PROFILE === "real";

export const NEXT_PORT = Number(process.env.PALAI_CONSOLE_PORT ?? 3200);
export const UPSTREAM_PORT = Number(process.env.FAKE_UPSTREAM_PORT ?? 3201);

// The fake profile's API key is a distinctive sentinel so the browser-surface secret scan is meaningful:
// this exact string is the server-only credential, and it must appear in NO browser surface (request
// headers, URLs, bodies, source maps, static chunks). The relay is its only holder. On the real profile the
// SAME scan runs against the REAL bootstrap key — a stronger assertion, since a leak would be a live
// credential. The scans compare with a boolean rather than a matcher so a failure never prints the key.
const FAKE_API_KEY = "palai-sk-console-proof-DO-NOT-LEAK-1a2b3c4d5e6f7a8b";

function realEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(
      `PALAI_CONSOLE_PROFILE=real requires ${name}, and this run has none. The real profile drives the built ` +
        "console against a RUNNING compose control plane; without it there is nothing to prove, and skipping " +
        "would report green for the absence itself. Bring the stack up with `PALAI_DISPATCH_WORKERS=1 " +
        "PALAI_MODEL_PROVIDER=fake palai local up`, then export PALAI_BASE_URL=$(… config.json .base_url) and " +
        "PALAI_API_KEY=$(cat $PALAI_HOME/api-key). Never pass the key as an argument.",
    );
  }
  return value;
}

export const UPSTREAM = IS_REAL ? realEnv("PALAI_BASE_URL").replace(/\/+$/, "") : `http://127.0.0.1:${UPSTREAM_PORT}`;
export const API_KEY = IS_REAL ? realEnv("PALAI_API_KEY") : FAKE_API_KEY;
