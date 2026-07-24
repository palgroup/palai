import AxeBuilder from "@axe-core/playwright";
import { test, expect } from "@playwright/test";

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

  // Operate the core flow with the keyboard only: focus the run button and activate it with Enter.
  await page.getByTestId("run-button").focus();
  await expect(page.getByTestId("run-button")).toBeFocused();
  await page.keyboard.press("Enter");

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").focus();
  await expect(page.getByTestId("approve-button")).toBeFocused();
  await page.keyboard.press("Enter");

  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 15_000 });

  // Status is NOT color-only: the terminal status carries a visible glyph + the word.
  const glyph = page.getByTestId("terminal-status").locator(".glyph");
  await expect(glyph).toHaveText("✔");
  await expect(page.getByTestId("terminal-status")).toContainText("completed");
});
