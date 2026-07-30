import { test, expect } from "@playwright/test";

import { announceProfile, runToTerminal, signIn, skipOnReal } from "./profile";

// The live journey against the built console: provision reads → start a run → watch the lane-separated
// timeline → act on the EXACT approval → see a recovery/attempt → download an artifact. UI-002 is the crown:
// the AUTHORITATIVE approval detail — the operation/branch/request_hash the canonical approval.requested.v1
// event actually carries — is never replaced by the proposal-supplied display string.
//
// E19 T7 — WHAT RUNS WHERE, AND WHY. The approval journey is FAKE-PROFILE ONLY, and that is a finding about
// the control plane rather than a choice about the specs: a compose run cannot reach approval.requested.v1
// at all, for three independent reasons enumerated in DIV-UI-001 (no workspace root and no shared volume in
// compose so no repository is ever prepared; a hardcoded fake adapter with no ToolCalls and a NULL
// config_policy so no tool is ever advertised; and publishApproved living only inside a live attempt). A
// FOURTH reason has since been fixed — PendingBoundaryCommands filtered approve/deny out of the boundary
// pump, making the HTTP approve path a dead branch — so the approve a human clicks is now applied; what
// still cannot happen on compose is producing something to approve. The conformance sweep re-derives that
// measurement on every run — MEASURED there, cited here — so if it ever stops being true the sweep goes red
// before these skips can go stale.
//
// The last test in this file is the one that DOES run on both, and it is what materially narrowed: the
// console rendering a REAL run's terminal, retrieved from a real control plane over real SSE.
test.beforeAll(() => announceProfile("journey.spec.ts"));
// The relay answers 401 without an operator session (E25 T1).
test.beforeEach(async ({ page }) => signIn(page));

test("UI-002: the approval UI shows the authoritative operation/branch/request_hash from the canonical event — the proposal display string does not replace them", async ({ page }) => {
  skipOnReal("DIV-UI-001");
  await page.goto("/runs");
  await page.getByTestId("run-button").click();

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });

  // The AUTHORITATIVE, model-independent detail the approval.requested.v1 event ACTUALLY carries
  // (packages/coordinator/publication.go): operation / branch / request_hash — each its own field, surfaced
  // from the canonical event, not from the model's prose. The sweep confirms this is the real five-key
  // payload, so the fixture's approval shape is one of the few it gets exactly right.
  await expect(page.getByTestId("approval-operation")).toHaveText("push_branch");
  await expect(page.getByTestId("approval-branch")).toHaveText("release");
  await expect(page.getByTestId("approval-request-hash")).toContainText("sha256:9f2b1c");

  // The proposal-supplied display string is present but SEPARATE and explicitly non-authoritative — it must
  // NOT replace the detail above. The soothing "everything looks fine, safe to approve" does not stand in for
  // the operation/branch the operator is actually authorizing.
  await expect(page.getByTestId("approval-display")).toContainText("not authoritative");
  await expect(page.getByTestId("approval-display")).toContainText("looks fine");
  // Proof the display did NOT substitute: the authoritative operation + branch are still shown independently.
  await expect(page.getByTestId("approval-operation")).toHaveText("push_branch");
  await expect(page.getByTestId("approval-branch")).toHaveText("release");
});

test("approve proceeds through recovery to a completed run with a downloadable artifact", async ({ page }) => {
  skipOnReal("DIV-UI-001");
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
  skipOnReal("DIV-UI-001");
  await page.goto("/runs");
  await page.getByTestId("run-button").click();

  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("deny-button").click();

  await expect(page.getByTestId("terminal-status")).toContainText(/cancel/i, { timeout: 15_000 });
  // The push tool-completed frame never arrives — deny genuinely blocked the side effect.
  await expect(page.getByTestId("timeline")).not.toContainText("tool_call.completed.v1");
});

// THE REAL-UPSTREAM LEG (E19 T7). On the real profile every byte behind this is genuine: the relay presents
// a real bootstrap API key server-side, POSTs to a real /v1/responses (with the Idempotency-Key the real
// router requires and the fixture does not — DIV-STA-001), reads a real Server-Sent-Events stream off the
// real journal, and retrieves the real terminal Response projection. Nothing here is scripted.
//
// It runs on BOTH profiles deliberately: the same assertions over the same UI, so the fake layer stays
// honest about the one journey both surfaces can complete.
test("the console renders a run's terminal state end to end — model, status and output from the upstream", async ({ page }) => {
  await runToTerminal(page);

  await expect(page.getByTestId("terminal-status")).toContainText("completed");
  // The model is chosen by the SERVER and echoed back on the terminal Response; the console never picks it.
  await expect(page.getByTestId("model")).toContainText("fake");
  // A non-empty structured output projection — the run actually produced something.
  await expect(page.getByTestId("final-output")).not.toBeEmpty();

  // The two lanes a run journals on ANY profile: the model step and the terminal. The remaining four lanes
  // are fixture-only against a compose stack (DIV-UI-002), so asserting them here would be asserting the
  // fixture rather than the console.
  const timeline = page.getByTestId("timeline");
  for (const lane of ["model_step", "terminal"]) {
    await expect(timeline.locator(`[data-lane="${lane}"]`).first()).toBeVisible();
  }
});
