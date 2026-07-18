import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { test, expect, type Request } from "@playwright/test";

import { API_KEY, UPSTREAM_PORT } from "./constants";

const UPSTREAM = `http://127.0.0.1:${UPSTREAM_PORT}`;

// Every file Next emits under .next/static is browser-fetchable (/_next/static/...), so
// scanning that tree is a real browser-surface scan of both the minified chunks (*.js) and
// their source maps (*.js.map). The key is injected only at runtime via server env, so it
// must be baked into none of them.
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

test("streams canonical events to the browser and never leaks the API key", async ({ page, request }) => {
  // Capture every request the browser makes, to prove the key rides none of them.
  const browserRequests: Request[] = [];
  page.on("request", (req) => browserRequests.push(req));

  await page.goto("/");
  await page.getByTestId("prompt-input").fill("What is 7 + 5?");
  await page.getByTestId("run-button").click();

  // Connection / status signal.
  await expect(page.getByTestId("status")).toHaveText(/streaming/i, { timeout: 15_000 });

  // Text delta signal — streamed model text, assembled from model_step.delta events.
  await expect(page.getByTestId("stream-text")).toContainText("The sum is being computed.", {
    timeout: 15_000,
  });

  // Tool requested + completed signals.
  await expect(page.getByTestId("tool-requested")).toContainText("add", { timeout: 15_000 });
  await expect(page.getByTestId("tool-requested")).toContainText('"a": 7');
  await expect(page.getByTestId("tool-completed")).toContainText("12");

  // Terminal signals: status, the actually-selected model, usage, and the structured result.
  await expect(page.getByTestId("status")).toHaveText(/completed/i, { timeout: 15_000 });
  await expect(page.getByTestId("model")).toContainText("fake");
  await expect(page.getByTestId("usage")).toContainText("32");
  await expect(page.getByTestId("final-output")).toContainText("12");

  // The ordered event timeline shows canonical event types in sequence.
  await expect(page.getByTestId("timeline")).toContainText("model_step.delta.v1");
  await expect(page.getByTestId("timeline")).toContainText("run.completed.v1");

  // --- Surface 1: the key is in NO browser request (headers, URL, or body). ---
  expect(browserRequests.length).toBeGreaterThan(0);
  for (const req of browserRequests) {
    const headers = await req.allHeaders();
    const haystack = `${JSON.stringify(headers)} ${req.url()} ${req.postData() ?? ""}`;
    expect(haystack, `key leaked in a browser request to ${req.url()}`).not.toContain(API_KEY);
  }

  // --- Surfaces 2 & 3: the key is in NO source map and NO static chunk. ---
  const assets = browserServedAssets();
  expect(assets.length).toBeGreaterThan(0);
  const maps = assets.filter((a) => a.path.endsWith(".js.map"));
  const chunks = assets.filter((a) => a.path.endsWith(".js"));
  expect(maps.length, "expected browser source maps to be emitted").toBeGreaterThan(0);
  expect(chunks.length, "expected static JS chunks to be emitted").toBeGreaterThan(0);
  for (const asset of assets) {
    expect(asset.body, `key leaked into ${asset.path}`).not.toContain(API_KEY);
  }
});

test("browser abort closes the transport without cancelling the server run", async ({ page, request }) => {
  const before = await (await request.get(`${UPSTREAM}/__introspect`)).json();

  await page.goto("/");
  await page.getByTestId("prompt-input").fill("What is 7 + 5?");
  await page.getByTestId("run-button").click();

  // Wait until the stream is live (first delta rendered), then abort mid-stream.
  await expect(page.getByTestId("stream-text")).toContainText("The sum", { timeout: 15_000 });
  await page.getByTestId("abort-button").click();
  await expect(page.getByTestId("status")).toHaveText(/aborted/i, { timeout: 15_000 });

  // Give any stray cancel or late frames a window to arrive, then prove the LP6 invariant:
  // the upstream transport closed mid-stream (a stream opened and closed BEFORE its terminal
  // frame), and the run was never cancelled (disconnect ≠ cancel).
  await page.waitForTimeout(750);
  const after = await (await request.get(`${UPSTREAM}/__introspect`)).json();
  expect(after.streamOpens, "a stream was opened server-side").toBeGreaterThan(before.streamOpens);
  expect(after.closes, "the upstream transport closed on abort").toBeGreaterThan(before.closes);
  expect(after.terminalsSent, "abort closed the stream before its terminal frame").toBe(before.terminalsSent);
  expect(after.cancelCalls, "no cancel was sent — disconnect is not cancel").toBe(before.cancelCalls);
});
