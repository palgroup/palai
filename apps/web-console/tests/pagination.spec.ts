import { expect, test } from "@playwright/test";

import { announceProfile, signIn, skipOnReal } from "./profile";

// LIST TRUNCATION IS VISIBLE (E25 T2, plan §3.6 D18 / feature list O12). This is a BUG FIX with a spec, not a
// feature: components/Panel.tsx read `body.data` and did not have `has_more` in its type at all, while
// api/pagination.go's defaultPageLimit is 20. So every admin list in this console — organizations, projects,
// keys, connections, routes, secret-refs, knowledge bases, agents, and after E25 the approval queue — showed
// at most twenty rows and said nothing. The twenty-first row did not appear to exist.
//
// THERE IS NO "PREVIOUS" CONTROL, AND THE REASON IS A MEASUREMENT RATHER THAN A SIMPLIFICATION: beginList
// REFUSES `?before=` with a 400 (apps/control-plane/api/pagination.go:179 — "backward pagination is not
// supported"), and contracts.Page's `previous_cursor` is never populated by renderPage. A back button would be
// a control that cannot work against the real API, so its ABSENCE is asserted here rather than left to chance.
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

  // The truncation is stated IN TEXT — not a colour, not a disabled arrow, not silence.
  await expect(page.getByTestId("panel-agents-more")).toContainText(/20 .*more/i);

  const rowIDs = async () => panel.locator("tbody tr td:first-child").allInnerTexts();
  const firstPage = await rowIDs();
  expect(new Set(firstPage).size, "page one served a duplicate row").toBe(20);

  await page.getByTestId("panel-agents-load-more").click();
  await expect(panel.locator("tbody tr")).not.toHaveCount(20, { timeout: 15_000 });

  // THE SECOND PAGE IS NEW ROWS. A continuation that re-fetched page one would append twenty rows the list
  // already had, and a count-only assertion would call that a pass.
  const afterFirstClick = await rowIDs();
  expect(afterFirstClick.slice(0, 20), "the continuation reordered or replaced page one").toEqual(firstPage);
  const appended = afterFirstClick.slice(20);
  expect(appended.length, "the continuation appended nothing").toBeGreaterThan(0);
  expect(appended.filter((id) => firstPage.includes(id)), "the continuation re-served rows page one already had").toEqual([]);

  // EXHAUSTION, driven rather than assumed: keep continuing while the control exists. The bound is generous
  // and finite — an unbounded loop here would hang instead of failing.
  for (let i = 0; i < 10; i++) {
    const control = page.getByTestId("panel-agents-load-more");
    if ((await control.count()) === 0) break;
    const before = await panel.locator("tbody tr").count();
    await control.click();
    // Each continuation must GROW the list; a click that changed nothing would otherwise spin the loop out.
    await expect(panel.locator("tbody tr")).not.toHaveCount(before, { timeout: 15_000 });
  }
  // Exhausted: the control is gone and so is the truncation notice, because has_more is now false.
  await expect(page.getByTestId("panel-agents-load-more")).toHaveCount(0, { timeout: 15_000 });
  await expect(page.getByTestId("panel-agents-more")).toHaveCount(0);

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

test("no list renders a 'previous' control — backward pagination is refused by the API, so it is not offered", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("panel-organizations")).toBeVisible({ timeout: 15_000 });

  // An absence assertion over the WHOLE admin surface, on BOTH profiles: no control anywhere offers a
  // backward page, and no request ever carries ?before=. A `previous` button would 400 against the real API.
  const before: string[] = [];
  page.on("request", (r) => {
    if (new URL(r.url()).searchParams.has("before")) before.push(r.url());
  });
  await expect(page.getByRole("button", { name: /previous|back|prev\b/i })).toHaveCount(0);
  await expect(page.locator("[data-testid$='-load-previous']")).toHaveCount(0);
  expect(before).toEqual([]);
});
