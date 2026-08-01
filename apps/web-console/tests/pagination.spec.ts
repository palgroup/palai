import { expect, test } from "@playwright/test";

import { announceProfile, signIn, skipOnReal } from "./profile";

// LIST TRUNCATION IS VISIBLE (E25 T2, plan §3.6 D18 / feature list O12). This is a BUG FIX with a spec, not a
// feature: components/Panel.tsx read `body.data` and did not have `has_more` in its type at all, while
// api/pagination.go's defaultPageLimit is 20. So every admin list in this console — organizations, projects,
// keys, connections, routes, secret-refs, knowledge bases, agents, and after E25 the approval queue — showed
// at most twenty rows and said nothing. The twenty-first row did not appear to exist.
//
// THE BACKWARD ARROW EXISTS NOW AND THE API'S REFUSAL IS UNCHANGED, and the distinction between those two
// facts is what this file is really about (E31 shell parity).
//
// beginList REFUSES `?before=` with a 400 (apps/control-plane/api/pagination.go:179 — "backward pagination
// is not supported") and contracts.Page's `previous_cursor` is never populated by renderPage. This file used
// to conclude "so no backward CONTROL may exist" and assert the absence of a button. That conclusion was
// broader than its premise: the premise is about a REQUEST, and a control that never issues that request is
// not the control the API refuses.
//
// components/Panel.tsx's backward arrow is a REPLAY. The console remembers the `after` value it used to
// reach each page and re-issues that same FORWARD request to go back. So the properties that actually
// protect the API's refusal are the ones asserted below and they are strictly stronger than a missing
// button: no request ever carries `?before=`, every continuation carries a cursor the SERVER minted, page
// one is still the unparameterised read, and the backward arrow is DISABLED at the one position where going
// back would be meaningless.
//
// WHY THE CONTROL CHANGED AT ALL: "Load more" APPENDED, so page four was eighty rows in one scroll and
// "which page am I on" had no answer. Measured on the reference console 2026-08-01, a list ends in a `‹ ›`
// pair of 32x32 buttons at the bottom left and shows one page at a time.
test.beforeAll(() => announceProfile("pagination.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

test("a collection larger than one page shows 20 rows, SAYS more exist, and continues with ?after=", async ({ page }) => {
  // A collection LARGER THAN A PAGE is fixture-only: a fresh compose stack holds zero agents, and synthesising
  // twenty-one rows on demand is what the fake upstream is for (the same reasoning as the hostile artifact,
  // DIV-UI-003). The sweep re-derives that the real collection is short — it does not take this comment's word.
  skipOnReal("DIV-UI-005");

  // NOTHING BELOW NAMES A ROW OR A COUNT, and that is a repair rather than a loosening (E25 T6). This test
  // used to assert `agt_20` at row 19, `agt_21` after the continuation, and `?after=cur_20` — three fixture
  // constants that held only while the fixture's agents collection was frozen at twenty-one seeded rows.
  // E25 T6's console CREATES agents, tests/config-journey.spec.ts creates several, and Playwright runs one
  // shared fixture process for the whole suite — so those constants made this test's greenness a property of
  // WHICH FILE RAN FIRST. That is the exact trap E25 T4 already paid for once ("an assertion that held only by
  // file order"), and the assertions that replace them are stronger: the cut is 20 whatever the collection
  // holds, the continuation's cursor is the one the SERVER minted rather than a matching literal, and the
  // second page is DISJOINT from the first — which is the bug a re-fetch of page one would be.
  const requested: string[] = [];
  const cursors: (string | null)[] = [];
  page.on("request", (r) => {
    const url = new URL(r.url());
    if (url.pathname === "/api/palai/v1/agents") requested.push(url.search === "" ? "<first page>" : url.search);
  });
  page.on("response", (r) => {
    const url = new URL(r.url());
    if (url.pathname !== "/api/palai/v1/agents") return;
    void r
      .json()
      .then((body: { next_cursor?: string | null }) => cursors.push(body.next_cursor ?? null))
      .catch(() => {});
  });

  await page.goto("/");
  const panel = page.getByTestId("panel-agents");
  await expect(panel.locator("tbody tr")).toHaveCount(20, { timeout: 15_000 });

  // The truncation is stated IN TEXT — not a colour, not a disabled arrow, not silence. It names the PAGE
  // too, because with a replacing pager "20 rows" alone no longer means "the first 20".
  const pager = page.getByTestId("panel-agents-more");
  await expect(pager).toContainText(/more are available/i);
  await expect(pager).toContainText("Page 1");

  const rowIDs = async () => panel.locator("tbody tr td:first-child").allInnerTexts();
  const firstPage = await rowIDs();
  expect(new Set(firstPage).size, "page one served a duplicate row").toBe(20);

  await page.getByTestId("panel-agents-page-next").click();
  await expect(pager).toContainText("Page 2", { timeout: 15_000 });

  // THE SECOND PAGE IS NEW ROWS AND IT REPLACES THE FIRST. A pager that re-fetched page one would leave the
  // row COUNT identical, and a count-only assertion would call that a pass.
  const second = await rowIDs();
  expect(second.length, "page two is empty").toBeGreaterThan(0);
  expect(second.filter((id) => firstPage.includes(id)), "page two re-served rows page one already had").toEqual([]);

  // THE BACKWARD ARROW RETURNS THE SAME ROWS, which is what makes the replay a replay rather than a second
  // forward step wearing an arrow. It is the whole reason a backward control is allowed to exist here.
  await page.getByTestId("panel-agents-page-back").click();
  await expect(pager).toContainText("Page 1", { timeout: 15_000 });
  expect(await rowIDs(), "going back did not return the page it came from").toEqual(firstPage);

  // EXHAUSTION, driven rather than assumed: keep going forward while the arrow is enabled. The bound is
  // generous and finite — an unbounded loop here would hang instead of failing.
  for (let i = 0; i < 10; i++) {
    const next = page.getByTestId("panel-agents-page-next");
    if (await next.isDisabled()) break;
    const before = await rowIDs();
    await next.click();
    // Each step must CHANGE the rows; a click that changed nothing would otherwise spin the loop out.
    await expect
      .poll(async () => (await rowIDs()).join(","), { timeout: 15_000 })
      .not.toBe(before.join(","));
  }
  // Exhausted: the forward arrow is disabled and the notice says this is the last page, because has_more is
  // now false. The pager itself REMAINS, because the reader is on page 2+ and still needs the way back.
  await expect(page.getByTestId("panel-agents-page-next")).toBeDisabled({ timeout: 15_000 });
  await expect(pager).toContainText("the last page");

  // EVERY CONTINUATION CARRIED A CURSOR THE SERVER MINTED. The console must not compute an offset of its own:
  // a real cursor is an HMAC'd keyset position bound to the tenant (api/pagination.go encodeCursor), so a
  // client-side guess comes back 400 invalid_cursor. Checked against the `next_cursor` values that actually
  // arrived on this page's own responses, so a console that hardcoded the fixture's `cur_20` would fail.
  const served = new Set(cursors.filter((c): c is string => typeof c === "string" && c !== ""));
  const continuations = requested.filter((q) => q !== "<first page>");
  expect(continuations.length, "no continuation request was made").toBeGreaterThan(0);
  for (const query of continuations) {
    const carried = new URLSearchParams(query).get("after") ?? "";
    expect(served.has(carried), `the console asked for ?after=${carried}, which no response ever offered`).toBe(true);
  }
  // The first page is fetched more than once on this surface and that is measured, not overlooked: the Agents
  // panel reads /agents, and AgentDiff independently reads /agents to find an agent whose revisions it can
  // diff. Both are unparameterised reads through the relay; only the CONTINUATION is this test's subject.
  expect(requested.filter((q) => q === "<first page>").length).toBeGreaterThan(0);
});

test("no request ever carries ?before=, and on page one the backward arrow cannot be pressed", async ({ page }) => {
  const before: string[] = [];
  page.on("request", (r) => {
    if (new URL(r.url()).searchParams.has("before")) before.push(r.url());
  });

  await page.goto("/");
  await expect(page.getByTestId("panel-organizations")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("panel-agents").locator("tbody tr")).toHaveCount(20, { timeout: 15_000 });

  // THE POSITION WHERE A BACKWARD REQUEST WOULD BE MEANINGLESS IS THE POSITION WHERE THE CONTROL REFUSES.
  // This is the assertion that replaces "no backward control exists": the API's refusal is about a REQUEST,
  // and a disabled arrow on page one is a control that cannot issue it. It is also the honest reading of the
  // ceiling for a reader — an arrow that vanished and reappeared would move the other arrow under the cursor.
  await expect(page.getByTestId("panel-agents-page-back")).toBeDisabled();

  // AND THE ARROW IS PRESSED ANYWAY, which is the half an absence assertion could never do: a disabled
  // control that still fires would be a live `?before=` on the wire, and only driving it can say so.
  await page.getByTestId("panel-agents-page-back").click({ force: true });
  await page.waitForTimeout(250);

  // Forward, then back — the replay path, which is the ONLY way this console ever reaches an earlier page.
  await page.getByTestId("panel-agents-page-next").click();
  await expect(page.getByTestId("panel-agents-more")).toContainText("Page 2", { timeout: 15_000 });
  await page.getByTestId("panel-agents-page-back").click();
  await expect(page.getByTestId("panel-agents-more")).toContainText("Page 1", { timeout: 15_000 });

  // An absence assertion over the WHOLE surface, on BOTH profiles, and now over a run in which a backward
  // control was actually operated three times. A `?before=` would 400 against the real API.
  expect(before, "a request carried ?before=, which apps/control-plane/api/pagination.go answers with a 400").toEqual([]);
});
