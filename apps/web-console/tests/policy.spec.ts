import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Request as NetRequest } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, sessionHeaders, signIn } from "./profile";

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

// The seeded project and the policy it already carries. `proj_local` is the fixture's project id; on the real
// profile the page reads whatever the stack seeds, and the assertions below are about what the form SENT
// rather than about these particular strings — see the wire leg.
const PROJECT = "proj_local";

/** readPolicy reads the stored document back THROUGH THE RELAY, i.e. over the same public API the form uses. */
async function readPolicy(page: import("@playwright/test").Page, projectID: string): Promise<Record<string, unknown>> {
  const res = await page.request.get(`/api/palai/v1/projects/${encodeURIComponent(projectID)}`, { headers: await sessionHeaders(page) });
  expect(res.status(), "the project could not be read back — the assertion below would be about nothing").toBe(200);
  const body = (await res.json()) as { config_policy?: Record<string, unknown> | null };
  return body.config_policy ?? {};
}

test("setting only the pool from the console leaves the approver list intact", async ({ page }) => {
  // THE PRECONDITION IS ASSERTED, NOT ASSUMED. If the project carried no approvers, "the approvers survived"
  // would be true of a form that erased them — the classic vacuous pass.
  const before = await readPolicy(page, PROJECT);
  expect(before.approvers, "the seeded project must already carry approvers or this test proves nothing").toEqual(["prin_release_captain"]);
  expect(before.allowed_tools, "the seeded project must already carry an allow-list or the second half proves nothing").toEqual(["git.push"]);

  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });

  // THE FORM READ THE CURRENT DOCUMENT. This is the fix's mechanism made visible: a form that writes the
  // whole document has to SHOW the whole document first, and an operator who cannot see a field cannot be
  // said to have chosen to keep it.
  await expect(page.getByTestId("policy-approvers-input")).toHaveValue("prin_release_captain");
  await expect(page.getByTestId("policy-allowed-tools-input")).toHaveValue("git.push");

  // Set ONLY the pool — the one action the operator came to perform.
  await page.getByTestId("policy-project-select").selectOption(PROJECT);
  await expect(page.getByTestId("policy-approvers-input")).toHaveValue("prin_release_captain");
  await page.getByTestId("policy-pool-select").selectOption("pool_mac");
  await page.getByTestId("policy-save-button").click();
  await expect(page.getByTestId("policy-status")).toBeVisible({ timeout: 15_000 });

  // THE READ-BACK, over the public API. Not the form's own state — a form that lost the field would still
  // be showing it.
  const after = await readPolicy(page, PROJECT);
  expect(after.pool, "the pool the operator actually came to set was not written").toBe("pool_mac");
  expect(
    after.approvers,
    "SETTING THE POOL ERASED THE APPROVER LIST. PATCH /v1/projects/{id} is an assignment (identity/store.go:269-275) " +
      "and HIL-P11 measured that an empty approver list is PERMISSIVE, so this console just opened the approval " +
      "gate to every principal in the project.",
  ).toEqual(["prin_release_captain"]);
  expect(after.allowed_tools, "setting the pool erased the tool allow-list").toEqual(["git.push"]);
  expect(after.default_tools, "setting the pool erased the default tool set").toEqual(["git.push"]);
  expect(after.allowed_models, "setting the pool erased the model allow-list").toEqual(["fake"]);
});

test("the request body carries all five policy fields, not the one that changed", async ({ page }) => {
  // THE WIRE, NOT THE OUTCOME. The leg above could be satisfied by a server that merged; this one asserts the
  // property the console is responsible for — every field present in one request — against the bytes the
  // browser actually sent. The real API does not merge, so these are the same claim on a real stack; asserting
  // both is what keeps that from being an assumption about the fixture.
  const patches: NetRequest[] = [];
  page.on("request", (req) => {
    if (req.method() === "PATCH" && req.url().includes("/api/palai/v1/projects/")) patches.push(req);
  });

  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("policy-project-select").selectOption(PROJECT);
  await page.getByTestId("policy-pool-select").selectOption("pool_mac");
  await page.getByTestId("policy-save-button").click();
  await expect(page.getByTestId("policy-status")).toBeVisible({ timeout: 15_000 });

  expect(patches.length, "the console sent no PATCH at all").toBeGreaterThan(0);
  const sent = JSON.parse(patches[patches.length - 1].postData() ?? "{}") as { config_policy?: Record<string, unknown> };
  const policy = sent.config_policy ?? {};
  expect(
    Object.keys(policy).sort(),
    "the request named fewer than the five fields configPolicyInput carries, so the ones it omitted were erased",
  ).toEqual(["allowed_models", "allowed_tools", "approvers", "default_tools", "pool"]);
  expect(policy.approvers).toEqual(["prin_release_captain"]);
});

