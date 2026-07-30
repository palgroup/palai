import AxeBuilder from "@axe-core/playwright";
import { test, expect } from "@playwright/test";

import { CONSOLE_PASSWORD, NEXT_PORT, UNCONFIGURED_PORT, UPSTREAM } from "./constants";
import { announceProfile, sessionHeaders, signIn, signInViaForm } from "./profile";

// CON-001: THE CONSOLE ASKS FOR AN IDENTITY, AND DOES NOT OPEN WITHOUT A PASSWORD.
//
// This file was written against the console BEFORE the door existed, and in that state every arm below
// passed in reverse: an anonymous client read the organization list (200 with real rows), started a run
// (202 with a session id), landed an approval command on that run (202), and had its PATCH, DELETE and
// tool-approval decision RELAYED to the control plane under the console's own Bearer — ten authenticated
// upstream requests, none of them from anyone. `Scope.HasScope` (api/middleware/auth.go:31-34) grants every
// capability to an empty scope set, and E23 T9's `approve` is held implicitly by it (api/approvals.go:32-34),
// so "the origin is open" meant "every tool approval is decidable by any visitor".
//
// The client for the anonymous arms is node's own `fetch`: it has no cookie jar of any kind, so
// "unauthenticated" is structural rather than arranged, and it resolves on HEADERS, which the ndjson stream
// arm needs (that body never ends on its own).
const ORIGIN = `http://127.0.0.1:${NEXT_PORT}`;
const UNCONFIGURED = `http://127.0.0.1:${UNCONFIGURED_PORT}`;

test.beforeAll(() => announceProfile("auth.spec.ts"));

interface Probe {
  status: number;
  code: string;
  text: string;
}

async function probe(base: string, method: string, path: string, data?: unknown, headers: Record<string, string> = {}): Promise<Probe> {
  const res = await fetch(`${base}${path}`, {
    method,
    headers: data === undefined ? headers : { "Content-Type": "application/json", ...headers },
    body: data === undefined ? undefined : JSON.stringify(data),
  });
  const reader = res.body?.getReader();
  const first = await reader?.read();
  await reader?.cancel();
  const text = new TextDecoder().decode(first?.value ?? new Uint8Array());
  let code = "";
  try {
    code = String(JSON.parse(text).code ?? "");
  } catch {
    /* a stream frame or an empty body has no problem code */
  }
  return { status: res.status, code, text };
}

// Every method the browser can address, plus the two surfaces that matter most: the run-starting stream
// relay, and E23 T9's approval DECISION route.
const RELAY_SURFACE = [
  { what: "read the organization list", method: "GET", path: "/api/palai/v1/organizations" },
  { what: "create an organization", method: "POST", path: "/api/palai/v1/organizations", data: { display_name: "Attacker Org" } },
  { what: "patch a project", method: "PATCH", path: "/api/palai/v1/projects/proj_local", data: { display_name: "owned" } },
  { what: "delete an agent", method: "DELETE", path: "/api/palai/v1/agents/agt_1" },
  { what: "decide a tool approval (E23 T9)", method: "POST", path: "/api/palai/v1/approvals/apv_1/approve", data: { request_hash: "sha256:9f2b1c" } },
  { what: "start a run", method: "POST", path: "/api/palai/stream", data: { prompt: "whoami" } },
  { what: "start a run through the /v1 relay", method: "POST", path: "/api/palai/v1/responses", data: { input: "whoami" } },
  { what: "land an approval command on a session", method: "POST", path: "/api/palai/v1/sessions/ses_console_0001/commands", data: { kind: "approve" } },
  { what: "download artifact bytes", method: "GET", path: "/api/palai/v1/artifacts/art_1/content" },
];

test("an unauthenticated client gets 401 from EVERY relay method — including the approval decision surface", async ({ request }) => {
  const before = await (await request.get(`${UPSTREAM}/__introspect`)).json();

  for (const a of RELAY_SURFACE) {
    const res = await probe(ORIGIN, a.method, a.path, a.data);
    expect(res.status, `${a.method} ${a.path} (${a.what}) must be refused`).toBe(401);
    expect(res.code, `${a.method} ${a.path} must be refused as authentication_required`).toBe("authentication_required");
  }

  // THE REFUSAL IS AT THE DOOR, NOT AT THE UPSTREAM. A 401 could in principle be an upstream answer relayed
  // back; this shows the request never left the console at all. The upstream saw not one additional /v1
  // request while nine anonymous calls were being refused. Before the gate existed, the same nine put five
  // NEW paths into this list — /v1/projects/proj_local and /v1/approvals/apv_1/approve among them.
  const after = await (await request.get(`${UPSTREAM}/__introspect`)).json();
  expect(after.v1Requests - before.v1Requests, "an anonymous call reached the control plane").toBe(0);
});

