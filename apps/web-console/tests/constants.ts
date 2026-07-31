import { execFileSync } from "node:child_process";

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

// A SECOND CONSOLE, IDENTICAL EXCEPT FOR THE ONE THING BEING PROVEN (E25 T1). The fail-closed claim is
// "a console with no PALAI_CONSOLE_PASSWORD_HASH does not serve", and the only honest way to assert it is
// against a real process that has none. This port carries the same build, the same API key and the same
// upstream — the hash is the single difference, so nothing else can explain the refusals.
export const UNCONFIGURED_PORT = Number(process.env.PALAI_CONSOLE_UNCONFIGURED_PORT ?? 3202);

// The operator password for both profiles. It is a test credential in the same sense FAKE_API_KEY is: it
// opens a loopback fixture that this repo starts and stops, and it sits three lines from its own hash — the
// point is that the door is real, not that this password is secret.
export const CONSOLE_PASSWORD = "console-proof-operator-password-DO-NOT-REUSE-4c1f9a2b";

// consolePasswordHash runs the SHIPPED script to produce the hash the server is given. Nothing is hardcoded
// and nothing is duplicated: scripts/hash-password.mjs is the only writer of the format, lib/session.ts is
// the only reader, and this call is what binds them — if the script's output ever stops being something the
// console accepts, every sign-in in the suite fails. It is a function rather than a constant so only the
// Playwright config pays for the derivation.
export function consolePasswordHash(): string {
  const line = execFileSync("node", ["scripts/hash-password.mjs"], { input: CONSOLE_PASSWORD, encoding: "utf8" }).trim();
  const prefix = "PALAI_CONSOLE_PASSWORD_HASH=";
  if (!line.startsWith(prefix)) throw new Error(`scripts/hash-password.mjs printed something unexpected: ${line.slice(0, 40)}…`);
  return line.slice(prefix.length);
}

// THE AXE TAG SET, IN ONE PLACE (E25 T2, plan §3.6 D16). Every axe scan in this suite uses these tags, and
// the list is here rather than inline because it was inline in four places and all four said WCAG 2.0.
//
// axe-core's own tag table defines `wcag2a`/`wcag2aa` as "WCAG 2.0 Level A"/"Level AA"; `wcag21a`,
// `wcag21aa` and `wcag22aa` are SEPARATE tags and were not included. So the console's accessibility evidence
// covered WCAG 2.0 only, and 2.4.11 Focus Not Obscured, 3.3.7 Redundant Entry and 3.3.8 Accessible
// Authentication — all new in 2.2, all criteria a FORM is judged by — sat entirely outside it, on a surface
// E25 is adding six forms to.
//
// What widening this proves is "axe-clean with the WCAG 2.0+2.1+2.2 tags", NOT "accessible": Deque's own
// figure is that axe finds "on average 57% of WCAG issues", and no scanner judges whether an error message
// MEANS anything. That gap is §6 operator leg 1 and it does not narrow here.
export const WCAG_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"];

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

// THE CREATE DIALOGS THE SWEEPS OPEN. Declared HERE rather than in a spec because TWO specs need the
// same five rows — tests/a11y.spec.ts scans each dialog with axe, tests/contrast.spec.ts measures the
// controls inside it — and Playwright refuses a spec-to-spec import outright ("test file X should not
// import test file Y"). A second hand-typed copy is what a declaration table exists to prevent.
//
// THE CREATE DIALOGS, AND THEY WERE SCANNED BY NOTHING AT ALL UNTIL THIS LOOP.
//
// The generated axe loop scans a route AS IT LOADS. A components/FormDialog.tsx renders only after a click,
// so every create form that moved behind a `+ Create` button left the accessibility evidence the moment it
// moved — five forms, on four routes, none of them scanned. It is the same hole this file already documents
// in tests/a11y.spec.ts for the transcript's second tab ("`hidden` takes the whole Debug panel out of the
// accessibility tree, so the first scan reports a clean bill of health for markup it did not see"), and a
// modal is the worse case of it: a dialog owns the focus trap, the accessible name and the Escape contract,
// which is exactly the surface axe has rules for.
//
// THE LIST IS DECLARED AND THEN CHECKED AGAINST THE SOURCE, so a sixth dialog cannot ship unscanned. The
// coverage test at the bottom of tests/a11y.spec.ts walks app/**/page.tsx for `<FormDialog` mounts and fails if the
// count does not match the rows here — the same shape as the route coverage assertion, and for the same
// reason: a surface nobody scans must be a red test rather than a thing somebody remembers.
export const FORM_DIALOGS: { route: string; open: string; dialog: string; label: string }[] = [
  { route: "/agents", open: "agent-create-open", dialog: "agent-create-dialog", label: "Create an agent" },
  { route: "/repositories", open: "binding-create-open", dialog: "binding-create-dialog", label: "Register a repository binding" },
  { route: "/policy", open: "key-mint-open", dialog: "key-mint-dialog", label: "Mint an API key" },
  { route: "/fleet", open: "pool-create-open", dialog: "pool-create-dialog", label: "Create a runner pool" },
  { route: "/fleet", open: "poolkey-mint-open", dialog: "poolkey-mint-dialog", label: "Mint an enrolment key" },
];
