import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

import { CONSOLE_ROUTES } from "../lib/routes";
import { IS_REAL, WCAG_TAGS } from "./constants";
import { announceProfile, runToTerminal, signIn } from "./profile";

// tabToTestId genuinely presses Tab until the element carrying data-testid=id holds focus — proving KEYBOARD
// REACHABILITY. This is stronger than `.focus()`, which succeeds even on a tabindex=-1 element that Tab can
// never reach; here a control dropped from the tab order would never be reached and this fails.
async function tabToTestId(page: Page, id: string, max = 30) {
  for (let i = 0; i < max; i++) {
    const onTarget = await page.evaluate((t) => document.activeElement?.getAttribute("data-testid") === t, id);
    if (onTarget) return;
    await page.keyboard.press("Tab");
  }
  throw new Error(`Tab never reached [data-testid="${id}"] within ${max} presses — not keyboard-reachable`);
}

// UI-001 (accessibility). The AUTOMATED ceiling: axe-core finds zero violations on EVERY route the console
// declares, keyboard navigation reaches the skip link first and operates the core run flow with no mouse, and
// status is conveyed by a glyph + word (never color alone).
//
// E19 T7 — THIS FILE RUNS ON BOTH PROFILES, AND THAT IS THE POINT. Against the real compose stack these scans
// cover surfaces the fixture cannot produce: an admin panel rendering a genuinely EMPTY collection, and
// whatever a capability the real stack does not mount renders (an ERROR panel). Both are states a real
// operator meets on day one and no fake had ever rendered.
//
// E25 T2 — THE SCANNED ROUTES ARE DERIVED FROM lib/routes.ts, NOT TYPED HERE (plan §3.6 D17). They were typed
// here, in two places, while Playwright collected new SPECS automatically — so a new PAGE got zero axe
// coverage and nothing said so. One scan is now generated per route, which makes the test COUNT of this file
// a function of the list's length, and the coverage assertion at the bottom fails if any route went unscanned.
//
// E25 T2 — AND THE TAG SET IS WIDER (plan §3.6 D16): WCAG_TAGS adds wcag21a/wcag21aa/wcag22aa to the
// wcag2a/wcag2aa this file used to pass, because those two are WCAG 2.0 ONLY and the 2.2 form criteria were
// entirely outside the scan. What that buys is MEASURED below rather than assumed — the widening adds exactly
// three axe rules, and one of the three tags adds none at all.
//
// What still does NOT narrow: a manual VoiceOver/screen-reader pass over a DEPLOYED console. Compose is not
// a deployment, and axe is not a screen reader. That remains §6 operator leg 8, and axe's own published rate
// is "on average 57% of WCAG issues" — so what is proven here is "axe-clean with the WCAG 2.0+2.1+2.2 tags",
// never "accessible".
test.beforeAll(() => announceProfile("a11y.spec.ts"));
// The relay answers 401 without an operator session (E25 T1), so every page in this file needs the door
// opened first. It goes through the REAL login route with the REAL password — no forged cookie.
test.beforeEach(async ({ page }) => signIn(page));

// scanned records which declared routes this run actually put through axe. The coverage test at the bottom
// compares it to lib/routes.ts, so a route that is never scanned is a FAILURE rather than an absence.
const scanned = new Set<string>();

for (const route of CONSOLE_ROUTES) {
  test(`axe-core reports zero violations on ${route.path}`, async ({ page }) => {
    await page.goto(route.path);
    // The route's OWN readiness signal. axe on a page still rendering "Loading…" scans a spinner and reports a
    // clean bill of health for markup it never saw, which is why lib/routes.ts requires this field.
    await expect(page.getByTestId(route.readyTestId)).toBeVisible({ timeout: 15_000 });
    const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
    // A scan that ran ZERO rules would report zero violations too. Rules are what makes this a measurement.
    expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
    scanned.add(route.path);
  });
}

// THIS TEST KEEPS ITS NAME, and the reason is a record rather than a preference: tests/uat/cases/UI-001 —
// a SHIPPED case inside the committed extensions-0.1.0 bundle — declares this exact title as one of its
// proofs, and tests/uat/extensions resolves every declared proof to a real `test("<title>"` in the tree. A
// rename would make a shipped case cite evidence that no longer exists. So the generated loop above scans "/"
// as one of the declared routes, and this scans it again with the assertion the loop cannot make generically:
// that the panel held REAL ROWS and not a spinner when axe looked at it.
test("axe-core reports zero violations on the admin surface", async ({ page }) => {
  await page.goto("/");
  // org_local is the id BOTH surfaces carry: the fixture seeds it, and identity/store.go's ProvisionFirstOrg
  // seeds it on every real bootstrap. Its display NAME is not — the real seed passes no orgName at all
  // (DIV-SHP-001) — so asserting on the name would be asserting on the fixture.
  await expect(page.getByTestId("panel-organizations")).toContainText("org_local", { timeout: 15_000 });
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});

