import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type Request as NetRequest } from "@playwright/test";

import { NEXT_PORT, WCAG_TAGS } from "./constants";
import { announceProfile, chooseOption, sessionHeaders, signIn } from "./profile";

// THE POLICY DOCUMENT IS WRITTEN WHOLE, OR THE APPROVAL GATE OPENS (E28 T2, plan §3.6 D9).
//
// This is the epic's crown proof and it is a SECURITY proof, not an ergonomics one. The chain is four
// measured links and every one of them is in the tree:
//
//   1. PATCH /v1/projects/{id} is an ASSIGNMENT. identity/store.go:269-275 marshals the decoded
//      configPolicyInput and hands it to UpdateProjectConfigPolicy. There is no merge and no patch.
//   2. configPolicyInput's four list fields are nil-able Go slices, so a request naming only `pool` stores
//      `approvers: null` — the list is not left alone, it is ERASED.
//   3. `HIL-P11` measured that an unconfigured approver list is PERMISSIVE: an empty list means every
//      principal in the project may approve.
//   4. Therefore the most innocent-looking button on an admin console — "put this project on the Mac pool" —
//      silently opens the approval gate to everyone.
//
// The tree already knew (3) in a COMMENT: cmd/cli/internal/admin/admin.go:241-244 says "the realistic
// accident is setting --pool and silently dropping --approvers", and docs/operations/runner-fleet.md:70 tells
// an operator the same thing. A comment is not a control. This is the control.
//
// IT RUNS ON BOTH COLOUR-SCHEME PROJECTS and on both upstream profiles, and nothing here is fixture-shaped:
// the fixture's PATCH reproduces the real marshal step field for field (tests/fake-control-plane.mjs
// assignPolicy), so a form that passes here passes against a real control plane for the same reason.
test.beforeAll(() => announceProfile("policy.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// THE SUBJECT IS SEEDED, NOT NAMED, and that is a profile portability fix rather than tidiness. The first
// draft of this file wrote `proj_local` — which is the FIXTURE's project id and, per DIV-SHP-002, off by a
// letter from the `prj_local` identity/store.go actually seeds. On the real profile every assertion here
// would have read a 404 and this spec would have failed for a reason that has nothing to do with what it
// proves. Each run creates its OWN project and gives it the policy the crown test needs, so the precondition
// is a step rather than an assumption and no shared row's approver list is mutated.
const APPROVER = "prin_release_captain";
const SEEDED = { allowed_models: ["fake"], allowed_tools: ["git.push"], default_tools: ["git.push"], approvers: [APPROVER] };

/**
 * api is a relay call with the browser context's session, for the SETUP and READ-BACK steps.
 *
 * `Origin` is stamped explicitly and that is not boilerplate: lib/session.ts refuses a mutation whose Origin
 * does not match the Host (403 `origin_mismatch`, the CSRF second layer), and a Playwright APIRequestContext
 * sends none — which is the same reason tests/profile.ts's signIn sets one. Leaving it off makes every write
 * here a 403 that reads like a permissions problem.
 */
async function api(page: Page, method: "get" | "post" | "patch", path: string, data?: unknown) {
  const origin = `http://127.0.0.1:${NEXT_PORT}`;
  const headers = { ...(await sessionHeaders(page)), Origin: origin };
  return page.request[method](`${origin}/api/palai/v1${path}`, { headers, ...(data === undefined ? {} : { data }) });
}

/** seedProject creates a project and assigns it the policy this file's assertions are about. */
async function seedProject(page: Page): Promise<string> {
  const created = await api(page, "post", "/projects", { display_name: `t2-policy-${Date.now()}` });
  expect(created.status(), "the sweep subject could not be created").toBeLessThan(300);
  const id = ((await created.json()) as { id?: string }).id ?? "";
  expect(id, "POST /v1/projects returned no id").not.toBe("");
  const patched = await api(page, "patch", `/projects/${encodeURIComponent(id)}`, { config_policy: SEEDED });
  expect(patched.status(), "the subject's policy could not be seeded").toBe(200);
  return id;
}

/** readPolicy reads the stored document back THROUGH THE RELAY, i.e. over the same public API the form uses. */
async function readPolicy(page: Page, projectID: string): Promise<Record<string, unknown>> {
  const res = await api(page, "get", `/projects/${encodeURIComponent(projectID)}`);
  expect(res.status(), "the project could not be read back — the assertion below would be about nothing").toBe(200);
  const body = (await res.json()) as { config_policy?: Record<string, unknown> | null };
  return body.config_policy ?? {};
}

/**
 * mintKey creates the key the dialog legs act on, and it is created rather than named for a second reason
 * beyond portability: the first draft revoked `key_admin`, which on a real stack is not a key id at all — and
 * if it had been, it would have been THE BOOTSTRAP KEY the rest of the suite authenticates with.
 */
async function mintKey(page: Page, projectID: string): Promise<string> {
  const res = await api(page, "post", "/api-keys", { project_id: projectID, scopes: ["provision"] });
  expect(res.status(), "the key the dialog legs act on could not be minted").toBeLessThan(300);
  const id = ((await res.json()) as { id?: string }).id ?? "";
  expect(id, "POST /v1/api-keys returned no id").not.toBe("");
  return id;
}

/** anyPoolID is the first pool the API offers. The fixture's `pool_mac` does not exist on a real stack. */
async function anyPoolID(page: Page): Promise<string> {
  const res = await api(page, "get", "/runner-pools");
  expect(res.status(), "GET /v1/runner-pools did not answer — the pool picker has nothing to offer").toBe(200);
  const id = ((await res.json()) as { data?: { id?: string }[] }).data?.[0]?.id ?? "";
  expect(id, "no runner pool exists on this stack — every tenant is seeded one at birth, so this is a real failure").not.toBe("");
  return id;
}

test("setting only the pool from the console leaves the approver list intact", async ({ page }) => {
  const PROJECT = await seedProject(page);
  const POOL = await anyPoolID(page);

  // THE PRECONDITION IS ASSERTED, NOT ASSUMED. If the project carried no approvers, "the approvers survived"
  // would be true of a form that erased them — the classic vacuous pass.
  const before = await readPolicy(page, PROJECT);
  expect(before.approvers, "the seeded project must already carry approvers or this test proves nothing").toEqual([APPROVER]);
  expect(before.allowed_tools, "the seeded project must already carry an allow-list or the second half proves nothing").toEqual(["git.push"]);

  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });

  // Choose the subject, then assert THE FORM READ ITS CURRENT DOCUMENT. This is the fix's mechanism made
  // visible: a form that writes the whole document has to SHOW the whole document first, and an operator who
  // cannot see a field cannot be said to have chosen to keep it.
  await chooseOption(page, "policy-project-select", PROJECT);
  await expect(page.getByTestId("policy-approvers-input")).toHaveValue(APPROVER);
  await expect(page.getByTestId("policy-allowed-tools-input")).toHaveValue("git.push");

  // Set ONLY the pool — the one action the operator came to perform.
  await chooseOption(page, "policy-pool-select", POOL);
  await page.getByTestId("policy-save-button").click();
  await expect(page.getByTestId("policy-status")).toBeVisible({ timeout: 15_000 });

  // THE READ-BACK, over the public API. Not the form's own state — a form that lost the field would still
  // be showing it.
  const after = await readPolicy(page, PROJECT);
  expect(after.pool, "the pool the operator actually came to set was not written").toBe(POOL);
  expect(
    after.approvers,
    "SETTING THE POOL ERASED THE APPROVER LIST. PATCH /v1/projects/{id} is an assignment (identity/store.go:269-275) " +
      "and HIL-P11 measured that an empty approver list is PERMISSIVE, so this console just opened the approval " +
      "gate to every principal in the project.",
  ).toEqual([APPROVER]);
  expect(after.allowed_tools, "setting the pool erased the tool allow-list").toEqual(["git.push"]);
  expect(after.default_tools, "setting the pool erased the default tool set").toEqual(["git.push"]);
  expect(after.allowed_models, "setting the pool erased the model allow-list").toEqual(["fake"]);
  // The E28 exit gate's policy row (plan §T4 group (d)). It is an EQUALITY rather than a zero, and the
  // before-count is printed beside the after-count because a write measured against an already-empty list
  // cannot show that anything survived.
  const entries = (v: unknown): number => (Array.isArray(v) ? v.length : 0);
  console.log(`POLICY WRITE — changed=pool approvers_before=${entries(before.approvers)} approvers_after=${entries(after.approvers)}`);
});

