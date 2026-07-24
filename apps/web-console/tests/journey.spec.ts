import { test, expect } from "@playwright/test";

// The §T10 live journey against the local stack (built console + fake /v1 upstream): provision reads →
// start a run → watch the lane-separated timeline → act on the EXACT approval → see a recovery/attempt →
// download an artifact. UI-002 is the crown here: the AUTHORITATIVE approval detail is never replaced by
// the model's summary.

test("UI-002: the approval UI shows the authoritative action/args/diff/destination/risk/expiry — the model summary does not replace it", async ({ page }) => {
  await page.goto("/runs");
  await page.getByTestId("run-button").click();

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });

  // The AUTHORITATIVE, model-independent detail — each field surfaced from the canonical event, not the
  // model's prose.
  await expect(page.getByTestId("approval-action")).toHaveText("git.push");
  await expect(page.getByTestId("approval-destination")).toContainText("github.com/acme/app");
  await expect(page.getByTestId("approval-destination")).toContainText("branch: release");
  await expect(page.getByTestId("approval-risk")).toContainText("high");
  await expect(page.getByTestId("approval-expiry")).toContainText("2026-07-24T00:05:00Z");
  await expect(page.getByTestId("approval-args")).toContainText('"commits": 3');
  await expect(page.getByTestId("approval-args")).toContainText('"branch": "release"');
  await expect(page.getByTestId("approval-diff")).toContainText("-0.1.0");
  await expect(page.getByTestId("approval-diff")).toContainText("+0.1.1");

  // The model's summary is present but SEPARATE and explicitly non-authoritative — it must NOT replace the
  // detail above. The soothing "everything looks fine, safe to approve" does not erase the high risk.
  await expect(page.getByTestId("approval-model-summary")).toContainText("not authoritative");
  await expect(page.getByTestId("approval-model-summary")).toContainText("looks fine");
  // Proof the summary did NOT substitute: the authoritative risk is still shown as high, independently.
  await expect(page.getByTestId("approval-risk")).toContainText("high");
  await expect(page.getByTestId("approval-risk")).toContainText("a bare");
});

test("approve proceeds through recovery to a completed run with a downloadable artifact", async ({ page }) => {
  await page.goto("/runs");
  await page.getByTestId("run-button").click();

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();

  // Recovery / attempt transitions are shown as their OWN lane (not folded into progress).
  await expect(page.getByTestId("recovery-display")).toContainText("recovery.proof.v1", { timeout: 15_000 });
  await expect(page.getByTestId("recovery-display")).toContainText("attempt.recovering.v1");

  // Terminal result: completed, the server-selected model, usage, and the structured output.
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 15_000 });
  await expect(page.getByTestId("model")).toContainText("fake");
  await expect(page.getByTestId("usage")).toContainText("58");
  await expect(page.getByTestId("final-output")).toContainText("Release branch pushed after approval");

  // The lane-separated timeline carries each §47.2 category distinctly.
  const timeline = page.getByTestId("timeline");
  for (const lane of ["model_step", "tool", "approval", "recovery", "usage", "terminal"]) {
    await expect(timeline.locator(`[data-lane="${lane}"]`).first()).toBeVisible();
  }

  // Artifact download rides the /v1 relay and returns the object bytes.
  const link = page.getByTestId("artifact-download");
  await expect(link).toBeVisible();
  const download = await Promise.all([page.waitForEvent("download"), link.click()]).then(([d]) => d);
  expect(download.suggestedFilename()).toBe("release-notes.txt");
});

test("deny blocks the operation — the run terminates canceled, the push never completes", async ({ page }) => {
  await page.goto("/runs");
  await page.getByTestId("run-button").click();

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("deny-button").click();

  await expect(page.getByTestId("terminal-status")).toContainText(/cancel/i, { timeout: 15_000 });
  // The push tool-completed frame never arrives — deny genuinely blocked the side effect.
  await expect(page.getByTestId("timeline")).not.toContainText("tool_call.completed.v1");
});
