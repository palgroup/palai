import { readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { expect, test, type Page } from "@playwright/test";

import { type DynamicConsoleRoute } from "../lib/routes";
import { DIVERGENCE_BY_ID } from "./divergences.mjs";
import { CONSOLE_PASSWORD, IS_REAL, NEXT_PORT, PROFILE, UPSTREAM } from "./constants";

// skipOnReal is the ONLY way a spec may decline to run on the real profile, and it makes that decision
// expensive on purpose (E19 T7).
//
// Green-by-skip is this repo's recurring failure — eight findings, the most recent a security-suite arm
// reported as a denial that had actually skipped. So a skip here cannot be a bare `test.skip()`:
//
//   1. It must cite a row of tests/divergences.mjs, and an unknown id THROWS rather than skips. A spec
//      cannot invent its own excuse.
//   2. The cited row is separately PROVEN by tests/conformance.test.mjs against the running real stack —
//      and that sweep also fails if the row has gone stale. So what is skipped here is not "assumed
//      impossible", it is "measured impossible, and re-measured on every sweep".
//   3. It prints the id, the subject and the owner to stdout, so reading the run output for SKIP (which is
//      what anyone auditing this suite must do) yields the reason, not just a count.
//
// It is deliberately one-directional: nothing skips on the FAKE profile. The fake layer runs everything.
export function skipOnReal(divergenceId: string): void {
  const row = DIVERGENCE_BY_ID.get(divergenceId);
  if (row === undefined) {
    throw new Error(
      `skipOnReal("${divergenceId}") cites a divergence that is not in tests/divergences.mjs. A spec may only ` +
        "decline the real profile for a difference the conformance sweep has measured and recorded.",
    );
  }
  if (!IS_REAL) return;
  // eslint-disable-next-line no-console -- the loudness IS the feature; a silent skip is the trap.
  console.log(`SKIPPED ON REAL PROFILE — ${row.id} [${row.kind}] ${row.subject}\n  owner: ${row.owner}`);
  test.skip(true, `${row.id}: ${row.subject} — ${row.detail}`);
}

// browserServedAssets scans .next/static — every file there is browser-fetchable (/_next/static/...), so a
// walk of it is a real browser-surface scan of both the minified chunks (*.js) and their source maps
// (*.js.map), which exist because next.config.ts sets productionBrowserSourceMaps.
//
// IT IS HERE, NOT IN A SPEC, BECAUSE TWO SWEEPS NEED THE SAME WALK (E25 T4): public-api-only.spec.ts scans
// these bytes for the API key, secret-never-returns.spec.ts scans them for a written environment value. A
// second copy would be a second thing to keep correct, and the correctness that matters is subtle — the walk
// is RECURSIVE and it enumerates FILES rather than a list of names, because a guard that names the asset
// directories it expects is a guard a new build layout defeats silently.
export function browserServedAssets(): { path: string; body: string }[] {
  const root = resolve(process.cwd(), ".next", "static");
  const out: { path: string; body: string }[] = [];
  for (const entry of readdirSync(root, { recursive: true, withFileTypes: true })) {
    if (!entry.isFile()) continue;
    const full = resolve(entry.parentPath ?? root, entry.name);
    out.push({ path: full, body: readFileSync(full, "utf8") });
  }
  return out;
}

// announceProfile prints which upstream a spec file actually ran against. "which profile ran" is a question
// the reports have had to answer by inference until now.
export function announceProfile(file: string): void {
  // eslint-disable-next-line no-console -- see above.
  console.log(`PROFILE=${PROFILE} — ${file}`);
}

// signIn opens the console's door for a browser context, and every spec that reads data needs it now — the
// relay answers 401 without a session (E25 T1). It goes through the REAL login route with the REAL password,
// so no spec is handed a forged cookie: if the door stopped working, the whole suite would fail rather than
// quietly testing an open console.
//
// `page.request` shares the browser context's cookie jar, so the Set-Cookie lands where the page will send
// it. Origin is set explicitly because an APIRequestContext does not stamp one, and the login route refuses
// a mutation without it — the same refusal a foreign page would get.
export async function signIn(page: Page): Promise<void> {
  const origin = `http://127.0.0.1:${NEXT_PORT}`;
  const res = await page.request.post(`${origin}/api/console/login`, {
    data: { password: CONSOLE_PASSWORD },
    headers: { Origin: origin, "Content-Type": "application/json" },
  });
  // Not an `expect`: a failure here is a broken precondition for everything after it, and it must read as
  // that rather than as an assertion inside whichever test happened to be first.
  if (res.status() !== 204) {
    throw new Error(`sign-in failed with ${res.status()} — the console's door is not working, so nothing after this proves anything`);
  }
}

// sessionHeaders returns the session as an explicit Cookie header, for the API-LEVEL requests some specs
// make (`page.request`, `request`) rather than browser navigations.
//
// MEASURED, not guessed: Playwright's APIRequestContext does NOT apply the browser's "localhost and
// 127.0.0.1 are potentially-trustworthy" exception to a `Secure` cookie, so over loopback HTTP it withholds
// the session and every such request comes back 401 — while `page.goto` in the same context sends it
// happily, because Chromium does apply the exception. `context.cookies(url)` filters the same way, which is
// why the lookup below passes no url. The cookie is read from the jar the real login route filled; nothing
// here is forged.
export async function sessionHeaders(page: Page): Promise<Record<string, string>> {
  const cookie = (await page.context().cookies()).find((c) => c.name === "palai_console_session");
  if (cookie === undefined) throw new Error("no session cookie in the browser context — signIn(page) must run first");
  return { Cookie: `${cookie.name}=${cookie.value}` };
}

// signInViaForm signs in through the PAGE — the real form, a real browser fetch — so the login request is
// visible to page.on("request"). That is required by exactly one spec and is the reason there are two
// helpers: public-api-only.spec.ts has to SEE the login request to assert that it is the single non-relay
// exception and that it carries no Bearer. A page.request post is invisible to that intercept, so using the
// fast helper there would have made the exception unobservable and the assertion vacuous.
export async function signInViaForm(page: Page): Promise<void> {
  await page.goto("/login");
  await page.getByTestId("password-input").fill(CONSOLE_PASSWORD);
  await page.getByTestId("login-button").click();
  // The form navigates to / on success. A refusal would leave the error region on screen instead.
  await expect(page.getByTestId("ceiling-note")).toBeVisible({ timeout: 15_000 });
}

// concreteDynamicPath turns a DYNAMIC route pattern into a path this run can actually visit, and it REFUSES
// to be skippable (E29).
//
// A screen keyed by a resource id — a session transcript — cannot be a row in CONSOLE_ROUTES, whose whole
// contract is "a path the generated axe loop can goto". The reading taken in E25 T8 was to avoid dynamic
// routes entirely; a transcript is the case where that stops working, so the route declares HOW to make its
// path concrete instead: read the first row of its collection through the relay, and CREATE one when the
// collection is empty.
//
// The create arm is the whole point. Without it, a deployment holding no sessions would produce a skipped
// scan, which is this repo's most-repeated failure — a suite reporting green for the exact condition it
// exists to detect. With it, "there are no sessions" is not a state in which the transcript screen goes
// unscanned; it is a state in which the scan makes one.
//
// Every request goes through the CONSOLE'S OWN RELAY (/api/palai/v1/*) with the browser's session cookie, so
// the helper exercises the same public-API-only path the browser does and never holds a credential.
export async function concreteDynamicPath(page: Page, route: DynamicConsoleRoute): Promise<string> {
  const origin = `http://127.0.0.1:${NEXT_PORT}`;
  const headers = await sessionHeaders(page);
  const list = await page.request.get(`${origin}/api/palai/v1${route.sampleFrom}`, { headers });
  if (list.status() !== 200) {
    throw new Error(`GET ${route.sampleFrom} answered ${list.status()} — ${route.label} cannot be resolved, so nothing after this proves anything`);
  }
  // THE FIRST ROW THE SCREEN CAN ACTUALLY SHOW, which is not always the first row (Task 12). A dynamic route
  // renders different controls for different rows, and a sampler that always took `data[0]` would resolve a
  // path whose declared dialog does not exist — on /bots/[id], a bot whose `kind` this console has no form
  // for renders no credential section at all, and the axe and contrast dialog loops would then fail with a
  // locator timeout that says nothing about accessibility. `pick` lets a route say which rows it can drive;
  // a route without one keeps taking the first, and a route whose predicate matches nothing CREATES, which
  // is the same refusal-to-skip the empty-collection arm already carries.
  const body = (await list.json()) as { data?: Record<string, unknown>[] };
  const usable = (body.data ?? []).find((row) => (route.pick === undefined ? true : route.pick(row)));
  const first = usable?.id;
  if (typeof first === "string" && first !== "") return route.build(first);

  const created = await page.request.post(`${origin}/api/palai/v1${route.sampleFrom}`, {
    headers: { ...headers, Origin: origin, "Content-Type": "application/json" },
    data: route.create,
  });
  if (created.status() !== 201 && created.status() !== 200) {
    throw new Error(
      `${route.sampleFrom} holds no row and POST answered ${created.status()}, so ${route.label} has no scannable path. ` +
        "This is a FAILURE and not a skip on purpose: an unscanned page must be a red test.",
    );
  }
  const row = (await created.json()) as { id?: string };
  if (typeof row.id !== "string" || row.id === "") throw new Error(`POST ${route.sampleFrom} returned no id`);
  return route.build(row.id);
}

// runToTerminal starts a run on /runs and drives it to a terminal status on EITHER profile.
//
// The approval leg is the one genuinely profile-dependent step, and it is asserted in BOTH directions
// rather than tolerated in either: on the fake profile the approval panel MUST appear (a regression that
// removed it would fail here, not quietly proceed), and on the real profile it must NOT — because
// DIV-UI-001 measured that a compose run cannot reach approval.requested.v1, and if one ever did, that row
// is stale and the conformance sweep says so first. So this is not a branch that hides a difference; it is
// a branch whose two arms are each pinned.
export async function runToTerminal(page: Page): Promise<void> {
  await page.goto("/runs");
  await page.getByTestId("run-button").click();
  if (!IS_REAL) {
    await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
    await page.getByTestId("approve-button").click();
  } else {
    await expect(page.getByTestId("approval-panel")).toHaveCount(0);
  }
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 60_000 });
}