test("the request body carries all five policy fields, not the one that changed", async ({ page }) => {
  // THE WIRE, NOT THE OUTCOME. The leg above could be satisfied by a server that merged; this one asserts the
  // property the console is responsible for — every field present in one request — against the bytes the
  // browser actually sent. The real API does not merge, so these are the same claim on a real stack; asserting
  // both is what keeps that from being an assumption about the fixture.
  const PROJECT = await seedProject(page);
  const POOL = await anyPoolID(page);
  const patches: NetRequest[] = [];
  page.on("request", (req) => {
    if (req.method() === "PATCH" && req.url().includes("/api/palai/v1/projects/")) patches.push(req);
  });

  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await chooseOption(page, "policy-project-select", PROJECT);
  await expect(page.getByTestId("policy-approvers-input")).toHaveValue(APPROVER);
  await chooseOption(page, "policy-pool-select", POOL);
  await page.getByTestId("policy-save-button").click();
  await expect(page.getByTestId("policy-status")).toBeVisible({ timeout: 15_000 });

  expect(patches.length, "the console sent no PATCH at all").toBeGreaterThan(0);
  const sent = JSON.parse(patches[patches.length - 1].postData() ?? "{}") as { config_policy?: Record<string, unknown> };
  const policy = sent.config_policy ?? {};
  expect(
    Object.keys(policy).sort(),
    "the request named fewer than the five fields configPolicyInput carries, so the ones it omitted were erased",
  ).toEqual(["allowed_models", "allowed_tools", "approvers", "default_tools", "pool"]);
  expect(policy.approvers).toEqual([APPROVER]);
  // The half a stored-outcome assertion cannot see, printed for the exit gate: a server that MERGED would
  // let "the approvers survived" pass over a form still sending one field.
  console.log(`POLICY WRITE — changed=pool fields_in_request=${Object.keys(policy).length}`);
});

