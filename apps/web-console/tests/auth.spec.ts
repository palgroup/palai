import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import AxeBuilder from "@axe-core/playwright";
import { test, expect } from "@playwright/test";

import { CONSOLE_ROUTES } from "../lib/routes";
import { CONSOLE_PASSWORD, IS_REAL, NEXT_PORT, UNCONFIGURED_PORT, UPSTREAM, WCAG_TAGS } from "./constants";
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

// THE UPSTREAM HALF OF THE REFUSAL CLAIM IS FIXTURE-ONLY, AND E25 T2 MEASURED IT THE HARD WAY.
//
// Two arms of this file read `${UPSTREAM}/__introspect` to prove the refused request never LEFT the console.
// A real control plane has no such endpoint — DIV-UI-004 records exactly that, and public-api-only.spec.ts
// already narrows the same way — so on the real profile the fetch 404s, `v1Requests` is undefined, and both
// arms failed with `NaN`. That was measured by running this suite against a real compose stack, which T1 never
// did (CON-001 says so: every proof there ran against the fake upstream).
//
// What is NOT done about it: skipping the tests. Every 401/503 assertion is profile-independent and still runs
// on both. What narrows on the real profile is only the SECOND, stronger half — the proof that nothing reached
// the upstream — and it narrows out loud rather than silently: on the fake profile the counter MUST be
// readable, so this cannot decay into a null that always passes.
async function upstreamV1Requests(): Promise<number | null> {
  if (IS_REAL) return null; // DIV-UI-004: no /__introspect on a real control plane.
  const body = (await (await fetch(`${UPSTREAM}/__introspect`)).json()) as { v1Requests?: number };
  expect(typeof body.v1Requests, "the FAKE upstream must expose /__introspect — the upstream half of this proof is not optional here").toBe("number");
  return body.v1Requests as number;
}

function expectNoUpstreamContact(before: number | null, after: number | null, why: string): void {
  if (before === null || after === null) {
    // Real profile only — see upstreamV1Requests. The refusals above already ran.
    // eslint-disable-next-line no-console -- a narrowed claim that says nothing is how a green report lies.
    console.log(`NARROWED ON REAL PROFILE — DIV-UI-004: no upstream introspection, so "${why}" is asserted at the door only`);
    return;
  }
  expect(after - before, why).toBe(0);
}

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

