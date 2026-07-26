import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

import { IS_REAL } from "./constants";
import { announceProfile, runToTerminal } from "./profile";

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

// UI-001 (accessibility). The AUTOMATED ceiling: axe-core finds zero violations on the admin and live-run
// surfaces (WCAG 2 A/AA rule set), keyboard navigation reaches the skip link first and operates the core
// run flow with no mouse, and status is conveyed by a glyph + word (never color alone).
//
// E19 T7 — THIS FILE NOW RUNS ON BOTH PROFILES, AND THAT IS THE POINT. Against the real compose stack these
// scans cover surfaces the fixture cannot produce: an admin panel rendering a genuinely EMPTY collection,
// and an ERROR panel for a capability the real stack does not mount (/v1/secret-refs is unregistered there —
// DIV-RTE-001). Both are states a real operator meets on day one and no fake had ever rendered.
//
// What still does NOT narrow: a manual VoiceOver/screen-reader pass over a DEPLOYED console. Compose is not
// a deployment, and axe is not a screen reader. That remains §6 operator leg 8.
test.beforeAll(() => announceProfile("a11y.spec.ts"));

test("axe-core reports zero violations on the admin surface", async ({ page }) => {
  await page.goto("/");
  // org_local is the id BOTH surfaces carry: the fixture seeds it, and identity/store.go's ProvisionFirstOrg
  // seeds it on every real bootstrap. Its display NAME is not — the real seed passes no orgName at all
  // (DIV-SHP-001) — so asserting on the name would be asserting on the fixture.
  await expect(page.getByTestId("panel-organizations")).toContainText("org_local", { timeout: 15_000 });
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});

test("axe-core reports zero violations on the live-run surface after a completed run", async ({ page }) => {
  // Render everything the profile can produce and reach terminal, so every live panel present on this
  // profile — timeline, recovery, usage, artifacts, and on the fake profile the approval detail — is in the
  // scan. The real profile's surface is honestly thinner (DIV-UI-002) and is scanned as it actually is.
  await runToTerminal(page);
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
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