test("an empty approver list is shown as permissive, in words, before it is saved", async ({ page }) => {
  // HIL-P11 ON SCREEN. Writing a ceiling where the operator can act on it does not close it, and this screen
  // does not claim otherwise — what it prevents is the false confidence of an empty box that looks locked.
  const PROJECT = await seedProject(page);
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await chooseOption(page, "policy-project-select", PROJECT);

  // Full list: no warning.
  await expect(page.getByTestId("policy-approvers-input")).toHaveValue(APPROVER);
  await expect(page.getByTestId("policy-approvers-permissive")).toHaveCount(0);

  await page.getByTestId("policy-approvers-input").fill("");
  const warning = page.getByTestId("policy-approvers-permissive");
  await expect(warning).toBeVisible();
  await expect(warning).toContainText(/every/i);
});

test("the pool control stays a dropdown and never degrades to a free-text box", async ({ page }) => {
  // Picker's rule, applied where it is most tempting to break it: a fleet with ONE pool is the shape every
  // deployment had before E28 T1, and a select with one option is still a select. A text box here accepts a
  // pool id that does not exist, which is then refused several steps later as a failure about a RUN.
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  const pool = page.getByTestId("policy-pool-select");
  await expect(pool).toBeVisible();
  // THE CLAIM IS UNCHANGED AND THE CHECK IS STRONGER (E29 component layer). It used to read
  // `tagName === "SELECT"`, which a <select> holding ZERO options would also have satisfied — the very
  // degradation Picker's rule exists to prevent, passing the test written to catch it. The control is now a
  // components/ui/Select, so the three things are asserted separately: it is not a text box, it advertises a
  // listbox, and OPENING it yields rows. The last one is what the tag check could never do.
  expect(await pool.evaluate((el) => el.tagName), "the pool control is not a button-triggered listbox").toBe("BUTTON");
  await expect(pool).toHaveAttribute("aria-haspopup", "listbox");
  await expect(page.locator('input[type="text"][data-testid="policy-pool-select"]')).toHaveCount(0);
  await pool.click();
  const options = page.getByRole("listbox").getByRole("option");
  await expect(options, "the pool listbox opened with nothing in it — a control that cannot be satisfied").not.toHaveCount(0);
});

