import { expect, test, type Page } from "@playwright/test";

import { IS_REAL } from "./constants";
import { announceProfile, runToTerminal, sessionHeaders, signIn } from "./profile";

// E25 T8 — THE OBSERVABILITY HALF. Six screens from feature list §7, every one on a /v1 route that already
// existed and none of which the console showed: O1 the run list, O2 a PAST run's timeline, O4 usage + the
// settled ledger, O5 budget + quota status, O6 the artifact browser, O9 the capability table.
//
// WHAT THIS FILE IS FOR, and it is not "the pages render": axe already scans all three routes (lib/routes.ts
// generates one scan per row) and the a11y coverage test already fails if one is missed. What is proven here
// is the part a scan cannot see — that each screen shows what the API ACTUALLY returns, that a screen with
// nothing to show says so in a SENTENCE rather than going blank, and that a read surface offers no write.
//
// EVERY TEST HERE RUNS ON BOTH PROFILES. There is not one skipOnReal in this file, and that is a deliberate
// consequence of what these screens are: a read of routes a bootstrap stack mounts and answers. The one
// difference between the profiles is how MUCH there is to read, and every assertion below is written to be
// true of both a rich fixture and a thin real stack — which is exactly the property DIV-UI-002 says the
// console has to have, rather than a property a fixture lends it.
test.beforeAll(() => announceProfile("observability.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// firstRunRow selects the newest run in the history list and returns its id, the id the row itself carries.
// Newest-first is the REAL order (storage/queries/responses.sql ListResponses: `ORDER BY created_at DESC,
// id DESC`), so the run this suite just drove is row one on both profiles — nothing here depends on a
// fixture constant or on which spec file ran first, which is the trap E25 T6 paid for in pagination.spec.ts.
async function selectNewestRun(page: Page): Promise<string> {
  const panel = page.getByTestId("panel-runs");
  await expect(panel.locator("tbody tr").first()).toBeVisible({ timeout: 30_000 });
  // THE ID COMES OFF THE ROW'S OWN ATTRIBUTE, not out of the first cell's text (page-parity pass). That cell
  // used to hold the whole 36-character response id; it holds the SHORT form now — a readable prefix, an
  // ellipsis and enough tail to tell two rows apart — so reading it would hand every assertion below a
  // truncated string and `toContainText(runID)` would compare against an ellipsis. A DRIVER edit: what this
  // file asserts is untouched.
  const open = panel.locator("tbody tr").first().getByTestId("run-open");
  const id = (await open.getAttribute("data-run-id")) ?? "";
  await open.click();
  // The selection is in the URL now, so waiting for it is waiting for the click to have landed rather than
  // for a re-render that may not have happened yet.
  await expect(page).toHaveURL(new RegExp(`run=${id}`));
  return id;
}

// --- O1 + O2 + O6 -----------------------------------------------------------------------------------------

test("a finished run is READABLE from the history list, and opening it replays the journal it actually wrote", async ({ page }) => {
  // The run is created through the console's own live surface, on whichever profile is running — so what the
  // history screen then reads is a run this suite genuinely produced, not a seeded row.
  await runToTerminal(page);

  // EVERY REQUEST THIS SCREEN MAKES, captured. The claim is not only that a timeline appears: it is that the
  // timeline came from re-reading the session's canonical event journal over the /v1 relay, which is the one
  // route O2 names. A page that had cached the live frames would render an identical timeline and prove
  // nothing, and this is what tells the two apart.
  const relayPaths: string[] = [];
  page.on("request", (r) => {
    const url = new URL(r.url());
    if (url.pathname.startsWith("/api/palai/")) relayPaths.push(url.pathname);
  });

  await page.goto("/history");
  const runID = await selectNewestRun(page);
  expect(runID, "the history list rendered a row with no response id in its first column").not.toBe("");

  // O1 — the DETAIL read. The list row carries only the durable columns (ListResponses selects id, state,
  // session_id, created_at); model, usage and output come from GET /v1/responses/{id}, which is the second
  // route O1 names and the reason opening a run is a read rather than an expansion of what the list held.
  await expect(page.getByTestId("run-detail")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("run-detail")).toContainText(runID);
  await expect(page.getByTestId("run-detail-status")).not.toBeEmpty();

  // O2 — the journal, re-read. The last event of a finished run is a TERMINAL-lane one on both profiles:
  // a compose run journals run.completed.v1 and the fixture's script ends on it, and laneFor puts both in
  // the same lane. That is the assertion, rather than a count or an event name a profile could differ on.
  const timeline = page.getByTestId("past-run-timeline");
  await expect(timeline.locator("li").first()).toBeVisible({ timeout: 30_000 });
  await expect(timeline.locator("li[data-lane='terminal']")).toHaveCount(1, { timeout: 30_000 });
  const lanes = await timeline.locator("li").evaluateAll((els) => els.map((e) => e.getAttribute("data-lane") ?? ""));
  expect(lanes.length, "the replayed timeline rendered no events").toBeGreaterThan(1);
  expect(lanes.at(-1), "the last replayed event was not the run's terminal").toBe("terminal");

  const eventsReads = relayPaths.filter((p) => /^\/api\/palai\/v1\/sessions\/[^/]+\/events$/.test(p));
  expect(eventsReads.length, `the timeline rendered without reading the journal — relay paths: ${relayPaths.join(", ")}`).toBeGreaterThan(0);
  expect(
    relayPaths.filter((p) => p === "/api/palai/stream"),
    "the history screen started a RUN to show a past one — reading history must never dispatch",
  ).toEqual([]);

  // O6 — the artifact browser, and it is asserted as a DISJUNCTION because the two profiles genuinely differ
  // in what a run leaves behind (DIV-UI-002: a compose run journals no artifact lane at all). Both arms
  // assert; neither is a skip. What is common to them — and is the actual claim — is that this panel is never
  // a blank region, and that the only thing it can offer is the HARDENED relay download.
  const artifacts = page.getByTestId("panel-run-artifacts");
  await expect(artifacts).toBeVisible();
  const artifactRows = await artifacts.locator("tbody tr").count();
  if (artifactRows === 0) {
    await expect(page.getByTestId("panel-run-artifacts-empty")).toContainText(/produced no files/i);
  } else {
    const hrefs = await artifacts.locator("a").evaluateAll((els) => els.map((e) => e.getAttribute("href") ?? ""));
    expect(hrefs.length).toBeGreaterThan(0);
    for (const href of hrefs) {
      expect(href, "an artifact link addressed something other than the hardened relay download").toMatch(
        /^\/api\/palai\/v1\/artifacts\/[^/]+\/content$/,
      );
    }
  }
  // eslint-disable-next-line no-console -- which arm ran is the evidence; a disjunction whose branch is invisible is a coin toss.
  console.log(`RUN ARTIFACTS — profile=${IS_REAL ? "real" : "fake"}, run ${runID} carries ${artifactRows} artifact row(s)`);
});

test("the history screen offers no way to START, CANCEL or DELETE a run — it is a read surface", async ({ page }) => {
  await page.goto("/history");
  // THE LIST MUST HAVE LOADED BEFORE ANYTHING IS COUNTED. `panel-runs` becomes visible while it still says
  // "Loading…", and an enumeration taken then finds zero controls and passes an absence assertion having
  // looked at a spinner — which is the same vacuity the a11y scan's readyTestId exists to prevent. So the
  // rows are waited for, and the enumeration is then asserted to be NON-EMPTY: the surface must be there for
  // its absences to mean anything.
  await expect(page.getByTestId("panel-runs").locator("tbody tr").first()).toBeVisible({ timeout: 30_000 });

  // An ABSENCE, asserted where the claim is an absence. POST /v1/responses/{id}/cancel is mounted and the
  // relay would happily forward it, so "this screen cannot cancel" is a property of the SCREEN and has to be
  // checked on the screen. The enumeration is printed so a future control that quietly appears is visible.
  const controls = await page.locator("button, input[type='submit']").evaluateAll((els) => els.map((e) => (e.textContent ?? "").trim()));
  // eslint-disable-next-line no-console -- the enumeration is the evidence.
  console.log(`HISTORY CONTROLS — ${controls.length}: ${controls.join(" | ")}`);
  expect(controls.length, "no control was enumerated at all — this absence assertion looked at an empty page").toBeGreaterThan(0);
  // THE POSITIVE CONTROL, BY TESTID RATHER THAN BY LABEL (page-parity pass). It looked for a button whose
  // text was exactly "Open" — a column of its own holding one word — and the row's opening control is now the
  // id cell itself, so the label it matched no longer exists. What this line is FOR is unchanged and is now
  // checked more tightly: the enumeration must have reached the list, which means one opening control per
  // rendered row rather than merely one somewhere on the page.
  const openControls = await page.getByTestId("run-open").count();
  const renderedRows = await page.getByTestId("panel-runs").locator("tbody tr").count();
  expect(openControls, "the run list rendered no opening control, so the enumeration missed the list").toBeGreaterThan(0);
  expect(openControls, "the enumeration saw a different number of rows than the list rendered").toBe(renderedRows);
  expect(controls.filter((t) => /cancel|delete|remove|start|retry/i.test(t))).toEqual([]);

  const writes: string[] = [];
  page.on("request", (r) => {
    if (r.method() !== "GET" && new URL(r.url()).pathname.startsWith("/api/palai/")) writes.push(`${r.method()} ${new URL(r.url()).pathname}`);
  });
  await selectNewestRun(page).catch(() => "");
  await page.waitForTimeout(1_000);
  expect(writes, "the history screen sent a non-GET through the relay").toEqual([]);
});

// --- O4 / O5 ----------------------------------------------------------------------------------------------

test("the metering screens state what they hold — and an empty one is a SENTENCE, never a blank region", async ({ page }) => {
  await page.goto("/usage");

  // FOUR PANELS, FOUR ROUTES: GET /v1/usage (meters), /v1/usage/ledger, /v1/budgets, /v1/quotas. Each is
  // asserted to be in exactly ONE of its two legitimate states — rows, or a named empty sentence — because
  // the third state, a rendered panel with neither, is the failure this test exists to catch. On a fresh
  // stack every one of the four is empty (DIV-UI-002's thin-surface reality), so on the real profile this is
  // the assertion doing all the work.
  const panels = [
    { id: "panel-usage-meters", empty: /has metered nothing/i },
    { id: "panel-usage-ledger", empty: /no settled entries/i },
    { id: "panel-budgets", empty: /no budget/i },
    { id: "panel-quotas", empty: /no quota/i },
  ];
  const states: string[] = [];
  for (const p of panels) {
    const panel = page.getByTestId(p.id);
    await expect(panel).toBeVisible({ timeout: 30_000 });
    // Never still loading when the assertion is made: a spinner is neither of the two states and would let
    // this pass over a panel that had rendered nothing at all.
    await expect(page.getByTestId(`${p.id}-loading`)).toHaveCount(0, { timeout: 30_000 });
    const rows = await panel.locator("tbody tr").count();
    if (rows === 0) {
      await expect(page.getByTestId(`${p.id}-empty`)).toContainText(p.empty);
      // The sentence is not a shrug: it says what an operator can DO about it, which for every one of these
      // four is "nothing, from here" — see the write-absence test below.
      expect((await page.getByTestId(`${p.id}-empty`).innerText()).length, `${p.id} rendered an empty state of two words`).toBeGreaterThan(40);
    } else {
      await expect(page.getByTestId(`${p.id}-empty`)).toHaveCount(0);
    }
    states.push(`${p.id}=${rows === 0 ? "empty-sentence" : `${rows} row(s)`}`);
  }
  // eslint-disable-next-line no-console -- which state each panel was in is the evidence, on both profiles.
  console.log(`METERING PANELS — profile=${IS_REAL ? "real" : "fake"}: ${states.join(", ")}`);

  // AND THE EMPTY ARM IS PINNED, ON BOTH PROFILES, so this test can never pass having exercised only the
  // populated one. Budgets and quotas are empty everywhere this suite runs and that is a fact rather than a
  // fixture choice: nothing on a bootstrap stack creates either, there is no CLI verb for either, and the
  // fake upstream deliberately serves the same emptiness (tests/fake-control-plane.mjs says why). If a
  // deployment ever does hold a limit, this fails loudly and the claim gets rewritten — which is the correct
  // outcome, and the opposite of a disjunction quietly taking whichever branch was available.
  expect(states, "neither limit collection rendered its empty SENTENCE, so the honestly-empty claim was never exercised").toEqual(
    expect.arrayContaining(["panel-budgets=empty-sentence", "panel-quotas=empty-sentence"]),
  );
});

test("the metering screen is the READ half only — no control sets a budget or a quota, and it says so", async ({ page }) => {
  await page.goto("/usage");
  // Every panel must have RESOLVED before an absence is counted: a page still showing four spinners has no
  // forms either, and would pass this test having rendered nothing. There is no "the controls are non-empty"
  // guard available here — this screen legitimately has zero buttons — so the guard is that all four panels
  // finished, which is what makes the absence a statement about the rendered surface.
  for (const id of ["panel-usage-meters", "panel-usage-ledger", "panel-budgets", "panel-quotas"]) {
    await expect(page.getByTestId(id)).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId(`${id}-loading`)).toHaveCount(0, { timeout: 30_000 });
  }

  // POST /v1/budgets and POST /v1/quotas are mounted, gated on the `provision` capability the console's key
  // holds, and the relay would forward either. So "you cannot set a limit here" is a claim about this SCREEN
  // and is checked on the screen: no form, no number input, no submit.
  await expect(page.locator("form")).toHaveCount(0);
  await expect(page.locator("input")).toHaveCount(0);
  const controls = await page.locator("button").evaluateAll((els) => els.map((e) => (e.textContent ?? "").trim()));
  // eslint-disable-next-line no-console -- the enumeration is the evidence.
  console.log(`USAGE CONTROLS — ${controls.length}: ${controls.join(" | ") || "<none>"}`);
  expect(controls.filter((t) => /set|save|create|edit|update|limit/i.test(t))).toEqual([]);

  // And the ceiling is WRITTEN, because an operator who cannot find the control needs to know whether it is
  // missing or whether they are missing it.
  await expect(page.getByTestId("usage-ceiling")).toContainText(/read/i);
});

// --- O9 ---------------------------------------------------------------------------------------------------

test("the capability table shows the tiers the API returned — recomputed from the API's own answer, not typed here", async ({ page }) => {
  await page.goto("/capabilities");
  await expect(page.getByTestId("panel-capabilities")).toBeVisible({ timeout: 30_000 });

  // THE TABLE IS DIFFED AGAINST THE ROUTE ITSELF, and that is the whole point of this test. A hardcoded
  // expectation ("console: preview") would pass on a deployment that advertises something else entirely, and
  // the tiers here are exactly the kind of thing a console must not have an opinion about: `a2a`, `slack`,
  // `queues`, `knowledge` and `capability-workers` are advertised ONLY where the binary MOUNTED them
  // (api/capabilities.go), so the set differs between deployments by design.
  const served = (await (await page.request.get("/api/palai/v1/capabilities", { headers: await sessionHeaders(page) })).json()) as {
    capabilities: Record<string, string>;
    maturity: string;
    isolation: string;
  };
  const expected = Object.entries(served.capabilities)
    .map(([name, tier]) => `${name}=${tier}`)
    .sort();
  const rendered = (
    await page.getByTestId("panel-capabilities").locator("tbody tr").evaluateAll((rows) =>
      rows.map((r) => {
        const cells = r.querySelectorAll("td");
        return `${(cells[0]?.textContent ?? "").trim()}=${(cells[1]?.textContent ?? "").trim()}`;
      }),
    )
  ).sort();
  // eslint-disable-next-line no-console -- the served matrix is the evidence, and it differs per deployment.
  console.log(`CAPABILITY MATRIX — profile=${IS_REAL ? "real" : "fake"}: ${rendered.join(", ")}`);
  expect(rendered, "the console re-derived, dropped or prettified a tier the API returned").toEqual(expected);
  expect(rendered.length).toBeGreaterThan(0);

  // The deployment posture rides the same body and is shown rather than dropped: `maturity` and `isolation`
  // are what tell an operator this is a development-isolation deployment, and they are the two fields
  // `palai up` prints above the same table.
  await expect(page.getByTestId("panel-deployment-posture")).toContainText(served.maturity);
  await expect(page.getByTestId("panel-deployment-posture")).toContainText(served.isolation);
});
