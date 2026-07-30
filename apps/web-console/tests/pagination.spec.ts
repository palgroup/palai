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

  const requested: string[] = [];
  page.on("request", (r) => {
    const url = new URL(r.url());
    if (url.pathname === "/api/palai/v1/agents") requested.push(url.search === "" ? "<first page>" : url.search);
  });

  await page.goto("/");
  const panel = page.getByTestId("panel-agents");
  await expect(panel.locator("tbody tr")).toHaveCount(20, { timeout: 15_000 });

  // The truncation is stated IN TEXT — not a colour, not a disabled arrow, not silence.
  await expect(page.getByTestId("panel-agents-more")).toContainText(/20 .*more/i);

  // The 21st row does not exist yet, and the last row of page one is the 20th.
  await expect(panel).not.toContainText("agt_21");
  await expect(panel.locator("tbody tr").nth(19)).toContainText("agt_20");

  await page.getByTestId("panel-agents-load-more").click();

  await expect(panel.locator("tbody tr")).toHaveCount(21, { timeout: 15_000 });
  await expect(panel).toContainText("agt_21");
  // Exhausted: the control goes away and so does the truncation notice, because has_more is now false.
  await expect(page.getByTestId("panel-agents-load-more")).toHaveCount(0);
  await expect(page.getByTestId("panel-agents-more")).toHaveCount(0);

  // The continuation carried the SERVER's cursor, and there was EXACTLY ONE of them. The console must not
  // compute an offset of its own: a real cursor is an HMAC'd keyset position bound to the tenant
  // (api/pagination.go encodeCursor), so a client-side guess comes back 400 invalid_cursor.
  //
  // The first page is fetched TWICE on this surface and that is measured, not overlooked: the Agents panel
  // reads /agents, and AgentDiff independently reads /agents to find an agent whose revisions it can diff.
  // Both are unparameterised reads through the relay; only the CONTINUATION is this test's subject.
  expect(requested.filter((q) => q !== "<first page>")).toEqual(["?after=cur_20"]);
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