test("an empty approver list is shown as permissive, in words, before it is saved", async ({ page }) => {
  // HIL-P11 ON SCREEN. Writing a ceiling where the operator can act on it does not close it, and this screen
  // does not claim otherwise — what it prevents is the false confidence of an empty box that looks locked.
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("policy-project-select").selectOption(PROJECT);

  // Full list: no warning.
  await expect(page.getByTestId("policy-approvers-input")).toHaveValue("prin_release_captain");
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
  expect(await pool.evaluate((el) => el.tagName)).toBe("SELECT");
});

// --- THE IRREVERSIBLE CONFIRMATION (plan §3.5 W1/W2/W5) -------------------------------------------------
//
// A revoke cannot satisfy SC 3.3.4's Reversible leg — runner_gateway.go:328's "decommissioned, not paused"
// is the fleet's wording of the same fact an API key has — so the Confirmed leg is the only one available,
// and the word "reviewing" inside it is what makes a hand-rolled dialog necessary rather than nicer. Once it
// is necessary, the ARIA APG's four requirements are not negotiable, and neither is the keyboard contract.
//
// window.confirm IS NOT REPLACED ANYWHERE, and the last test in this file is what keeps that true.

test("the revoke dialog is an alertdialog that reviews what is about to die", async ({ page }) => {
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("revoke-key_admin").click();

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
  await expect(review).toContainText("key_admin");
  await expect(review).toContainText(/capabilit/i);

  // FOCUS STARTS ON CANCEL. No accessibility requirement is claimed for this (§3.5 W5 is UNCONFIRMED) — it
  // is what window.confirm already does in this console, and matching it is how this change claims nothing.
  await expect(page.getByTestId("key-revoke-dialog-cancel")).toBeFocused();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
});

test("Tab cannot leave the open dialog, and Escape cancels it", async ({ page }) => {
  await page.goto("/policy");
  await expect(page.getByTestId("panel-api-keys")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("revoke-key_admin").click();
  await expect(page.getByTestId("key-revoke-dialog")).toBeVisible();

  // TWENTY PRESSES, FORWARD AND BACK. The dialog holds two controls, so twenty presses is nine full cycles —
  // enough that a trap which only holds for one lap fails here. Focus is read as "is the active element
  // inside the dialog", which is the property that matters and not "is it one of two ids".
  const inside = () => page.evaluate(() => document.querySelector('[data-testid="key-revoke-dialog"]')?.contains(document.activeElement) === true);
  for (let i = 0; i < 20; i++) {
    await page.keyboard.press("Tab");
    expect(await inside(), `Tab press ${String(i + 1)} moved focus out of the dialog`).toBe(true);
  }
  for (let i = 0; i < 20; i++) {
    await page.keyboard.press("Shift+Tab");
    expect(await inside(), `Shift+Tab press ${String(i + 1)} moved focus out of the dialog`).toBe(true);
  }

  await page.keyboard.press("Escape");
  await expect(page.getByTestId("key-revoke-dialog")).toHaveCount(0);
  // NOTHING WAS REVOKED. An Escape that cancelled the dialog and performed the action would be the worst
  // possible reading of "the least destructive default".
  await expect(page.getByTestId("panel-api-keys")).not.toContainText("2026-07-31");
  // And focus came back to the control that opened it.
  await expect(page.getByTestId("revoke-key_admin")).toBeFocused();
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
