import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { test, expect, type Request, type Response as NetResponse } from "@playwright/test";

import { API_KEY, IS_REAL, NEXT_PORT, UPSTREAM, UPSTREAM_PORT } from "./constants";
import { announceProfile, runToTerminal, signInViaForm } from "./profile";

test.beforeAll(() => announceProfile("public-api-only.spec.ts"));

const APP_ORIGIN = `http://127.0.0.1:${NEXT_PORT}`;
const RELAY_PREFIX = `${APP_ORIGIN}/api/palai/`;
// THE SINGLE EXCEPTION (E25 T1, §2). The console's door needs one same-origin path that is not under the
// relay, because a session cannot be obtained through the /v1 public API. It is named here, once, and
// COUNTED: assertion 1 requires it to be the only non-relay data request AND requires it to actually occur,
// so an exception that stopped being exercised could not sit here unnoticed. An exception is not an
// exception unless it is counted.
const LOGIN_PATH = `${APP_ORIGIN}/api/console/login`;

// browserServedAssets scans .next/static — every file there is browser-fetchable (/_next/static/...), so
// this is a real browser-surface scan of both the minified chunks (*.js) and their source maps (*.js.map).
function browserServedAssets(): { path: string; body: string }[] {
  const root = resolve(process.cwd(), ".next", "static");
  const out: { path: string; body: string }[] = [];
  for (const entry of readdirSync(root, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile()) continue;
    const full = resolve(entry.parentPath ?? root, entry.name);
    out.push({ path: full, body: readFileSync(full, "utf8") });
  }
  return out;
}

