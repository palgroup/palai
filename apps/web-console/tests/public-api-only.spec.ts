import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { test, expect, type Request } from "@playwright/test";

import { API_KEY, NEXT_PORT, UPSTREAM, UPSTREAM_PORT } from "./constants";

const APP_ORIGIN = `http://127.0.0.1:${NEXT_PORT}`;
const RELAY_PREFIX = `${APP_ORIGIN}/api/palai/`;

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
test("every console request rides the /v1 relay — no privileged backchannel, no direct upstream/DB", async ({ page, request }) => {
  const requests: Request[] = [];
  page.on("request", (req) => requests.push(req));

  // Exercise the admin surface (many list fetches) and a full live run incl. an approval round-trip and an
  // artifact download — the broadest set of data requests the console makes.
  await page.goto("/");
  await expect(page.getByTestId("panel-organizations")).toContainText("Local Org", { timeout: 15_000 });
  await expect(page.getByTestId("panel-secret-refs")).toContainText("provider-key");

  await page.goto("/runs");
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 15_000 });
  await expect(page.getByTestId("artifact-download")).toBeVisible({ timeout: 15_000 });

  // --- Assertion 1: EVERY data request (fetch/xhr) is same-origin under the /api/palai/ relay. ---
  const dataRequests = requests.filter((r) => r.resourceType() === "fetch" || r.resourceType() === "xhr");
  expect(dataRequests.length, "the console made data requests").toBeGreaterThan(0);
  for (const req of dataRequests) {
    expect(req.url(), `a data request escaped the relay: ${req.url()}`).toContain(RELAY_PREFIX);
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
    expect(haystack, `key leaked in a browser request to ${req.url()}`).not.toContain(API_KEY);
  }

  // --- Assertion 4: the key is in NO static chunk and NO source map. ---
  const assets = browserServedAssets();
  expect(assets.length).toBeGreaterThan(0);
  expect(assets.some((a) => a.path.endsWith(".js.map")), "expected browser source maps").toBe(true);
  for (const asset of assets) {
    expect(asset.body, `key leaked into ${asset.path}`).not.toContain(API_KEY);
  }

  // --- Assertion 5 (upstream end): the relay hit ONLY /v1/* paths, EVERY one Bearer-authenticated —
  // no non-/v1 backchannel, no unauthenticated probe. ---
  const seen = await (await request.get(`${UPSTREAM}/__introspect`)).json();
  expect(seen.beareredV1Requests, "the relay authenticated server-side").toBeGreaterThan(0);
  expect(seen.nonV1Requests, "the relay hit a non-/v1 backchannel").toBe(0);
  expect(seen.unbeareredV1Requests, "the relay sent an unauthenticated /v1 request").toBe(0);
  for (const p of seen.paths as string[]) {
    expect(p.startsWith("/v1/"), `upstream saw a non-/v1 path: ${p}`).toBe(true);
  }
  // Sanity: the upstream really is a different origin, so "same-origin only" above is a real constraint.
  expect(UPSTREAM_PORT).not.toBe(NEXT_PORT);
});
