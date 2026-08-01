import { expect, test } from "@playwright/test";

import { UPSTREAM } from "./constants";
import { announceProfile, signIn, skipOnReal } from "./profile";

// SINGLE SHOT, OR A TURN IN A CONVERSATION (E31).
//
// THE DEFECT: `/runs` offered a prompt, a model and an agent, and nothing on the screen said whether the
// model would see anything before this message. It could not have — measured on this branch's base:
//
//   grep -n 'session_id' app/api/palai/stream/route.ts   -> 0 (before this change)
//   app/runs/page.tsx:147                                -> setSessionId(String(f.sessionId ?? ""))
//
// `session_id` was READ off the `meta` frame and never sent, so every run this console has ever started
// opened a fresh session. The owner's words were "single shot bir şey görmüyorum" and they were exactly
// right: only one of the two things could happen, and nothing said which.
//
// The wire contract has carried the field the whole time — protocols/schemas/execution/response-create.json
// declares `session_id`, sdks/typescript's ResponseCreateRequest carries it, and
// apps/control-plane/api/responses.go:289 reads it as RequestedSessionID. This is the console catching up.
//
// WHAT IS ASSERTED, AND WHY IT IS THE UPSTREAM AND NOT THE BROWSER: `/__introspect` reports what the fake
// UPSTREAM actually received. A test that read the browser's own POST body would prove the page sent
// something and say nothing about whether the relay forwarded it — and the relay is where the field had to
// be added. tests/output-contract.spec.ts proves its own claim through the same slot for the same reason.
test.beforeAll(() => announceProfile("run-mode.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/** lastCreateSessionId is the `session_id` the upstream received on the most recent create, or null. */
async function lastCreateSessionId(request: { get: (url: string) => Promise<{ json: () => Promise<unknown> }> }): Promise<string | null> {
  const seen = (await (await request.get(`${UPSTREAM}/__introspect`)).json()) as { lastCreateSessionId?: string | null };
  return seen.lastCreateSessionId ?? null;
}

test("the screen SAYS which of the two it is about to do, before the prompt is typed", async ({ page }) => {
  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });

  // THE SENTENCE IS THE DELIVERABLE. A control that changes behaviour without saying what the behaviour IS
  // would leave the complaint exactly where it was.
  const note = page.getByTestId("run-mode-note");
  await expect(note).toBeVisible();
  await expect(note).toContainText(/single shot/i);
  await expect(note).toContainText(/opens a NEW session/i);
});

test("choosing a conversation changes the sentence AND what the upstream receives", async ({ page, request }) => {
  // A CHAINABLE SESSION IS FIXTURE-ONLY. A fresh compose stack holds no session at all, so there is nothing
  // to continue and the picker is correctly absent — which is a real state this page handles and not one
  // this test can drive.
  skipOnReal("DIV-UI-005");

  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });

  // The picker offers ACTIVE sessions only, because admission refuses a non-active one with a 409. Read the
  // value out of the control rather than naming a fixture id: the collection is mutable and shared.
  const options = await page.getByTestId("run-chain-select").evaluate((el) => el.getAttribute("data-value"));
  expect(options, "the conversation picker is not a ui/Select").not.toBeNull();

  const listbox = page.getByTestId("run-chain-select");
  await listbox.click();
  const first = page.getByRole("listbox").locator('[role="option"]').nth(1); // 0 is the "Single shot" placeholder
  const chosen = await first.getAttribute("data-value");
  expect(chosen, "the picker offers no session to continue").not.toBeNull();
  await first.click();

  // THE SENTENCE FLIPS, and it names the session rather than saying "a conversation" — an operator about to
  // append to the wrong thread needs to see which thread.
  const note = page.getByTestId("run-mode-note");
  await expect(note).toContainText(/turn in a conversation/i);
  await expect(note).toContainText(String(chosen));

  // AND THE RUN CARRIES IT TO THE UPSTREAM. This is the half that a screenshot cannot show and the half the
  // whole change is about.
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 60_000 });

  expect(
    await lastCreateSessionId(request),
    "the create carried no session_id — the conversation the operator chose went nowhere, which is the exact " +
      "defect this control was added to fix",
  ).toBe(chosen);
});