test("an unauthenticated client gets 401 from EVERY relay method — including the approval decision surface", async () => {
  const before = await upstreamV1Requests();

  for (const a of RELAY_SURFACE) {
    const res = await probe(ORIGIN, a.method, a.path, a.data);
    expect(res.status, `${a.method} ${a.path} (${a.what}) must be refused`).toBe(401);
    expect(res.code, `${a.method} ${a.path} must be refused as authentication_required`).toBe("authentication_required");
  }

  // THE REFUSAL IS AT THE DOOR, NOT AT THE UPSTREAM. A 401 could in principle be an upstream answer relayed
  // back; this shows the request never left the console at all. The upstream saw not one additional /v1
  // request while nine anonymous calls were being refused. Before the gate existed, the same nine put five
  // NEW paths into this list — /v1/projects/proj_local and /v1/approvals/apv_1/approve among them.
  const after = await upstreamV1Requests();
  expectNoUpstreamContact(before, after, "an anonymous call reached the control plane");
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

test("FAIL-CLOSED: a console with no PALAI_CONSOLE_PASSWORD_HASH serves nothing — not a read, not a write, not a sign-in", async () => {
  const before = await upstreamV1Requests();

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

  const after = await upstreamV1Requests();
  expectNoUpstreamContact(before, after, "an unconfigured console must not talk to the control plane");

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

  // The same rule set the rest of the console is held to (tests/constants.ts WCAG_TAGS). E25 T2 widened it to
  // include the WCAG 2.1/2.2 tags and this form was re-measured under them: it found NOTHING here, on a page
  // whose criteria (3.3.8 Accessible Authentication, 3.3.7 Redundant Entry) are precisely the 2.2 additions —
  // and the reason is measured in a11y.spec.ts rather than assumed, because widening a tag set adds only the
  // three axe rules that carry those tags, none of which a single-field login form can violate.
  const clean = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
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
  const withError = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(withError.violations, JSON.stringify(withError.violations, null, 2)).toEqual([]);
});

// CON-013: NO CONSOLE FORM CAN PUT A FIELD IN THE URL, AND THE SAFETY IS IN THE HTML RATHER THAN IN A SCRIPT
// ARRIVING.
//
// OBSERVED, on a running console: the operator opened /login, typed the password, submitted — and the address
// bar read `/login?password=<their password>`. From there it was in the browser's history, in the server's
// access log, and in the `Referer` of every request that page made afterwards.
//
// THE CAUSE IS NOT A MISSING preventDefault(). components/ResourceForm.tsx calls it as its handler's first
// statement and always has. That handler only exists once React has HYDRATED, and a `<form>` with no `method`
// defaults to GET (HTML Living Standard, the form element's method attribute: "The missing value default and
// invalid value default are the GET state"). So whenever hydration has not happened, the browser submits
// NATIVELY and every named field goes into the query string. The only thing preventing the leak was
// JavaScript attaching at all.
//
// AND IT IS NOT A NARROW WINDOW. Measured on `next dev` (Next 16.2.10, Turbopack) at 127.0.0.1:3203, with the
// pre-fix component in the tree — NOTHING on the page hydrates, so every form falls back to a native GET,
// every time:
//
//   /login          login-form                ?password=SENTINEL-password
//   /agents         agent-create-form         ?agent-name=SENTINEL-agent-name
//   /environments   environment-create-form   ?environment-name=…&environment-description=…
//   /environments   environment-value-form    ?environment-key=…&value=SENTINEL-value      ← SecretField
//   /fleet          pool-create-form          ?pool-name=…&pool-posture=…&pool-os=…&pool-arch=…
//   /policy         policy-form               ?policy-allowed-models=…&…&policy-approvers=…
//   /repositories   repository-binding-form   ?binding-provider=…&binding-identity=…&binding-clone-url=…&…
//   /tools          mcp-connection-form       ?mcp-name=…&mcp-url=…
//
// AT THE TIME OF THAT MEASUREMENT ten forms rendered server-side across seven routes; eight carried values,
// and all ten navigated with a query string. It is written in the past tense on purpose — the table above is
// a RECORD of what was measured on a tree that no longer exists, and five of those forms are now behind a
// dialog (see the next paragraph). Re-reading it as a present-tense inventory is how a measurement becomes a
// belief; what is true TODAY is derived below rather than described here.
//
// FIVE OF THE TEN HAVE LEFT THE SERVED SWEEP, AND THE COUNT IS NO LONGER A NUMBER ANYBODY TYPES. `/agents`'
// `agent-create-form` and `/repositories`' `repository-binding-form` went behind a `+ Create` button in the
// page-parity pass; `/policy`'s `key-mint-form` and `/fleet`'s `pool-create-form` and `poolkey-mint-form`
// followed in the page-parity-govern pass. A form inside a components/FormDialog.tsx renders only after a
// click, so it is never in the server-rendered HTML and DIRECTION 1 cannot see it.
//
// THE FLOOR IS GONE, AND THAT IS THE POINT. It read `>= 8` while the tree served ten, so it was slack by two
// on the day it landed; two dialogs made it exactly tight, and the third would have turned it red for a
// change that weakens nothing. A floor is what a "the sweep saw something" guard decays into — each author
// lowers it to today's reality and the third time nobody notices it reads `>= 1` — and it cannot be shared,
// because two people moving forms at once means whoever lands second silently breaks a number the first one
// set. DIRECTION 1 now DERIVES its expectation: ResourceForm mounts per page, minus FormDialog mounts per
// page, compared per ROUTE. It self-adjusts as forms move and has no number left to lower.
//
// The PROPERTY is not weakened, and DIRECTION 2 is why it cannot be. Both dialogs wrap the same
// components/ResourceForm.tsx, the source walk still resolves to exactly that one file, and that file still
// carries `method="post"` — which is the direction this test's own comment calls "the one that makes a NEW
// form impossible rather than merely remembered". What narrowed is the served INVENTORY, not the guarantee.
//
// `environment-value-form`'s `value` is SecretField — uncontrolled, living only in the DOM, which is
// exactly the shape a native submit serialises into an address bar. On the same dev server, `POST
// /api/console/login` did not appear in the network log AT ALL, and no element carried a `__reactFiber$` key
// after six seconds.
//
// THE DEV/PRODUCTION DISTINCTION, STATED EXACTLY. On a production build (`next start`) the same probe reports
// `hydrated: true` and `POST /api/console/login` DOES fire, so a production console does not leak the
// password once hydration completes. That is not a reason to call the dev finding harmless: it is the same
// markup either way, the operator hitting it was a real person losing a real credential, and "safe as long as
// a bundle attaches in time" is a property no HTML attribute should have to borrow from JavaScript. The
// dev-mode inertness itself is a SEPARATE defect and is not addressed here.
//
// WHY NOTHING CAUGHT IT: every login proof in this file drives /api/console/login with `fetch`, and the two
// that do use the page (signInViaForm, the keyboard arm above) both act on a HYDRATED page and then assert
// what the response was — never what the ADDRESS BAR then said. The endpoint was proven; the surface a human
// uses was not.
//
// The value typed below is a sentinel that is not a credential anywhere. A leak assertion must never be the
// reason a real password is written into a test, a fixture, a URL or a failure message — and this one prints
// the URL on failure by design.
const URL_LEAK_SENTINEL = "not-a-password-and-never-was";

// THE SWEEP LOOKS IN BOTH DIRECTIONS, because one direction cannot see what the other finds.
//
// The SERVED sweep walks routes and reads what the server actually sent — it catches an unprotected form that
// exists. It CANNOT catch a form that renders only behind data a bootstrap stack has none of. The SOURCE
// sweep walks the tree for `<form` elements — it catches the one the served sweep would never render, and it
// is the direction that makes a NEW form impossible rather than merely remembered.
test("EVERY form the console serves is method=post — the sweep walks the route table AND the source", async () => {
  // DIRECTION 1 — THE CANONICAL ROUTE LIST. lib/routes.ts is the console's one source of navigation, so a new
  // page adds a row there and is swept here without anyone remembering to. `/login` is added explicitly
  // because it is deliberately NOT in that table (it lives outside the session gate — see the axe comment on
  // the login page), and a hand-written list of one is the honest shape of that exception.
  //
  // No session: the static shell renders for an anonymous client (the gate is in the relay, not the layout —
  // see the FAIL-CLOSED test above), which is why a plain fetch is enough to see this markup.
  const swept: string[] = [];
  const servedPerRoute = new Map<string, number>();
  const routes = ["/login", ...CONSOLE_ROUTES.map((r) => r.path)];
  for (const path of routes) {
    const html = await (await fetch(`${ORIGIN}${path}`)).text();
    const tags = html.match(/<form[^>]*>/g) ?? [];
    servedPerRoute.set(path, tags.length);
    for (const tag of tags) {
      const testId = /data-testid="([^"]+)"/.exec(tag)?.[1] ?? "(no testid)";
      swept.push(`${path} ${testId}`);
      expect(tag, `${path} serves a form with no method="post" — a native submit puts every named field in the query string: ${tag}`).toMatch(/method="post"/i);
    }
  }
  // eslint-disable-next-line no-console -- the inventory IS the claim; a reader must see what was covered.
  console.log(`FORM METHOD SWEEP — ${swept.length} served form(s): ${swept.join(", ")}`);

  // THE COUNT IS DERIVED FROM THE SOURCE, AND IT USED TO BE A HAND-TYPED FLOOR (`>= 8`).
  //
  // A floor is what a "the sweep saw something" guard degrades into. It was written when the tree held ten
  // forms, so it was already slack by two on the day it landed; the first form to move into a dialog
  // legitimately drops the served count, the author lowers the number to match, and the third time nobody
  // notices it reads `>= 1`. It also cannot be shared: with two people moving forms at once, whoever lands
  // second silently breaks a number the first one set.
  //
  // So the expectation is COMPUTED. `components/ResourceForm.tsx` is structurally the console's only form
  // element — direction 2 below asserts exactly that, over the whole tree — so every form the console can
  // serve is one `<ResourceForm` mount in one `app/**/page.tsx`, and counting those mounts per page gives
  // the number that page must serve. The comparison is per ROUTE rather than in total: a form that moves
  // between two pages keeps the total identical and is caught here.
  //
  // A MOUNT THE SERVER CANNOT RENDER IS SUBTRACTED FROM THE SOURCE, NOT EXCUSED BY A SMALLER NUMBER — and
  // the subtrahend is counted rather than declared. components/FormDialog.tsx is the shell a create form is
  // put behind (`+ Create X`), and a form inside one renders only after a click, so it is in the source and
  // never in the server-rendered HTML. Counting `<FormDialog` mounts per page therefore gives exactly the
  // forms this route legitimately does not serve, with no list for anybody to keep up to date.
  //
  // THE ASSUMPTION IS ONE FORM PER DIALOG, and it is asserted rather than assumed one line down: a page
  // mounting more FormDialogs than ResourceForms is either a dialog wrapping something that is not a form or
  // a form that went missing, and both deserve to be read rather than subtracted away.
  const mountsPerRoute = new Map<string, number>();
  const appRoot = resolve(process.cwd(), "app");
  for (const entry of readdirSync(appRoot, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile() || entry.name !== "page.tsx") continue;
    const full = resolve(entry.parentPath ?? appRoot, entry.name);
    const src = readFileSync(full, "utf8");
    const mounts = (src.match(/<ResourceForm[\s>]/g) ?? []).length;
    const behindDialog = (src.match(/<FormDialog[\s>]/g) ?? []).length;
    if (mounts === 0 && behindDialog === 0) continue;
    // app/page.tsx -> "/", app/policy/page.tsx -> "/policy". The same mapping Next uses for a static route.
    const dir = full.slice(appRoot.length + 1).replace(/\/?page\.tsx$/, "");
    const path = dir === "" ? "/" : `/${dir}`;
    expect(
      behindDialog,
      `${path} mounts ${String(behindDialog)} FormDialog(s) and only ${String(mounts)} ResourceForm(s) — this ` +
        "derivation subtracts one form per dialog, so either a dialog wraps something that is not a form or a " +
        "form went missing, and neither should be silently subtracted",
    ).toBeLessThanOrEqual(mounts);
    mountsPerRoute.set(path, mounts - behindDialog);
  }

  // A FORM ON A ROUTE THIS SWEEP NEVER VISITS IS THE HOLE THE DERIVATION OPENS, so it is closed first: a page
  // mounting a form must be one of the paths fetched above. A dynamic segment (`app/sessions/[id]`) cannot be
  // fetched by this loop, so a form appearing there would otherwise be swept by nothing at all.
  const unswept = [...mountsPerRoute.keys()].filter((path) => !routes.includes(path));
  expect(unswept, "a page mounts a form on a route the served sweep cannot fetch — lib/routes.ts declares the swept paths, and a dynamic segment is not one of them").toEqual([]);
  const expected = [...mountsPerRoute.entries()]
    .filter(([, served]) => served > 0)
    .map(([path, served]) => `${path}=${String(served)}`)
    .sort();
  const actual = [...servedPerRoute.entries()]
    .filter(([, n]) => n > 0)
    .map(([path, n]) => `${path}=${String(n)}`)
    .sort();
  // eslint-disable-next-line no-console -- the derivation is the evidence; a bare pass hides which side moved.
  console.log(`FORM COUNT DERIVED — source mounts ${expected.join(", ")}; served ${actual.join(", ")}`);
  // The derivation must have found SOMETHING. A walk that matched nothing would make the comparison below
  // `[] === []`, which is the shape of green this repository keeps having to re-measure.
  expect(expected.length, "no page.tsx mounts a ResourceForm — the source walk matched nothing and is asserting nothing").toBeGreaterThan(0);
  expect(
    actual,
    "the forms the server SENT are not the forms the source MOUNTS MINUS the ones behind a FormDialog. A page " +
      "stopped rendering a form it declares, or a dialog was added around something this walk cannot see. " +
      "Neither is fixed by a smaller number here — the count is derived and has no number to lower.",
  ).toEqual(expected);

  // DIRECTION 2 — THE SOURCE WALK. Every `<form` element in the tree must be ResourceForm's, because that is
  // the one that carries the attribute. A second `<form>` anywhere else would be a form nobody gave a method,
  // and if it renders only behind data (a selected tool, a non-empty list) the sweep above would never see it.
  //
  // The match is deliberately blunt: a `<form` in a COMMENT counts too, which means a file that merely
  // discusses form markup fails this. That is the safe direction — it fails loudly and a human reads two
  // lines — where the alternative is a parser that quietly disagrees with the compiler.
  const roots = ["app", "components", "lib"].map((d) => resolve(process.cwd(), d));
  const withForm: string[] = [];
  for (const root of roots) {
    for (const entry of readdirSync(root, { recursive: true, withFileTypes: true })) {
      if (!entry.isFile() || !/\.tsx?$/.test(entry.name)) continue;
      const full = resolve(entry.parentPath ?? root, entry.name);
      if (/<form[\s>]/.test(readFileSync(full, "utf8"))) withForm.push(full.replace(`${process.cwd()}/`, ""));
    }
  }
  expect(withForm, "exactly one file may contain a <form> element, and it must be the shared one that carries method=post").toEqual(["components/ResourceForm.tsx"]);
  expect(readFileSync(resolve(process.cwd(), "components/ResourceForm.tsx"), "utf8"), "the one form element in the tree must carry method=post").toMatch(/<form[\s\S]*?method="post"[\s\S]*?>/);
});