/**
 * resetFakeFixture restores the fake control plane's mutable collections to the fixture as authored.
 *
 * IT EXISTS BECAUSE THE FIXTURE IS ONE PROCESS AND THE SUITE RUNS TWO PROJECTS OVER IT. `playwright.config`
 * declares chromium-fake and chromium-fake-dark, both pointed at a single `node tests/fake-control-plane.mjs`,
 * and a spec that writes — PATCH a session's name, publish a tool revision — leaves that write in place for
 * whichever project runs second. Measured 2026-08-01: sessions.spec.ts failed 16 assertions in the dark
 * project and passed 22/22 when the dark project ran alone. mcp-tools.spec.ts had already shown the same
 * shape three times over `trev_console_0001`.
 *
 * A file that MUTATES shared fixture state calls this in a `beforeAll`. A file that only reads does not need
 * it. On the real profile it is a no-op: there is no fake to reset, and a compose stack's state is the point.
 */
export async function resetFakeFixture(): Promise<void> {
  if (PROFILE === "real") return;
  const res = await fetch(`${UPSTREAM}/__reset`, { method: "POST" });
  if (!res.ok) throw new Error(`the fake fixture refused to reset (${res.status}) — a spec that believes it is isolated and is not will fail somewhere else entirely`);
}

/**
 * openDeclaredDialog opens one FORM_DIALOGS / PRIMITIVE_DIALOGS row the way an operator does, and it is
 * SHARED because two sweeps open the same list (tests/a11y.spec.ts scans each with axe, tests/contrast.spec.ts
 * measures the controls inside it). Playwright refuses a spec-to-spec import, which is why the rows live in
 * tests/constants.ts; this is the other half of that split, and it lives here rather than there because
 * constants.ts may not import `@playwright/test` (it is loaded by playwright.config.ts at config time).
 *
 * TWO SHAPES OF OPENER, and the second is the reason this function exists. A panel-head control (`+ Create
 * pool`) is in the document as soon as the route settles, so `getByTestId(open)` finds it. A ROW action is
 * not: components/ui/Menu.tsx portals its popup to <body> on click, so the item does not exist until the
 * row's `⋯` is opened — and both testids are prefixes there, because a row's controls are keyed by an id the
 * fake seeds and a compose stack mints fresh. `rowMenu` selects that path and the FIRST row offering the
 * control is used, which is exactly what tests/fleet.spec.ts's firstMachineWith does and why it runs on both
 * profiles.
 *
 * IT SETTLES THE DIALOG BEFORE RETURNING. Both callers measure what is INSIDE — axe judges the markup, the
 * contrast sweep judges every control's boundary — and a dialog whose fields arrive from a fetch is a dialog
 * that answers to its name before it has anything to judge. That is the reveal-once shape this suite was
 * caught by: the container is visible, the scan finds nothing, and the number looks fine.
 */