test("a single shot sends NO session_id key at all, so an unchained run is unchanged on the wire", async ({ page, request }) => {
  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });

  // THE ABSENCE IS THE ASSERTION, and it is the same rule the pin and the output contract already follow:
  // absent means the upstream body carries no key at all, so every run this console started before today
  // stays bit-identical. An empty string forwarded as `session_id: ""` would make the API decide what a
  // session named "" means, which is a question the browser should never ask it.
  await expect(page.getByTestId("run-mode-note")).toContainText(/single shot/i);
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 60_000 });

  expect(await lastCreateSessionId(request), "a single shot sent a session_id").toBeNull();
});

test("a finished run offers to continue ITS session, and the offer arms the picker rather than starting a run", async ({ page }) => {
  skipOnReal("DIV-UI-001");

  await page.goto("/runs");
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 60_000 });

  // READING "ses_…" ON THE SCREEN TAUGHT AN OPERATOR NOTHING. Being able to send the next prompt into it is
  // what makes "this run is a turn" a fact they can act on rather than a string they can copy.
  const session = await page.getByTestId("terminal-session").innerText();
  expect(session).not.toBe("");

  await page.getByTestId("run-continue").click();
  // IT ARMS, IT DOES NOT FIRE. A one-click "continue" would be a second way to start a run on a page that
  // has one, and an operator who clicked it expecting to read the session would have started work.
  await expect(page.getByTestId("run-continue-armed")).toBeVisible();
  await expect(page.getByTestId("run-mode-note")).toContainText(/turn in a conversation/i);
  await expect(page.getByTestId("run-mode-note")).toContainText(session);
  // The run that just finished is still the one on screen — nothing was started by arming.
  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i);
});

test("the picker offers only ACTIVE sessions, because admission refuses any other with a 409", async ({ page }) => {
  skipOnReal("DIV-UI-005");

  await page.goto("/runs");
  await expect(page.getByTestId("run-chain-select")).toBeVisible({ timeout: 15_000 });

  // THE SERVER DID THE FILTERING, and this asserts the result rather than the request: every row the picker
  // offers must be a session the sessions list also calls active. A client-side narrowing over a page the
  // server cut at twenty would silently omit an active session further down, which is the trap
  // components/Panel.tsx's filter box states its own scope about.
  const offered = await page.getByTestId("run-chain-select").evaluate(async (el) => {
    (el as HTMLElement).click();
    await new Promise((r) => setTimeout(r, 300));
    return [...document.querySelectorAll('[role="option"]')].map((o) => o.getAttribute("data-value") ?? "").filter((v) => v !== "");
  });
  expect(offered.length, "the picker offered nothing, so this test would prove nothing").toBeGreaterThan(0);

  const active = await page.evaluate(async () => {
    const res = await fetch("/api/palai/v1/sessions?status=active");
    const body = (await res.json()) as { data?: { id?: string }[] };
    return (body.data ?? []).map((r) => String(r.id));
  });
  expect(offered.filter((id) => !active.includes(id)), "the picker offers a session the server does not call active").toEqual([]);
});

test("a run pinned to a conversation is refused when that conversation is not active, and says so", async ({ page }) => {
  skipOnReal("DIV-UI-005");

  // A PAGE FIRST, because a relative `fetch` inside page.evaluate has no origin on about:blank and throws
  // "Failed to parse URL" — which is a test that fails for a reason unrelated to what it is testing.
  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });

  // THE REFUSAL PATH IS DRIVEN, NOT ASSUMED. A closed session is not offered by the picker — that is the
  // point of the filter — so the only way to reach the 409 is to ask for one directly, which is what a
  // stale browser tab does when a session is closed between the fetch and the click. If this stopped
  // returning 409 the picker's filter would be the only thing standing between an operator and a confusing
  // failure, and nothing would say so.
  const closed = await page.evaluate(async () => {
    const res = await fetch("/api/palai/v1/sessions");
    const body = (await res.json()) as { data?: { id?: string; status?: string }[] };
    return (body.data ?? []).find((r) => r.status !== "active")?.id ?? "";
  });
  expect(closed, "the fixture holds no non-active session, so this refusal cannot be driven").not.toBe("");

  const refusal = await page.evaluate(async (sessionId) => {
    const res = await fetch("/api/palai/stream", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt: "continue", session_id: sessionId }),
    });
    const text = await res.text();
    return { status: res.status, text };
  }, closed);

  // The relay streams, so the refusal arrives as an `error` frame rather than as an HTTP status.
  expect(refusal.text, `a run onto a non-active session was not refused: ${refusal.text.slice(0, 200)}`).toMatch(
    /session_conflict|only an active session/i,
  );
});