test("the login form cannot put the password in the URL — method=post is in the HTML, so no script has to arrive", async ({ page, baseURL }) => {
  // ARM 1 — THE SERVED BYTES for the one form that carries a credential. The sweep above covers all ten; this
  // repeats it for login so a failure here reads as "the credential form" rather than as one row of a list.
  const html = await (await fetch(`${ORIGIN}/login`)).text();
  const formTag = /<form[^>]*data-testid="login-form"[^>]*>/.exec(html)?.[0] ?? "";
  expect(formTag, "the login form must be in the SERVER-RENDERED html — a client-only form has no pre-hydration shape to assert").not.toBe("");
  expect(formTag, `a form with no method= submits as GET and puts every named field in the query string: ${formTag}`).toMatch(/method="post"/i);

  // ARM 2 — THE FAILURE MODE, REPRODUCED DETERMINISTICALLY. The page's own scripts are blocked, so React
  // never hydrates: the same state `next dev` is in permanently, held open on a production build. Playwright's
  // own tooling is injected into an isolated world, so fill() and click() still work here.
  //
  // WHY THIS RATHER THAN A `next dev` SERVER, which is the more faithful reproduction and was measured to
  // work: (1) `next dev` and `next start` share `.next`, and this suite serves two production consoles from
  // it while tests/profile.ts browserServedAssets() walks `.next/static` for the two secret sweeps — a dev
  // server writing there is a change underneath the specs this task must not weaken; (2) the dev-mode
  // hydration failure is a separate defect that is being routed for a FIX, and an arm that depends on it
  // would silently stop testing the pre-hydration window the day it is fixed, passing for the wrong reason.
  // Blocking the scripts asserts the property directly and cannot decay.
  await page.route(/\/_next\/static\/.*\.js(\?.*)?$/, (route) => route.abort());
  await page.goto("/login");
  const form = page.getByTestId("login-form");
  await expect(form).toBeVisible();
  // THE ARM MUST PROVE IT IS UNHYDRATED, or it is asserting preventDefault() and nothing else — and the
  // obvious ways to check that are BOTH vacuous, measured rather than reasoned about:
  //
  //   scripts allowed  {"onsubmitNull":true,"literalKey":false,"prefixedKeys":["__reactFiber$t6kmppufgoj","__reactProps$t6kmppufgoj"]}
  //   scripts blocked  {"onsubmitNull":true,"literalKey":false,"prefixedKeys":[]}
  //
  // `el.onsubmit === null` is true on a HYDRATED form too (React binds synthetic events at the root and never
  // sets the DOM property), and `"__reactProps$" in el` is false on a hydrated form too — React 19 suffixes
  // that key with a per-render id, so the bare name is never present. Either check passes in both states and
  // would have let this arm run against a fully hydrated page while looking rigorous. The PREFIX is the only
  // one of the three that separates them.
  const hydrated = await form.evaluate((el) => Object.keys(el).some((k) => k.startsWith("__reactProps$")));
  expect(hydrated, "scripts were blocked, so React must NOT have hydrated — otherwise this arm proves preventDefault(), not the markup").toBe(false);

  await page.getByTestId("password-input").fill(URL_LEAK_SENTINEL);
  await Promise.all([page.waitForLoadState("load"), page.getByTestId("login-button").click()]);

  const landed = new URL(page.url());
  expect(landed.search, `the credential reached the URL: ${page.url()}`).toBe("");
  expect(page.url(), "no part of a submitted field may appear in the address").not.toContain(URL_LEAK_SENTINEL);
  // And the Referer that page would now stamp on every subsequent request carries nothing either.
  expect(await page.evaluate(() => document.location.href).catch(() => landed.href)).not.toContain(URL_LEAK_SENTINEL);

  // WHAT THIS DELIBERATELY DOES NOT CLAIM: that a pre-hydration submit SIGNS IN. It does not, and it must
  // not — /api/console/login takes JSON, so there is no `action` that a native form-encoded POST could
  // usefully reach (see components/ResourceForm.tsx). A pre-hydration submit is a visible failure, and a
  // visible failure is the correct trade against a silent leak. The claim is only that the credential never
  // travels in the address, which is the property that survives the browser's history and the access log.
  expect(baseURL, "sanity: the arms above ran against the configured console").toBe(ORIGIN);
});