export async function openDeclaredDialog(
  page: Page,
  d: { open: string; dialog: string; rowMenu?: string },
): Promise<void> {
  if (d.rowMenu === undefined) {
    await expect(page.getByTestId(d.open)).toBeVisible({ timeout: 15_000 });
    await page.getByTestId(d.open).click();
  } else {
    const toggles = page.locator(`[data-testid^="${d.rowMenu}"]`);
    await expect(toggles.first(), `no row on this page carries a "${d.rowMenu}…" menu, so ${d.dialog} cannot be opened`).toBeVisible({
      timeout: 15_000,
    });
    const count = await toggles.count();
    let opened = false;
    for (let i = 0; i < count && !opened; i++) {
      await toggles.nth(i).click();
      // The popup is portalled and positioned a frame later, so `count()` — which does not auto-wait —
      // reads zero on an immediate probe. tests/fleet.spec.ts measured exactly that, on all seven rows.
      await expect(page.getByRole("menu"), "the row menu did not open").toBeVisible();
      const item = page.locator(`[data-testid^="${d.open}"]`);
      if ((await item.count()) > 0) {
        await item.first().click();
        opened = true;
      } else {
        await page.keyboard.press("Escape");
        await expect(page.getByRole("menu")).toHaveCount(0);
      }
    }
    expect(opened, `${String(count)} row menu(s) were opened and none offered a "${d.open}…" item`).toBe(true);
  }
  await expect(page.getByTestId(d.dialog)).toBeVisible();
  await page.waitForLoadState("networkidle");
}