// --- THE IRREVERSIBLE CONFIRMATION (plan §3.5 W1/W2/W5) -------------------------------------------------
//
// A revoke cannot satisfy SC 3.3.4's Reversible leg — runner_gateway.go:328's "decommissioned, not paused"
// is the fleet's wording of the same fact an API key has — so the Confirmed leg is the only one available,
// and the word "reviewing" inside it is what makes a hand-rolled dialog necessary rather than nicer. Once it
// is necessary, the ARIA APG's four requirements are not negotiable, and neither is the keyboard contract.
//
// window.confirm IS NOT REPLACED ANYWHERE, and the last test in this file is what keeps that true.

// openRevoke opens the row's ⋯ menu and clicks Revoke inside it.
//
// THE CONTROL MOVED INTO A ROW-END MENU (page-parity pass) and this helper is the only thing that changed
// about these two legs: every assertion below is the one it was, made against the same dialog. The menu
// deliberately stays OPEN while the dialog is up, because ConfirmDestructive returns focus to the element
// that opened it and an element removed from the DOM cannot receive it — which is what the last assertion
// of the Escape leg checks.
async function openRevoke(page: Page, key: string): Promise<void> {
  await page.getByTestId(`key-menu-${key}`).click();
  await page.getByTestId(`revoke-${key}`).click();
}

