import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

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
// run→approve flow with no mouse, and status is conveyed by a glyph + word (never color alone). A manual
// VoiceOver/screen-reader pass over a DEPLOYED console is the §6 operator leg above this automated ceiling.

test("axe-core reports zero violations on the admin surface", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("panel-organizations")).toContainText("Local Org", { timeout: 15_000 });
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});

test("axe-core reports zero violations on the live-run surface after a completed run", async ({ page }) => {
  await page.goto("/runs");
  await page.getByTestId("run-button").click();
  // Render the approval panel (its detail must be a11y-clean too), approve, and reach terminal so every
  // live panel — timeline, recovery, usage, artifacts — is present for the scan.
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 15_000 });
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

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  // Tab through to the approve control the same way, then activate it — the approval detail is reached and
  // operated with no mouse.
  await tabToTestId(page, "approve-button");
  await expect(page.getByTestId("approve-button")).toBeFocused();
  await page.keyboard.press("Enter");

  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 15_000 });

  // Status is NOT color-only: the terminal status carries a visible glyph + the word.
  const glyph = page.getByTestId("terminal-status").locator(".glyph");
  await expect(glyph).toHaveText("✔");
  await expect(page.getByTestId("terminal-status")).toContainText("completed");
});