// --- DRIVING components/ui/Select (E29 component layer) --------------------------------------------------
//
// The seven native `<select>`s became a Base UI listbox, and Playwright's `selectOption` only speaks to a
// native `<select>` ("Error: Element is not a <select> element"). These three are the replacements, and they
// are here rather than copied into five spec files for the reason Picker.tsx already gives about the rule it
// holds: the fourth copy is the one that gets it wrong.
//
// THEY DRIVE THE CONTROL THE WAY AN OPERATOR DOES — click the trigger, wait for the popup, click a row —
// rather than by setting state. That is strictly more than `selectOption` proved: `selectOption` on a native
// control dispatches a change event without ever opening anything, so a dropdown that failed to open, or
// opened empty, or rendered its rows unclickable, passed. These fail.

/** openListbox clicks a ui/Select trigger and waits for the listbox its popup carries. */
async function openListbox(page: Page, testId: string) {
  const trigger = page.getByTestId(testId);
  // 15s, THE SAME BUDGET EVERY PANEL WAIT IN THIS SUITE USES, and it is not padding. components/Panel.tsx
  // returns a head with NO TOOLBAR while `rows === null` — which is every refetch, including the one a
  // server-side filter triggers — so a control chosen here can unmount and remount between two clicks. The
  // native `selectOption` absorbed that inside Playwright's own auto-wait; this has to say it. Measured: the
  // dark project failed at the 5s default on a machine also running a second worktree's suite.
  await expect(trigger, `no select carries data-testid="${testId}"`).toBeVisible({ timeout: 15_000 });
  await trigger.click();
  const listbox = page.getByRole("listbox");
  await expect(listbox, `the ${testId} listbox did not open`).toBeVisible();
  return listbox;
}

/**
 * chooseOption is `selectOption(value)` for a ui/Select — the value, not the label.
 *
 * Addressing by value rather than by the row's words is deliberate and is what keeps these tests about
 * BEHAVIOUR: the empty choice has no value to name at all in label terms ("Any status", "All event types",
 * "Order as served" are three different words for it), and a filter's copy is exactly the thing a design
 * pass changes. components/ui/Select.tsx puts the value on every row for this.
 */
export async function chooseOption(page: Page, testId: string, value: string): Promise<void> {
  const listbox = await openListbox(page, testId);
  const option = listbox.locator(`[role="option"][data-value="${value}"]`);
  await expect(option, `the ${testId} listbox offers no option with value "${value}"`).toHaveCount(1);
  await option.click();
  await expect(page.getByTestId(testId), `choosing "${value}" in ${testId} did not take`).toHaveAttribute("data-value", value);
}

/**
 * chooseOptionByLabel finds the one row whose text carries `label` and selects it, returning its value.
 *
 * Two callers need this shape and both said so before the listbox existed: mcp-tools' tool picker labels a
 * row `<canonical> (<id>)` over a fixture-minted id, and config-journey picks the environment it just
 * created BY NAME — "the label is what an operator reads, so a picker that offered the right id under the
 * wrong name would fail here; going straight to the id would not have noticed".
 */
export async function chooseOptionByLabel(page: Page, testId: string, label: string): Promise<string> {
  const listbox = await openListbox(page, testId);
  const option = listbox.locator('[role="option"]').filter({ hasText: label });
  await expect(option, `no row in ${testId} carries "${label}"`).toHaveCount(1);
  const value = await option.getAttribute("data-value");
  expect(value, `the ${testId} row for "${label}" carries no value`).not.toBeNull();
  await option.click();
  return String(value);
}

/** chosenValue is `inputValue()` for a ui/Select: what the control currently holds, not what it displays. */
export async function chosenValue(page: Page, testId: string): Promise<string> {
  const value = await page.getByTestId(testId).getAttribute("data-value");
  expect(value, `${testId} is not a ui/Select — it carries no data-value`).not.toBeNull();
  return String(value);
}
