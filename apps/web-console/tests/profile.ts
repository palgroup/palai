import { expect, test, type Page } from "@playwright/test";

import { DIVERGENCE_BY_ID } from "./divergences.mjs";
import { CONSOLE_PASSWORD, IS_REAL, NEXT_PORT, PROFILE } from "./constants";

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

// signIn opens the console's door for a browser context, and every spec that reads data needs it now — the
// relay answers 401 without a session (E25 T1). It goes through the REAL login route with the REAL password,
// so no spec is handed a forged cookie: if the door stopped working, the whole suite would fail rather than
// quietly testing an open console.
//
// `page.request` shares the browser context's cookie jar, so the Set-Cookie lands where the page will send
// it. Origin is set explicitly because an APIRequestContext does not stamp one, and the login route refuses
// a mutation without it — the same refusal a foreign page would get.
export async function signIn(page: Page): Promise<void> {
  const origin = `http://127.0.0.1:${NEXT_PORT}`;
  const res = await page.request.post(`${origin}/api/console/login`, {
    data: { password: CONSOLE_PASSWORD },
    headers: { Origin: origin, "Content-Type": "application/json" },
  });
  // Not an `expect`: a failure here is a broken precondition for everything after it, and it must read as
  // that rather than as an assertion inside whichever test happened to be first.
  if (res.status() !== 204) {
    throw new Error(`sign-in failed with ${res.status()} — the console's door is not working, so nothing after this proves anything`);
  }
}

// sessionHeaders returns the session as an explicit Cookie header, for the API-LEVEL requests some specs
// make (`page.request`, `request`) rather than browser navigations.
//
// MEASURED, not guessed: Playwright's APIRequestContext does NOT apply the browser's "localhost and
// 127.0.0.1 are potentially-trustworthy" exception to a `Secure` cookie, so over loopback HTTP it withholds
// the session and every such request comes back 401 — while `page.goto` in the same context sends it
// happily, because Chromium does apply the exception. `context.cookies(url)` filters the same way, which is
// why the lookup below passes no url. The cookie is read from the jar the real login route filled; nothing
// here is forged.
export async function sessionHeaders(page: Page): Promise<Record<string, string>> {
  const cookie = (await page.context().cookies()).find((c) => c.name === "palai_console_session");
  if (cookie === undefined) throw new Error("no session cookie in the browser context — signIn(page) must run first");
  return { Cookie: `${cookie.name}=${cookie.value}` };
}

// signInViaForm signs in through the PAGE — the real form, a real browser fetch — so the login request is
// visible to page.on("request"). That is required by exactly one spec and is the reason there are two
// helpers: public-api-only.spec.ts has to SEE the login request to assert that it is the single non-relay
// exception and that it carries no Bearer. A page.request post is invisible to that intercept, so using the
// fast helper there would have made the exception unobservable and the assertion vacuous.
export async function signInViaForm(page: Page): Promise<void> {
  await page.goto("/login");
  await page.getByTestId("password-input").fill(CONSOLE_PASSWORD);
  await page.getByTestId("login-button").click();
  // The form navigates to / on success. A refusal would leave the error region on screen instead.
  await expect(page.getByTestId("ceiling-note")).toBeVisible({ timeout: 15_000 });
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