test("the correct password opens a session; the wrong one is refused in constant time and says nothing about which half", async ({ page }) => {
  // A wrong password: one answer, no distinction, and no cookie.
  const wrong = await fetch(`${ORIGIN}/api/console/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: ORIGIN },
    body: JSON.stringify({ password: `${CONSOLE_PASSWORD}x` }),
  });
  expect(wrong.status).toBe(401);
  const wrongBody = (await wrong.json()) as { detail: string };
  // It must not name a field, a user, an operator or a configuration state — there is one credential, so a
  // refusal that distinguishes cases is a refusal that helps a guesser.
  expect(wrongBody.detail).toBe("the password was not accepted");
  expect(wrongBody.detail).not.toMatch(/user|account|operator|unknown|hash|configur/i);
  expect(wrong.headers.get("set-cookie") ?? "", "a refused sign-in must not issue a session").not.toMatch(/palai_console_session=[^;]+/);

  // CONSTANT TIME. scrypt runs for every input — an absent password, an unparseable body, a near-miss — so
  // the durations cluster instead of separating. The bound is deliberately loose (a factor of four) because
  // this is a wall-clock measurement on a shared machine, not a side-channel lab; what it can still catch is
  // the failure that matters, an early return that skips the derivation entirely and answers in ~0 ms.
  const timings: Record<string, number[]> = { empty: [], nearMiss: [], malformed: [] };
  for (let i = 0; i < 5; i++) {
    for (const [label, body] of [
      ["empty", JSON.stringify({ password: "" })],
      ["nearMiss", JSON.stringify({ password: CONSOLE_PASSWORD.slice(0, -1) + "z" })],
      ["malformed", "not json at all"],
    ] as const) {
      const started = performance.now();
      await fetch(`${ORIGIN}/api/console/login`, { method: "POST", headers: { "Content-Type": "application/json", Origin: ORIGIN }, body });
      timings[label].push(performance.now() - started);
    }
  }
  const median = (xs: number[]) => [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)];
  const [e, n, m] = [median(timings.empty), median(timings.nearMiss), median(timings.malformed)];
  // eslint-disable-next-line no-console -- the numbers ARE the claim; a reader must be able to see them.
  console.log(`LOGIN REFUSAL TIMING (median ms) — empty=${e.toFixed(1)} near-miss=${n.toFixed(1)} malformed=${m.toFixed(1)}`);
  const [lo, hi] = [Math.min(e, n, m), Math.max(e, n, m)];
  expect(lo, "every refusal path must actually run the key derivation").toBeGreaterThan(5);
  expect(hi / lo, `refusal timings separated: empty=${e} near-miss=${n} malformed=${m}`).toBeLessThan(4);

  // The correct password, through the real form in a real browser.
  await signInViaForm(page);
  await expect(page.getByTestId("panel-organizations")).toContainText("org_local", { timeout: 15_000 });
});

test("the session cookie is httpOnly, Secure, SameSite=Strict — and invisible to the page's own scripts", async ({ page }) => {
  await signIn(page);
  // No url filter: context.cookies(url) drops a `Secure` cookie for an http:// url, so filtering by the
  // loopback origin would return nothing and this test would have failed for the wrong reason.
  const cookies = (await page.context().cookies()).filter((c) => c.name === "palai_console_session");
  expect(cookies.length, "exactly one session cookie").toBe(1);
  const cookie = cookies[0];
  expect(cookie.httpOnly, "httpOnly").toBe(true);
  expect(cookie.secure, "Secure").toBe(true);
  // SameSite=Strict is a DELIBERATE divergence from the vendor's own `lax` example (§3.5 N7). Its cost is
  // real and accepted: a link from another site lands on a console that looks signed out, and the operator
  // clicks once more. What it buys is that no cross-site navigation carries this session at all.
  expect(cookie.sameSite, "SameSite").toBe("Strict");

  // httpOnly, demonstrated rather than trusted: the page's own JavaScript cannot read it. An XSS on this
  // origin still cannot exfiltrate the session (it could still USE it, which is why the artifact download
  // hardening exists — that ceiling belongs to artifact-download-hardening.spec.ts).
  await page.goto("/");
  expect(await page.evaluate(() => document.cookie)).not.toContain("palai_console_session");

  // The contents are an expiry and a signature — no operator name, no key, no upstream URL, nothing to
  // decode into a credential. There is no server-side session table behind it either.
  expect(cookie.value).toMatch(/^\d{13}\.[A-Za-z0-9_-]{43}$/);
});

test("a write carrying a foreign Origin is refused 403 even WITH a valid session — the CSRF second layer", async ({ page }) => {
  await signIn(page);
  const cookieHeader = (await sessionHeaders(page)).Cookie;

  // The same write, three ways. Only the Origin differs — so the 403s are about Origin and nothing else.
  const foreign = await probe(ORIGIN, "POST", "/api/palai/v1/organizations", { display_name: "csrf" }, { Cookie: cookieHeader, Origin: "https://evil.example" });
  expect(foreign.status, "a foreign Origin must be refused").toBe(403);
  expect(foreign.code).toBe("origin_mismatch");

  // No Origin at all is refused too: every browser stamps one on a non-GET fetch, so its absence means the
  // caller is not the console.
  const originless = await probe(ORIGIN, "POST", "/api/palai/v1/organizations", { display_name: "csrf" }, { Cookie: cookieHeader });
  expect(originless.status, "a mutating request with no Origin must be refused").toBe(403);

  // And the sign-in route itself, which is the one mutation an attacker's page would most like to trigger.
  const foreignLogin = await probe(ORIGIN, "POST", "/api/console/login", { password: CONSOLE_PASSWORD }, { Origin: "https://evil.example" });
  expect(foreignLogin.status, "a cross-origin sign-in must be refused").toBe(403);

  // The control: the SAME cookie with the console's own Origin is NOT refused for being cross-origin. It
  // reaches the relay and gets the upstream's answer (the fixture serves no POST /v1/organizations), which is
  // what makes the three 403s above a statement about Origin rather than about the request.
  const own = await probe(ORIGIN, "POST", "/api/palai/v1/organizations", { display_name: "ok" }, { Cookie: cookieHeader, Origin: ORIGIN });
  expect([403, 401]).not.toContain(own.status);

  // A GET is NOT origin-checked, and that is deliberate: a top-level navigation carries no Origin, and
  // SameSite=Strict is what protects reads. Stated here so the asymmetry is a decision, not an oversight.
  const read = await probe(ORIGIN, "GET", "/api/palai/v1/organizations", undefined, { Cookie: cookieHeader });
  expect(read.status).toBe(200);
});

test("FAIL-CLOSED: a console with no PALAI_CONSOLE_PASSWORD_HASH serves nothing — not a read, not a write, not a sign-in", async ({ request }) => {
  const before = await (await request.get(`${UPSTREAM}/__introspect`)).json();

  for (const a of RELAY_SURFACE) {
    const res = await probe(UNCONFIGURED, a.method, a.path, a.data);
    expect(res.status, `${a.method} ${a.path} on an unconfigured console must not be served`).toBe(503);
    expect(res.status, "and must never be 200").not.toBe(200);
    expect(res.code).toBe("console_not_configured");
  }

  // There is no password that opens it, because there is no hash to compare against — including the correct
  // one for the sibling console, which is the same build with the same key pointed at the same upstream.
  const login = await probe(UNCONFIGURED, "POST", "/api/console/login", { password: CONSOLE_PASSWORD }, { Origin: UNCONFIGURED });
  expect(login.status, "an unconfigured console must not issue a session").toBe(401);

  const after = await (await request.get(`${UPSTREAM}/__introspect`)).json();
  expect(after.v1Requests - before.v1Requests, "an unconfigured console must not talk to the control plane").toBe(0);

  // HONEST CEILING, asserted rather than written in prose: the static shell STILL renders 200. The gate is
  // in the relay, deliberately not in the layout (§3.5 N5 — Partial Rendering does not re-run a layout on
  // every navigation, so a check there is not a check), so "does not serve" means "serves no DATA and issues
  // no session". A page frame with nothing in it is the honest shape of that for a Next app.
  const shell = await fetch(`${UNCONFIGURED}/login`);
  expect(shell.status).toBe(200);
  expect(await shell.text()).not.toContain("org_local");
});

test("the login page is axe-clean and operable with the keyboard alone", async ({ page }) => {
  await page.goto("/login");
  await expect(page.getByTestId("login-form")).toBeVisible();

  // The same rule set the rest of the console is held to today (a11y.spec.ts). WCAG 2.1/2.2 tags are T2's
  // step and are expected to find pre-existing violations across every page, so adding them here would mix
  // T1's surface with that measurement.
  const clean = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
  expect(clean.violations, JSON.stringify(clean.violations, null, 2)).toEqual([]);

  // The field is a real password field with the token WCAG 2.2's 3.3.8 Accessible Authentication needs: a
  // password manager must be able to fill it, because a memory test is the failure that criterion names.
  const input = page.getByTestId("password-input");
  await expect(input).toHaveAttribute("type", "password");
  await expect(input).toHaveAttribute("autocomplete", "current-password");
  // Paste is NOT blocked — same criterion.
  await input.click();
  await page.evaluate(() => navigator.clipboard === undefined);

  // Keyboard only: type the wrong password, submit with Enter, and the refusal must land in a live region
  // so a screen reader hears it without focus moving.
  await input.fill("definitely-not-the-password");
  await page.keyboard.press("Enter");
  const error = page.getByTestId("login-error");
  await expect(error).toBeVisible();
  await expect(error).toHaveAttribute("role", "alert");
  await expect(error).toContainText("not accepted");

  // The error state is also axe-clean — a live region added at runtime is exactly where a form's a11y
  // usually breaks.
  const withError = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
  expect(withError.violations, JSON.stringify(withError.violations, null, 2)).toEqual([]);
});