test("the revoke dialog is an alertdialog that reviews what is about to die", async ({ page }) => {
  const key = await mintKey(page, await seedProject(page));
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await openRevoke(page, key);

  const dialog = page.getByTestId("key-revoke-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toHaveAttribute("role", "alertdialog");
  await expect(dialog).toHaveAttribute("aria-modal", "true");
  // The two pointers, RESOLVED rather than merely present: an aria-labelledby naming a missing id is an
  // attribute that reads as compliance and announces nothing.
  const labelledBy = await dialog.getAttribute("aria-labelledby");
  const describedBy = await dialog.getAttribute("aria-describedby");
  expect(labelledBy).not.toBeNull();
  expect(describedBy).not.toBeNull();
  await expect(page.locator(`#${String(labelledBy)}`)).toHaveText(/revoke/i);
  await expect(page.locator(`#${String(describedBy)}`)).toContainText(/never comes back/i);

  // THE REVIEW HALF. "Are you sure?" confirms nothing; the operator must be able to see WHICH key.
  const review = page.getByTestId("key-revoke-dialog-review");
  await expect(review).toContainText(key);
  await expect(review).toContainText(/capabilit/i);

  // FOCUS STARTS ON CANCEL. No accessibility requirement is claimed for this (§3.5 W5 is UNCONFIRMED) — it
  // is what window.confirm already does in this console, and matching it is how this change claims nothing.
  await expect(page.getByTestId("key-revoke-dialog-cancel")).toBeFocused();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
});

test("Tab cannot leave the open dialog, and Escape cancels it", async ({ page }) => {
  const key = await mintKey(page, await seedProject(page));
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await openRevoke(page, key);
  await expect(page.getByTestId("key-revoke-dialog")).toBeVisible();

  // TWENTY PRESSES, FORWARD AND BACK. The dialog holds two controls, so twenty presses is nine full cycles —
  // enough that a trap which only holds for one lap fails here. Focus is read as "is the active element
  // inside the dialog", which is the property that matters and not "is it one of two ids".
  //
  // THE ASSERTION IS UNCHANGED ACROSS THE E29 COMPONENT LAYER, AND THAT IS THE POINT. The dialog is now
  // portalled and its focus management is @base-ui/react's — whose Tab redirect runs one
  // requestAnimationFrame after the key, which this cadence outruns: measured, forty presses reached
  // `.skip-link`, `.brand` and `key-mint-button` on the page behind. components/ui/Dialog.tsx therefore
  // keeps a synchronous keydown wrap, and this line is what says so — a trap that only holds for a human is
  // not what this test was written to accept.
  const inside = () => page.evaluate(() => document.querySelector('[data-testid="key-revoke-dialog"]')?.contains(document.activeElement) === true);
  for (let i = 0; i < 20; i++) {
    await page.keyboard.press("Tab");
    expect(await inside(), `Tab press ${String(i + 1)} moved focus out of the dialog`).toBe(true);
  }
  for (let i = 0; i < 20; i++) {
    await page.keyboard.press("Shift+Tab");
    expect(await inside(), `Shift+Tab press ${String(i + 1)} moved focus out of the dialog`).toBe(true);
  }

  // THE REST OF THE DOCUMENT IS HIDDEN FROM A SCREEN READER WHILE THIS IS OPEN, and that is a property the
  // inline dialog never had: it carried aria-modal="true" and nothing else, so an AT that ignores the
  // attribute could walk straight into the page behind it. The portalled popup is a body-level node and
  // every OTHER body-level node is marked, which is the mechanism rather than the promise. Measured here as
  // "the page's own container is hidden", so it cannot pass by finding some unrelated marked node.
  const contained = await page.evaluate(() => {
    const main = document.querySelector("main");
    for (let n: Element | null = main; n !== null; n = n.parentElement) {
      if (n.getAttribute("aria-hidden") === "true") return true;
    }
    return false;
  });
  expect(contained, "the page behind the open dialog is still exposed to a screen reader").toBe(true);

  await page.keyboard.press("Escape");
  await expect(page.getByTestId("key-revoke-dialog")).toHaveCount(0);
  // AND THE MARKING IS UNDONE. A containment that is never lifted is a console that goes silent after one
  // dialog, which is a worse failure than never containing at all — and it is invisible on screen.
  const released = await page.evaluate(() => {
    const main = document.querySelector("main");
    for (let n: Element | null = main; n !== null; n = n.parentElement) {
      if (n.getAttribute("aria-hidden") === "true") return false;
    }
    return true;
  });
  expect(released, "the page stayed hidden from a screen reader after the dialog closed").toBe(true);
  // NOTHING WAS REVOKED — asserted over the API rather than over the rendered row, because a row that failed
  // to refresh would look the same as one that was never revoked. An Escape that cancelled the dialog and
  // performed the action anyway would be the worst possible reading of "the least destructive default".
  const after = await api(page, "get", `/api-keys/${encodeURIComponent(key)}`);
  expect(((await after.json()) as { revoked_at?: string | null }).revoked_at ?? null, "Escape revoked the key").toBeNull();
  // And focus came back to the control that opened it.
  await expect(page.getByTestId(`revoke-${key}`)).toBeFocused();
});

test("window.confirm is still the confirmation for the REVERSIBLE actions", async ({ page }) => {
  // THE DISTINCTION IS THE POINT OF THE COMPONENT, and the temptation is to convert everything. SC 3.3.4 is
  // satisfied by ANY ONE of its three legs, and unbinding an environment key satisfies leg 1 — so the native
  // dialog stays, keeping every property (keyboard, announcement, focus trap) that a hand-rolled one has to
  // re-earn. This asserts it on the RENDERED page rather than by reading the source.
  let confirms = 0;
  page.on("dialog", (dialog) => {
    confirms += 1;
    void dialog.dismiss();
  });
  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });

  // Create an environment and a key, so there is something to unbind.
  const name = `t2-confirm-${Date.now()}`;
  await page.getByTestId("env-name-input").fill(name);
  await page.getByTestId("env-create-button").click();
  await expect(page.getByTestId("environment-create-status")).toContainText(name, { timeout: 15_000 });
  await page.getByTestId("value-key-input").fill("CONFIRM_PROBE");
  await page.getByTestId("value-secret-input").fill("t2-confirm-probe-not-a-credential");
  await page.getByTestId("value-write-button").click();
  await expect(page.getByTestId("environment-value-status")).toContainText("version 1", { timeout: 15_000 });

  await page.getByTestId("unbind-CONFIRM_PROBE").click();
  expect(confirms, "the reversible unbind no longer goes through window.confirm").toBe(1);
  // Dismissed, so nothing happened — which is also the proof that the native dialog is still load-bearing.
  await expect(page.getByTestId("panel-environment-keys")).toContainText("CONFIRM_PROBE");
  await expect(page.getByTestId("key-revoke-dialog")).toHaveCount(0);
});