// THE WIDENED TAG SET IS NOT DECORATION, AND THIS IS WHERE THAT STOPS BEING A CLAIM (plan §3.6 D16).
//
// An unknown or misspelled axe tag selects NO rules and analyze() then reports zero violations — identical
// output to a clean page. So the widening is verified by what it adds to the RULE SET, and the numbers are the
// measured ones: `wcag21aa` brings autocomplete-valid and avoid-inline-spacing, `wcag22aa` brings target-size,
// and `wcag21a` brings NOTHING — no axe-core 4.12 rule carries that tag. It stays in WCAG_TAGS because it is
// the correct tag for WCAG 2.1 Level A and a future rule should be picked up for free, but nobody should read
// it as coverage: it is a subscription, not a scan. That is the honest cost of widening a tag set.
test("the WCAG 2.1/2.2 tags genuinely select rules the 2.0 tags do not", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("panel-organizations")).toBeVisible({ timeout: 15_000 });

  const ruleIDs = async (tags: string[]) => {
    const r = await new AxeBuilder({ page }).withTags(tags).analyze();
    return new Set([...r.passes, ...r.violations, ...r.incomplete, ...r.inapplicable].map((x) => x.id));
  };
  const narrow = await ruleIDs(["wcag2a", "wcag2aa"]);
  const wide = await ruleIDs(WCAG_TAGS);
  const added = [...wide].filter((id) => !narrow.has(id)).sort();

  // eslint-disable-next-line no-console -- the number is the evidence; a silent widening is the trap.
  console.log(`AXE TAG WIDENING — 2.0 tags select ${narrow.size} rules, WCAG_TAGS selects ${wide.size}; added: ${added.join(", ")}`);
  expect(added).toEqual(["autocomplete-valid", "avoid-inline-spacing", "target-size"]);
});

test("axe-core reports zero violations on the live-run surface after a completed run", async ({ page }) => {
  // Render everything the profile can produce and reach terminal, so every live panel present on this
  // profile — timeline, recovery, usage, artifacts, and on the fake profile the approval detail — is in the
  // scan. The real profile's surface is honestly thinner (DIV-UI-002) and is scanned as it actually is.
  await runToTerminal(page);
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});

test("keyboard navigation: skip link is the first stop and the run→approve flow works with no mouse", async ({ page }) => {
  await page.goto("/runs");

  // The FIRST Tab lands on the visible skip link (axe bypass-block, keyboard-first).
  await page.keyboard.press("Tab");
  const firstFocus = await page.evaluate(() => document.activeElement?.textContent ?? "");
  expect(firstFocus).toContain("Skip to main content");

  // Operate the core flow with the keyboard only: genuinely Tab to the run button (not .focus() — this
  // proves it is REACHABLE in the tab order) and activate it with Enter.
  await tabToTestId(page, "run-button");
  await expect(page.getByTestId("run-button")).toBeFocused();
  await page.keyboard.press("Enter");

  if (!IS_REAL) {
    // The approve half of the claim. On the real profile no run can reach an approval at all (DIV-UI-001),
    // so the keyboard-operability claim narrows there to the run flow — stated, not silently dropped.
    await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
    await tabToTestId(page, "approve-button");
    await expect(page.getByTestId("approve-button")).toBeFocused();
    await page.keyboard.press("Enter");
  }

  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 60_000 });

  // Status is NOT color-only: the terminal status carries a visible glyph + the word.
  const glyph = page.getByTestId("terminal-status").locator(".glyph");
  await expect(glyph).toHaveText("✔");
  await expect(page.getByTestId("terminal-status")).toContainText("completed");
});

// COVERAGE — DECLARED LAST ON PURPOSE, so it runs after every generated scan (workers: 1,
// fullyParallel: false, so declaration order is execution order).
//
// This is the assertion that makes an unscanned page impossible rather than unlikely: every path in
// lib/routes.ts must have been put through axe by the loop above. Adding a route without adding its scan is
// not a review question, it is a red test — and if the generated loop were ever replaced by hand-written
// tests that missed one, this is what would catch it. The count is printed so a reader can see it rise as
// E25's pages land.
test("every route lib/routes.ts declares was actually scanned by axe", () => {
  const declared = CONSOLE_ROUTES.map((r) => r.path).sort();
  // eslint-disable-next-line no-console -- the count is the evidence.
  console.log(`AXE ROUTE COVERAGE — ${scanned.size}/${declared.length} declared route(s) scanned: ${[...scanned].sort().join(", ")}`);
  expect(
    [...scanned].sort(),
    "a route declared in lib/routes.ts was never put through axe. This test asserts over the WHOLE file, so a " +
      "filtered run (--grep) fails it too — that direction is deliberate: an unscanned page must be a red test, " +
      "and 'the filter excluded the scans' is exactly the excuse a silent gap would hide behind.",
  ).toEqual(declared);
  expect(CONSOLE_ROUTES.every((r) => r.readyTestId !== "")).toBe(true);
});