// THE CROWN (§47.6): the console is public-API-only. This is the mechanically un-foolable proof — the
// browser physically CANNOT reach the upstream control-plane or any privileged backchannel/DB, because
// every request it makes is captured and asserted to be same-origin under the /api/palai/ relay (which
// itself can only address /v1/*). Cross-origin egress to the upstream, a DB socket, or any non-relay data
// path would surface here as a captured request and FAIL. The upstream's own introspection closes the
// loop from the other end: it received ONLY /v1/* paths, every one carrying a server-side Bearer.
//
// E19 T7 — THIS CROWN NOW RUNS ON BOTH PROFILES, AND THE REAL ONE IS THE STRONGER CLAIM. On the real
// profile the credential being scanned for is a LIVE bootstrap key against a running control plane, not a
// sentinel string: assertions 1-4, 6 and 7 are the same code proving the same thing about a real secret and
// a real cross-origin upstream. Only assertion 5 — the upstream's own view of what it received — is
// fixture-only, because a real control plane has no introspection endpoint (DIV-UI-004). That is the
// weaker half; the browser end is the one that would catch a backchannel, and it runs everywhere.
//
// The key is compared with a BOOLEAN rather than `.not.toContain(API_KEY)` throughout: a failing matcher
// prints both operands, which on the real profile would print a live credential into the test log.
test("every console request rides the /v1 relay — no privileged backchannel, no direct upstream/DB", async ({ page, request }) => {
  const requests: Request[] = [];
  page.on("request", (req) => requests.push(req));
  // page.on("request") does NOT fire for WebSocket or service-worker fetches — collect those separately so
  // the "no backchannel of ANY type" claim holds against the request types the request event cannot see.
  const responses: NetResponse[] = [];
  page.on("response", (resp) => responses.push(resp));
  const websockets: string[] = [];
  page.on("websocket", (ws) => websockets.push(ws.url()));

  // Sign in through the FORM, in the browser, so the login request is captured by the intercept above and
  // has to survive assertions 1-3 like every other request — that is what makes "one exception" checkable
  // rather than asserted. It also lands on / and renders the admin surface.
  await signInViaForm(page);

  // Exercise the admin surface (many list fetches) and a full live run — the broadest set of data requests
  // the console makes on this profile.
  await page.goto("/");
  await expect(page.getByTestId("panel-organizations")).toContainText("org_local", { timeout: 15_000 });
  if (IS_REAL) {
    // /v1/secret-refs is not registered on a compose stack (DIV-RTE-001), so the panel renders its ERROR
    // state. That path is exercised here on purpose: an unmounted capability is a real operator-facing
    // state, and it still has to ride the relay like everything else.
    await expect(page.getByTestId("panel-secret-refs")).toBeVisible();
  } else {
    await expect(page.getByTestId("panel-secret-refs")).toContainText("provider-key");
  }

  await runToTerminal(page);
  if (!IS_REAL) await expect(page.getByTestId("artifact-download")).toBeVisible({ timeout: 15_000 });

  // --- Assertion 1: EVERY data request (fetch/xhr) is same-origin under the /api/palai/ relay, with
  // /api/console/login as the ONE named exception — and the exception is COUNTED in both directions: it must
  // occur (so it is a live exception, not a dead licence) and nothing else may join it. ---
  const dataRequests = requests.filter((r) => r.resourceType() === "fetch" || r.resourceType() === "xhr");
  expect(dataRequests.length, "the console made data requests").toBeGreaterThan(0);
  const loginRequests = dataRequests.filter((r) => r.url() === LOGIN_PATH);
  expect(loginRequests.length, "the sign-in exception must actually be exercised here").toBeGreaterThan(0);
  for (const req of dataRequests) {
    if (req.url() === LOGIN_PATH) continue;
    expect(req.url(), `a data request escaped the relay: ${req.url()}`).toContain(RELAY_PREFIX);
  }
  // The exception carries NO Bearer and NO API key. A session is not a control-plane credential, and the
  // console's key must never ride a browser request — least of all the one request that is not the relay.
  for (const req of loginRequests) {
    const headers = await req.allHeaders();
    expect(headers["authorization"], "the sign-in request must carry no Bearer").toBeUndefined();
    expect(JSON.stringify(headers).toLowerCase(), "the sign-in request must carry no bearer of any spelling").not.toContain("bearer");
    expect((req.postData() ?? "").includes(API_KEY), "the API key must not ride the sign-in body").toBe(false);
  }

  // --- Assertion 2: NO browser request of ANY type reaches the upstream origin (or any other host). ---
  for (const req of requests) {
    expect(req.url().startsWith(`${UPSTREAM}/`), `a browser request reached the upstream directly: ${req.url()}`).toBe(false);
    expect(req.url().startsWith(APP_ORIGIN), `a browser request left the app origin: ${req.url()}`).toBe(true);
  }

  // --- Assertion 3: the API key is in NO browser request (headers, URL, or body). ---
  for (const req of requests) {
    const headers = await req.allHeaders();
    const haystack = `${JSON.stringify(headers)} ${req.url()} ${req.postData() ?? ""}`;
    expect(haystack.includes(API_KEY), `key leaked in a browser request to ${req.url()}`).toBe(false);
  }

  // --- Assertion 4: the key is in NO static chunk and NO source map. ---
  const assets = browserServedAssets();
  expect(assets.length).toBeGreaterThan(0);
  expect(assets.some((a) => a.path.endsWith(".js.map")), "expected browser source maps").toBe(true);
  for (const asset of assets) {
    expect(asset.body.includes(API_KEY), `key leaked into ${asset.path}`).toBe(false);
  }

  // --- Assertion 5 (upstream end): the relay hit ONLY /v1/* paths, EVERY one Bearer-authenticated —
  // no non-/v1 backchannel, no unauthenticated probe. FIXTURE-ONLY: a real control plane ships no
  // introspection endpoint (DIV-UI-004), so on the real profile this half is absent and the browser-end
  // assertions above carry the claim alone. ---
  if (!IS_REAL) {
    const seen = await (await request.get(`${UPSTREAM}/__introspect`)).json();
    expect(seen.beareredV1Requests, "the relay authenticated server-side").toBeGreaterThan(0);
    expect(seen.nonV1Requests, "the relay hit a non-/v1 backchannel").toBe(0);
    expect(seen.unbeareredV1Requests, "the relay sent an unauthenticated /v1 request").toBe(0);
    // E25 T1 — THE OPERATOR SESSION DOES NOT LEAK UPSTREAM. The whole run above happened with a session
    // cookie set on this origin, and not one upstream request carried a Cookie header. The relay does not
    // forward incoming headers (it calls client.request with a method, a path and a body), so this is
    // structural — and counted rather than assumed, because "structural" is what this tree keeps having to
    // re-measure. It matters concretely: a session forwarded upstream would be an operator credential
    // sitting in a control-plane access log.
    expect(seen.cookieBearingV1Requests, "the console's session cookie rode an upstream /v1 request").toBe(0);
    for (const p of seen.paths as string[]) {
      expect(p.startsWith("/v1/"), `upstream saw a non-/v1 path: ${p}`).toBe(true);
    }
  }
  // Sanity: the upstream really is a different origin, so "same-origin only" above is a real constraint.
  // On the real profile the upstream is a compose port that is likewise never 3200.
  expect(new URL(UPSTREAM).port).not.toBe(String(NEXT_PORT));
  expect(UPSTREAM_PORT).not.toBe(NEXT_PORT);

  // --- Assertion 6: the key is in NO RESPONSE BODY. A server component that leaked process.env.PALAI_API_KEY
  // into a client prop ships in the page HTML / RSC flight payload — not in .next/static, not in any request
  // (assertions 3-4) — so this scans the one leak class those miss. ---
  for (const resp of responses) {
    let body: string;
    try {
      body = await resp.text();
    } catch {
      continue; // body not retained (redirect / aborted / no content) — nothing to scan
    }
    expect(body.includes(API_KEY), `key leaked in a response body from ${resp.url()}`).toBe(false);
  }

  // --- Assertion 7: NO WebSocket backchannel and NO service worker (neither surfaces in page.on("request"),
  // so both would be a blind spot in assertions 1-2). There is none today; this hardens "ANY type". ---
  expect(websockets, `the console opened a websocket backchannel: ${websockets.join(", ")}`).toEqual([]);
  const serviceWorkers = await page.evaluate(async () => {
    if (!("serviceWorker" in navigator)) return 0;
    return (await navigator.serviceWorker.getRegistrations()).length;
  });
  expect(serviceWorkers, "a service worker was registered — an uncaptured fetch backchannel").toBe(0);
});
