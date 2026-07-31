import { expect, test, type Page } from "@playwright/test";

import { NEXT_PORT } from "./constants";
import { announceProfile, sessionHeaders, signIn, skipOnReal } from "./profile";

// THE FLEET SCREEN, AND THE STATE E24 SHIPPED A DECISION FOR THAT NOTHING COULD SHOW (E28 T3, plan §T3).
//
// THE RED THIS FILE WAS WRITTEN AGAINST: E28 T1 opened a pool's birth path, so a machine can now enrol into a
// STRICT pool and sit `pending` waiting for a human — and `page.goto("/fleet")` answered 404. E24 T6's
// approve route was correct, correctly gated on `approve` rather than `provision`, and reachable from no
// screen at all; before T1 it decided a state no operator could produce, and after T1 it decided a state no
// operator could SEE.
//
// FOUR SECTIONS AND THREE THINGS THE PAGE REFUSES TO INVENT. The pools (with `waiting` kept as the `*int64`
// it is), the pool keys (minted once, revoked with the machines the revocation does NOT stop), the machines
// (with `last_seen_at`'s real meaning and NO health badge, because there is nothing behind one), and the
// waiting room — which is HERE and not on /approvals, for the server's own reason: a machine enrolment has no
// request_hash to bind to.
//
// WHAT RUNS WHERE, AND THIS PARAGRAPH WAS WRONG FIRST. It said everything needing a MACHINE runs on the
// fixture, reasoning that a compose stack enrols none. It enrols ONE: deploy/compose starts a `runner`
// service and `palai local up` mints it a token, so the real profile has `runner-local`, active, in
// `pool_default` — and the conformance sweep now compares its row. Four things genuinely cannot exist there
// and only those four skip, citing DIV-UI-009: a SECOND machine (compose runs one), a PENDING one (the only
// pool is not strict when the runner enrols, and taking a pool strict does not re-ask about what is already
// in it), the two 403 admissions (one key, every capability, an unconfigured and therefore permissive
// approver list), and a NON-ZERO live count. Everything else runs on both profiles — including the machine
// table with a real row, the cordon confirmation, and the revoke dialog's single-read network assertion.
test.beforeAll(() => announceProfile("fleet.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/**
 * api is a relay call with the browser context's session, for the SETUP and READ-BACK steps.
 *
 * `Origin` is stamped for the reason tests/policy.spec.ts stamps it: lib/session.ts refuses a mutation whose
 * Origin does not match the Host (403 `origin_mismatch`), and a Playwright APIRequestContext sends none.
 */
async function api(page: Page, method: "get" | "post" | "patch", path: string, data?: unknown) {
  const origin = `http://127.0.0.1:${NEXT_PORT}`;
  const headers = { ...(await sessionHeaders(page)), Origin: origin };
  return page.request[method](`${origin}/api/palai/v1${path}`, { headers, ...(data === undefined ? {} : { data }) });
}

/** open loads the fleet screen and waits for its declared readiness signal. */
async function open(page: Page) {
  await page.goto("/fleet");
  await expect(page.getByTestId("panel-runner-pools")).toBeVisible({ timeout: 15_000 });
}

/**
 * firstMachineWith returns the id of the first machine in the TABLE offering `action`.
 *
 * SELECTED FROM THE RENDERED LIST RATHER THAN PINNED, and that is what lets the legs below run on the real
 * profile: a compose stack enrols exactly ONE machine (`runner-local`, active, in `pool_default`) and its id
 * is a fresh hash on every stack. Scoped to the machines panel because `runner-revoke-` is also the prefix of
 * the dialog's own testids, and a locator that matched those would depend on whether one was open.
 */
async function firstMachineWith(page: Page, action: "cordon" | "revoke"): Promise<string> {
  // THE ROW ACTIONS MOVED INTO A ROW-END ⋯ MENU (page-parity pass), so a row's control does not EXIST until
  // its menu is open. The menus are walked in order and the first one offering `action` is the answer, which
  // is the same claim this helper always made — a machine on THIS stack that offers this control — and the
  // menu is LEFT OPEN, so the caller's own click finds the control where the helper found it.
  //
  // Opening a menu makes no request, which the single-read leg below depends on: it asserts that nothing has
  // read a machine singly before the dialog asked.
  const toggles = page.getByTestId("panel-runners").locator('[data-testid^="runner-menu-"]');
  await expect(toggles.first(), "the machines panel rendered no rows at all").toBeVisible({ timeout: 15_000 });
  const count = await toggles.count();
  for (let i = 0; i < count; i++) {
    const toggle = toggles.nth(i);
    const id = (await toggle.getAttribute("data-testid"))?.replace("runner-menu-", "") ?? "";
    await toggle.click();
    if ((await page.getByTestId(`runner-${action}-${id}`).count()) > 0) return id;
    await toggle.click();
  }
  throw new Error(`no machine on this stack offers ${action} — ${String(count)} row menu(s) were opened and none carried it`);
}

/** openRowMenu opens one row's ⋯ so the control inside it can be clicked. */
async function openRowMenu(page: Page, testId: string): Promise<void> {
  await page.getByTestId(testId).click();
}

/** singleReadWatcher records every GET /v1/runners/{id} the BROWSER makes — the network assertion below. */
function singleReadWatcher(page: Page): string[] {
  const seen: string[] = [];
  page.on("request", (req) => {
    if (req.method() !== "GET") return;
    const id = new URL(req.url()).pathname.match(/^\/api\/palai\/v1\/runners\/([^/]+)$/)?.[1];
    if (id !== undefined) seen.push(decodeURIComponent(id));
  });
  return seen;
}

// --- THE POOLS, AND A COUNT THAT MUST NOT BE FLATTENED ---------------------------------------------------

test("a pool is created from the console with the posture it was given", async ({ page }) => {
  // E28 T1's route, driven from a screen. It runs on BOTH profiles: POST /v1/runner-pools is a real route on
  // a real control plane, and this is the first thing on this page that could not be done at all before T1.
  const name = `t3-fleet-${Date.now()}`;
  await open(page);

  // THE CREATE FORM IS BEHIND THE PANEL'S `+ Create pool` BUTTON (page-parity-govern pass). A driver change:
  // every assertion below is the one it was, made against the same form.
  await page.getByTestId("pool-create-open").click();
  await expect(page.getByTestId("pool-create-dialog")).toBeVisible();
  await page.getByTestId("pool-name-input").fill(name);
  await page.getByTestId("pool-posture-select").selectOption("unsandboxed-host");
  await page.getByTestId("pool-os-input").fill("darwin");
  await page.getByTestId("pool-arch-input").fill("arm64");
  await page.getByTestId("pool-strict-input").check();
  await page.getByTestId("pool-create-button").click();

  await expect(page.getByTestId("pool-create-status")).toContainText(name, { timeout: 15_000 });
  // Read back over the PUBLIC API rather than off the row the form just rendered: a create that answered a
  // shape the list does not repeat is exactly what T1's one-projection rule exists to prevent.
  const listed = (await (await api(page, "get", "/runner-pools")).json()) as { data?: { name?: string; posture?: string; strict_enrollment?: boolean }[] };
  const row = (listed.data ?? []).find((p) => p.name === name);
  expect(row, "the pool the console just created is not in GET /v1/runner-pools").toBeDefined();
  expect(row?.posture).toBe("unsandboxed-host");
  expect(row?.strict_enrollment, "the waiting room was requested at creation and the pool did not get it").toBe(true);
  await expect(page.getByTestId("panel-runner-pools")).toContainText(name);
  // The E28 exit gate's pool row (plan §T4 group (a)). It prints the POSTURE and the SURFACE rather than a
  // count, because a count alone is satisfied by the pool every tenant is seeded at birth — and that seed
  // wrote 'sandboxed-linux' as a LITERAL, which is the hole this epic exists to have closed.
  console.log(`POOL CREATED — pool=${name} posture=${String(row?.posture)} strict=${String(row?.strict_enrollment)} via=console`);
});

test("a pool's queue depth is rendered as the number it is, and zero is one of the numbers", async ({ page }) => {
  // `RunnerPoolItem.Waiting` answers the one question an operator of a fleet actually asks — why is nothing
  // running in my Mac pool — and zero is the answer that means the pool is keeping up.
  //
  // `pool_default` is the id BOTH stacks carry: the fixture seeds it and `InsertDefaultRunnerPool` writes it
  // on every real bootstrap, so this runs on both profiles rather than being a fixture assertion.
  await open(page);
  const zero = page.getByTestId("pool-waiting-pool_default");
  await expect(zero).toHaveAttribute("data-waiting-known", "true");
  await expect(zero).toHaveText(/^0\b/);
});

test("a pool with runs queued renders the count the gateway gave it", async ({ page }) => {
  // The other half, and it needs a pool with attempts parked for want of a free machine — which on a real
  // stack means holding a run open across the read. Zero alone would pass over a console that rendered a
  // constant, so this is the leg that makes the number the SERVER's.
  skipOnReal("DIV-UI-009");
  await open(page);
  const busy = page.getByTestId("pool-waiting-pool_mac");
  await expect(busy).toHaveAttribute("data-waiting-known", "true");
  await expect(busy).toHaveText(/^2\b/);
});

test("a deployment whose gateway could not be asked renders no queue depth rather than a zero", async ({ page }) => {
  // THE POINTER, AND THE HALF THAT COSTS SOMETHING IF IT IS FLATTENED. api/runners.go's runnerPoolView writes
  // `waiting` only when the pointer is set, and internal/fleet/api.go sets it in a loop guarded by
  // `a.live == nil` — so a stack that bound no runner listener answers with the key ABSENT from every row.
  // "Nobody could ask the gateway" and "nothing is waiting" are not the same answer, and a console that
  // prints 0 for both tells an operator their empty Mac pool is healthy.
  //
  // THE STATE IS REACHED BY REWRITING THE RESPONSE AT THE BROWSER BOUNDARY, not by a fixture row, and that is
  // a fidelity decision: the live gateway is a DEPLOYMENT-level fact, so one pool carrying the key while its
  // sibling does not is a wire neither stack can produce. These are the same bytes a listener-less stack
  // sends, and this leg therefore runs on BOTH profiles.
  await page.route("**/api/palai/v1/runner-pools**", async (route) => {
    const url = new URL(route.request().url());
    if (route.request().method() !== "GET" || url.pathname !== "/api/palai/v1/runner-pools") return route.fallback();
    const upstream = await route.fetch();
    const body = (await upstream.json()) as { data?: Record<string, unknown>[] };
    body.data = (body.data ?? []).map(({ waiting: _dropped, ...rest }) => rest);
    await route.fulfill({ response: upstream, json: body });
  });

  await open(page);
  const cells = page.locator('[data-testid^="pool-waiting-"]');
  // WAIT FOR A ROW, NOT FOR THE PANEL. `panel-runner-pools` is visible while it still says "Loading…", and
  // the intercept adds a round trip — so counting straight after `open` measured zero on the real profile and
  // the loop below then asserted nothing at all. The count assertion under it is what caught that.
  await expect(cells.first(), "the pool panel rendered no rows").toBeVisible({ timeout: 15_000 });
  const count = await cells.count();
  expect(count, "no pool rows rendered, so this assertion would be about nothing").toBeGreaterThan(0);
  for (let i = 0; i < count; i++) {
    const cell = cells.nth(i);
    await expect(cell).toHaveAttribute("data-waiting-known", "false");
    await expect(cell, "a pool the gateway could not be asked about rendered a digit").not.toHaveText(/\d/);
    await expect(cell).toContainText(/could not be asked|not be asked/i);
  }
});

test("an existing pool's waiting room is switched on from the console", async ({ page }) => {
  // PATCH /v1/runner-pools/{pool_id} is E28 T1's SECOND route and taking a pool strict is an OPERATIONAL act
  // rather than a creation parameter — `FLT-P12` says the waiting room holds nothing "unless an operator
  // asks", and before T1 the operator could not ask. A route with no caller is the class of hole this
  // repository keeps finding, so the screen calls it.
  const name = `t3-strict-${Date.now()}`;
  const created = await api(page, "post", "/runner-pools", { name, posture: "sandboxed-linux", os: "", arch: "", strict_enrollment: false });
  expect(created.status(), "the subject pool could not be created").toBeLessThan(300);
  const id = ((await created.json()) as { id?: string }).id ?? "";
  expect(id).not.toBe("");

  await open(page);
  await openRowMenu(page, `pool-menu-${id}`);
  await page.getByTestId(`pool-strict-${id}`).click();
  await expect(page.getByTestId("pool-strict-status")).toContainText(id, { timeout: 15_000 });

  const after = (await (await api(page, "get", "/runner-pools")).json()) as { data?: { id?: string; strict_enrollment?: boolean }[] };
  expect((after.data ?? []).find((p) => p.id === id)?.strict_enrollment, "the pool's waiting room was not opened").toBe(true);
});

// --- THE KEYS: SHOWN ONCE, AND REVOKED WITHOUT STOPPING WHAT THEY ADMITTED --------------------------------

test("a pool key is shown once, and nothing reads it back", async ({ page }) => {
  // The server's behaviour mirrored in the browser: poolKeyView(item, true) is used by the mint route and by
  // nothing else (api/runners.go:282 vs :298,:320). Runs on BOTH profiles — minting a pool key is a real
  // write against a real control plane.
  await open(page);
  const pool = await page.getByTestId("poolkey-pool-select").inputValue();
  expect(pool, "the key panel has no pool selected, so the mint below would be about nothing").not.toBe("");

  // Same move, same reason: the mint is behind the key panel's `+ Mint key`.
  await page.getByTestId("poolkey-mint-open").click();
  await expect(page.getByTestId("poolkey-mint-dialog")).toBeVisible();
  await page.getByTestId("poolkey-mint-button").click();
  const value = page.getByTestId("poolkey-reveal-value");
  await expect(value).toBeVisible({ timeout: 15_000 });
  const secret = await value.innerText();
  expect(secret.length, "the revealed pool key is empty — the reveal region rendered nothing").toBeGreaterThan(8);

  // NOT IN THE LISTING. GET /v1/runner-pools/{pool_id}/keys renders metadata; a `key` there would be a
  // credential in a list an operator leaves open.
  const listed = await (await api(page, "get", `/runner-pools/${encodeURIComponent(pool)}/keys`)).text();
  expect(listed.includes(secret), "the pool key's VALUE came back on the metadata listing").toBe(false);

  // AND NOT AFTER A RELOAD. The value lives in one DOM node for as long as the region is on screen.
  await page.reload();
  await expect(page.getByTestId("panel-runner-pools")).toBeVisible({ timeout: 15_000 });
  const document = await page.content();
  expect(document.includes(secret), "the pool key survived a reload — it is in the reloaded document").toBe(false);
});

test("revoking a pool key shows the machines it already admitted and does not stop", async ({ page }) => {
  // api/runners.go's revoke answers with `enrolled_runners_still_running`, NAMED for what it means. An
  // operator not shown them reads "revoked" as "removed" and believes one call decommissioned a fleet.
  skipOnReal("DIV-UI-009");
  await open(page);
  await page.getByTestId("poolkey-pool-select").selectOption("pool_mac");
  // SELECTED BY PREFIX, NOT BY A FIXED ID. A revoked key stays revoked, so the fixture re-seeds a fresh
  // revocable one with a new id — the approval queue's rule, and the reason is that this suite runs twice
  // (once per colour scheme) against one fixture process.
  const menu = page.locator('[data-testid^="poolkey-menu-rpk_seeded_"]').first();
  await expect(menu).toBeVisible({ timeout: 15_000 });
  await menu.click();
  const revocable = page.locator('[data-testid^="revoke-poolkey-rpk_seeded_"]').first();
  await expect(revocable).toBeVisible({ timeout: 15_000 });
  await revocable.click();

  const dialog = page.getByTestId("poolkey-revoke-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toHaveAttribute("role", "alertdialog");
  await page.getByTestId("poolkey-revoke-dialog-confirm").click();

  const status = page.getByTestId("poolkey-revoke-status");
  await expect(status).toBeVisible({ timeout: 15_000 });
  // THE COUNT, AND THE SENTENCE THAT MAKES IT MEAN SOMETHING.
  await expect(status).toContainText("2");
  await expect(status).toContainText(/still running|keep running/i);
  await expect(status).toContainText("run_active_02");
});

// --- THE MACHINES: WHAT last_seen_at MEANS, AND THE BADGE THAT DOES NOT EXIST -----------------------------

test("last_seen_at says what it means, and no machine carries a health badge", async ({ page }) => {
  // api/runners.go:26-29, verbatim: "last_seen_at records the last time the machine AUTHENTICATED (enrol,
  // connect, renew). Nothing polls and nothing expires a row, so a stale stamp means 'has not authenticated
  // since', NOT 'is down'. The item deliberately carries no `healthy` field, because there is nothing behind
  // one." A console that renders an up/down dot from that stamp is inventing a status the server does not
  // have — which is worse than an empty column, because an operator acts on it.
  await open(page);
  const note = page.getByTestId("runner-last-seen-note");
  await expect(note).toBeVisible();
  await expect(note).toContainText("has not authenticated since");
  await expect(note).toContainText(/not.{0,4}is down|NOT 'is down'|not "is down"/i);

  const body = await page.locator("main").innerText();
  expect(/\bhealthy\b/i.test(body), `the fleet page rendered the word "healthy"; there is no such field and nothing behind one:\n${body.slice(0, 400)}`).toBe(false);
  expect(/\b(online|offline)\b/i.test(body), "the fleet page derived an up/down status the API does not carry").toBe(false);
});

test("cordon goes through the native confirmation and revoke does not", async ({ page }) => {
  // WCAG 2.2 SC 3.3.4 is satisfied by ANY ONE of its three legs. A cordon satisfies leg 1 verbatim — resume
  // undoes it (execution/runner_gateway.go:324) — so window.confirm stays, keeping every property (keyboard
  // operation, screen-reader announcement, focus trap) a hand-rolled dialog has to re-earn. A revoke cannot
  // satisfy leg 1 and gets the component. Converting the reversible half is a RED TEST rather than a review
  // question, which is what this leg is for.
  let confirms = 0;
  page.on("dialog", (dialog) => {
    confirms += 1;
    void dialog.dismiss();
  });
  await open(page);
  const id = await firstMachineWith(page, "cordon");
  const before = (await (await api(page, `get`, `/runners/${id}`)).json()) as { state?: string };
  await page.getByTestId(`runner-cordon-${id}`).click();
  expect(confirms, "the reversible cordon no longer goes through window.confirm").toBe(1);
  await expect(page.getByTestId("runner-revoke-dialog")).toHaveCount(0);

  // Dismissed, so nothing moved — which is also the proof that the native dialog is still load-bearing.
  const after = (await (await api(page, "get", `/runners/${id}`)).json()) as { state?: string };
  expect(after.state, "a DISMISSED window.confirm cordoned the machine anyway").toBe(before.state);
});

test("the revoke dialog cannot open without the single read that carries the lease count", async ({ page }) => {
  // THE NETWORK ASSERTION, NOT THE TEXT. `ActiveLeases` is on the SINGLE read and not on the listing, and
  // api/runners.go:49-59 says why: it is a live value, and "a page of them would be a page of separate
  // instants presented as one". SC 3.3.4's Confirmed leg contains the word "reviewing", which is a DATA call
  // rather than a sentence — so a dialog that rendered a plausible number out of the list row would satisfy
  // every text assertion and review nothing. This asserts the request.
  const reads = singleReadWatcher(page);
  await open(page);
  const id = await firstMachineWith(page, "revoke");
  expect(reads, "the page read a machine singly before anything asked it to").toEqual([]);

  await page.getByTestId(`runner-revoke-${id}`).click();
  const dialog = page.getByTestId("runner-revoke-dialog");
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  expect(reads, "the revoke dialog opened without GET /v1/runners/{runner_id} — its lease count is therefore not a live read").toContain(id);

  await expect(dialog).toHaveAttribute("role", "alertdialog");
  await expect(dialog).toHaveAttribute("aria-modal", "true");
  const review = page.getByTestId("runner-revoke-dialog-review");
  await expect(review).toContainText(id);
  await expect(page.getByTestId("runner-revoke-leases"), "the lease count is the whole reason for the single read").toBeVisible();
  // THE SERVER'S OWN SENTENCE (execution/runner_gateway.go:328).
  await expect(page.getByTestId("runner-revoke-dialog-message")).toContainText(/decommissioned, not paused/i);
  // NOTHING WAS REVOKED — the dialog was opened and left. Asserted over the API rather than over the row,
  // because a row that failed to refresh looks identical to one that was never touched, and on the real
  // profile this is the stack's only machine.
  const after = (await (await api(page, "get", `/runners/${id}`)).json()) as { state?: string };
  expect(after.state, "opening the dialog revoked the machine").not.toBe("revoked");
});

test("the review names the machine's label and the count the gateway gave for it", async ({ page }) => {
  // The half that needs a machine actually SERVING something: zero alone would pass over a dialog rendering a
  // constant, and the number is the one an operator decides to unplug on.
  skipOnReal("DIV-UI-009");
  await open(page);
  await openRowMenu(page, "runner-menu-run_active_02");
  await page.getByTestId("runner-revoke-run_active_02").click();
  const review = page.getByTestId("runner-revoke-dialog-review");
  await expect(review).toBeVisible({ timeout: 15_000 });
  await expect(review, "the machine's label is half of 'which machine is this'").toContainText("mac-mini-01");
  await expect(page.getByTestId("runner-revoke-leases")).toContainText(/^2 lease/);
});

test("a machine whose lease count could not be read says so rather than showing a zero", async ({ page }) => {
  // THE SAME POINTER DISTINCTION AS THE POOL'S `waiting`, on the field it was copied FROM — and it is the one
  // that decides an action: api/runners.go:57-59 says zero "is the one that means the Mac can be unplugged".
  // A dialog that renders 0 when nothing was asked authorizes pulling the power on a machine still serving.
  //
  // Reached by rewriting the single read at the browser boundary, for the reason the pool leg above is: the
  // live gateway is deployment-wide (fleet/api.go `decorate` sets the pointer for every machine or for none),
  // so a per-machine absence is not a wire either stack can send.
  await page.route("**/api/palai/v1/runners/**", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const upstream = await route.fetch();
    const body = (await upstream.json()) as Record<string, unknown>;
    delete body.active_leases;
    await route.fulfill({ response: upstream, json: body });
  });

  await open(page);
  await page.getByTestId(`runner-revoke-${await firstMachineWith(page, "revoke")}`).click();
  const leases = page.getByTestId("runner-revoke-leases");
  await expect(leases).toBeVisible({ timeout: 15_000 });
  await expect(leases).toContainText(/could not be asked/i);
  await expect(leases, "a machine nobody could ask about was rendered as serving nothing").not.toHaveText(/\d/);
});

test("a machine serving nothing says zero, which is a different answer from 'nobody asked'", async ({ page }) => {
  // Runs on BOTH: an idle compose stack's single machine serves nothing, so its single read carries
  // `active_leases: 0` — the value that means the Mac can be unplugged.
  await open(page);
  await page.getByTestId(`runner-revoke-${await firstMachineWith(page, "revoke")}`).click();
  const leases = page.getByTestId("runner-revoke-leases");
  await expect(leases).toBeVisible({ timeout: 15_000 });
  await expect(leases).toContainText(/^0 lease/);
  await expect(leases).not.toContainText(/could not be asked/i);
});

// --- THE WAITING ROOM, AND THE QUEUE IT IS NOT IN --------------------------------------------------------

test("a machine waiting in a strict pool is on this page and is admitted from it", async ({ page }) => {
  // THE RED THIS WHOLE FILE WAS WRITTEN AGAINST. A machine enrols into a strict pool, sits `pending`, and
  // until this page existed nothing in the console showed it — /fleet answered 404.
  skipOnReal("DIV-UI-009");
  await open(page);
  const room = page.getByTestId("waiting-room");
  await expect(room).toBeVisible();
  await expect(room, "the waiting room must say which POOL a machine is asking to join").toContainText("pool_mac");

  // BY PREFIX, for the reason the key leg is: an admitted machine leaves the room for good, so the fixture
  // re-seeds a fresh one with a new id rather than un-admitting the old one.
  const admit = page.locator('[data-testid^="admit-run_waiting_"]').first();
  await expect(admit).toBeVisible({ timeout: 15_000 });
  const id = (await admit.getAttribute("data-testid"))?.replace("admit-", "") ?? "";
  expect(id).not.toBe("");
  await admit.click();
  await expect(page.getByTestId("admit-status")).toContainText(id, { timeout: 15_000 });

  // ADMITTED, read back over the public API rather than off the row that vanished.
  const after = (await (await api(page, "get", `/runners/${id}`)).json()) as { state?: string };
  expect(after.state, "the machine was not admitted").toBe("active");
  // IT LEFT THE ROOM. Asserted on the row's own control rather than on the section's text, because the
  // confirmation sentence names the machine that was just admitted and lives inside the section.
  await expect(page.getByTestId(`admit-${id}`)).toHaveCount(0);
  // The E28 exit gate's waiting-room row (plan §T4 group (b)). `from=console` is the field that makes this
  // E28's claim rather than E24's: the approve route shipped with a component test, and what had never
  // happened is an operator admitting a machine FROM A SCREEN.
  console.log(`WAITING ROOM — machine=${id} pool=pool_mac pending=true admitted from=console`);
});

test("a parked tool call is not on /fleet, and /fleet says which queue holds it", async ({ page }) => {
  // §3.6 D20, one direction. The two screens cover DISJOINT sets and each names the other's — a console that
  // let an operator believe one page holds every pending decision is the silent-truncation failure on the
  // surface where it costs most.
  skipOnReal("DIV-UI-006");
  await page.goto("/approvals");
  await expect(page.getByTestId("panel-approvals")).toBeVisible({ timeout: 15_000 });
  // THE CONDITION IS ASSERTED PRESENT, not assumed: a parked call has to exist or "the fleet page does not
  // show it" is a statement about an empty world. Its id is taken off the ROW rather than out of prose, so
  // this cannot pass by matching nothing.
  //
  // READ OFF THE TABLE AND THEN OPENED (page-parity pass). This used to take the id off the first
  // `tool-approval-facts-*` on the page, which existed only because /approvals rendered every parked call's
  // whole ledger AND its decision form inline — seven forms on one screen. The queue is a table now and the
  // facts belong to whichever call is selected, so the row is picked first and opened second. A DRIVER edit:
  // what this leg asserts — that the fleet page shows neither the id nor the hash, and says where the call
  // is — is untouched.
  const parkedID = (await page.getByTestId("approval-open").first().getAttribute("data-approval-id")) ?? "";
  expect(parkedID, "no gated tool call is parked, so this assertion would be true of an empty world").not.toBe("");
  await page.getByTestId("approval-open").first().click();
  await expect(page.getByTestId(`tool-approval-facts-${parkedID}`)).toBeVisible({ timeout: 15_000 });
  const hash = await page.getByTestId(`tool-approval-request-hash-${parkedID}`).innerText();
  expect(hash, "the parked call carries no request hash, which is the thing a machine enrolment does not have").not.toBe("");

  await open(page);
  const fleet = await page.locator("main").innerText();
  expect(fleet.includes(parkedID), `the fleet page rendered a parked tool call (${parkedID}) — the two queues are not disjoint`).toBe(false);
  expect(fleet.includes(hash), "the fleet page rendered a parked call's request hash").toBe(false);
  // AND IT SAYS SO, with a link to where the call actually is.
  const note = page.getByTestId("fleet-approvals-scope-note");
  await expect(note).toContainText(/tool call/i);
  await expect(note.locator('a[href="/approvals"]')).toHaveCount(1);
});

test("a pending machine is not on /approvals, and /approvals says which screen holds it", async ({ page }) => {
  // The converse, and the server's own reason is on the screen: api/runners.go:520-521, verbatim, "A MACHINE
  // ENROLMENT HAS NO REQUEST HASH TO BIND TO", while api/approvals.go's decision route requires one. The
  // separation is correct and the console does not collapse it.
  skipOnReal("DIV-UI-009");
  await open(page);
  await expect(page.getByTestId("waiting-room")).toContainText("run_pending_locked");

  await page.goto("/approvals");
  await expect(page.getByTestId("panel-approvals")).toBeVisible({ timeout: 15_000 });
  const queue = await page.locator("main").innerText();
  expect(queue.includes("run_pending_locked"), "a pending MACHINE appeared in the tool-approval queue").toBe(false);
  const note = page.getByTestId("approvals-scope-note");
  await expect(note).toContainText(/machine/i);
  await expect(note).toContainText(/request hash/i);
  await expect(note.locator('a[href="/fleet"]')).toHaveCount(1);
});

test("the three refusals of an admission get three sentences and none of them is a generic failure", async ({ page }) => {
  // api/runners.go's approveRunner distinguishes 403 `approver_not_authorized` (the project's approver list),
  // 403 `insufficient_scope` (the key's capability) and 404 (unknown, foreign, or not admissible — deliberately
  // indistinguishable). The two 403s have DIFFERENT FIXES and a console that flattens them sends an operator
  // to the wrong file.
  skipOnReal("DIV-UI-009");
  await open(page);

  const sentences: Record<string, string> = {};
  for (const id of ["run_pending_locked", "run_pending_noscope", "run_pending_gone"]) {
    await page.getByTestId(`admit-${id}`).click();
    const error = page.getByTestId(`admit-${id}-error`);
    await expect(error).toBeVisible({ timeout: 15_000 });
    sentences[id] = await error.innerText();
  }

  expect(sentences.run_pending_locked).toContain("approver list");
  await expect(page.getByTestId("admit-run_pending_locked-error").locator('a[href="/policy"]')).toHaveCount(1);
  expect(sentences.run_pending_noscope).toContain("approve");
  expect(sentences.run_pending_noscope, "the capability gate is a different fix from the approver list").toContain("capability");
  expect(sentences.run_pending_gone).toContain("waiting for approval");
  expect(sentences.run_pending_gone, "404 covers three causes the server refuses to tell apart, and the screen must not imply which").toContain("indistinguishable");

  const all = Object.values(sentences);
  expect(new Set(all).size, `three typed refusals produced ${String(new Set(all).size)} distinct sentence(s):\n${all.join("\n---\n")}`).toBe(3);
  for (const sentence of all) {
    expect(/something went wrong|unexpected error|an error occurred|try again later/i.test(sentence), `a typed refusal was flattened into a generic failure: ${sentence}`).toBe(false);
  }
});

// --- THE THREE CEILINGS, ON THE SCREEN --------------------------------------------------------------------

test("the page writes the three things a fleet screen must not let an operator assume", async ({ page }) => {
  // A CONSOLE SAYS WHAT IT DOES NOT SHOW (E25 T5's rule). Showing "3 active runners" and saying nothing about
  // where a tool actually executes is silent truncation on the fleet surface. All three sentences below are
  // MEASURED gap rows, and deleting one turns this red.
  await open(page);

  // FLT-P15 — the biggest, and known-gaps says to read it before any other FLT-P row. It is NOT collapsed,
  // for the reason /approvals leaves its scope note open: it changes what you conclude from every row on the
  // page, on every visit.
  const remote = page.getByTestId("fleet-remote-execution-note");
  await expect(remote).toBeVisible();
  await expect(remote).toContainText(/engine/i);
  await expect(remote).toContainText("xcodebuild");
  await expect(remote).toContainText("control plane");

  // FLT-P12 / FLT-P4 and FLT-P13 are standing facts about the deployment: true on every visit, unchanged by
  // anything on screen, read once. They are collapsed and KEYBOARD-REACHABLE, which is the property that
  // makes collapsing them honest rather than hiding them — a <details> is text in the document, and this
  // opens it the way an operator would.
  const strict = page.getByTestId("fleet-strict-note");
  await expect(strict).toBeAttached();
  await page.getByTestId("fleet-standing-notes").locator("summary").click();
  await expect(strict).toBeVisible();
  await expect(strict).toContainText(/pool key/i);
  await expect(strict).toContainText(/revok/i);
  await expect(page.getByTestId("fleet-approval-scope-note")).toContainText(/reboot/i);
});

test("the concurrency ceiling appears on the machine count and names the half it cannot check", async ({ page }) => {
  // `dispatchWorkerFleetWarning` (cmd/cli/internal/stack/up.go:1162-1180) prints this to a TERMINAL when
  // PALAI_DISPATCH_WORKERS=1 AND at least two machines are enrolled. The console can observe the SECOND half
  // and NOT the first: PALAI_DISPATCH_WORKERS is read by the control-plane process (main.go
  // dispatchWorkerCount) and appears on NO /v1 route, so this console cannot know it. The notice is therefore
  // shown on the machine count alone and says which half it could not check — a screen that implied it had
  // read the worker count would be claiming a measurement it did not take.
  skipOnReal("DIV-UI-009");
  await open(page);
  const note = page.getByTestId("fleet-concurrency-note");
  await expect(note).toBeVisible();
  await expect(note).toContainText("PALAI_DISPATCH_WORKERS");
  await expect(note).toContainText(/bounded by the control plane, not the fleet/i);
  await expect(note, "the console cannot read the worker count and must not imply that it did").toContainText(/cannot read|no .{0,12}route/i);
});
