import { expect, test, type Page } from "@playwright/test";

import { DIVERGENCE_BY_ID } from "./divergences.mjs";
import { IS_REAL, PROFILE } from "./constants";

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

// announceProfile prints which upstream a spec file actually ran against. "which profile ran" is a question
// the reports have had to answer by inference until now.
export function announceProfile(file: string): void {
  // eslint-disable-next-line no-console -- see above.
  console.log(`PROFILE=${PROFILE} — ${file}`);
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
