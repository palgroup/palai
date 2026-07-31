import { expect, test, type Page } from "@playwright/test";

import { shortId } from "../components/Session";
import { compactTokens, LANE_LABEL, type SessionRow } from "../lib/sessions";
import { laneFor } from "../lib/timeline";
import { NEXT_PORT } from "./constants";
import { announceProfile, chooseOption, chooseOptionByLabel, sessionHeaders, signIn, skipOnReal } from "./profile";

// THE TWO SESSION SCREENS, DRIVEN THROUGH THE BROWSER (E29).
//
// EVERY ASSERTION HERE IS ABOUT WHAT IS ON SCREEN, and that is a correction rather than a style. This repo
// shipped four defects in one day behind tests that proved an ENDPOINT while no test ever submitted the form
// in front of it — an auth suite that drove /api/console/login with a leak sitting in the gap. So the reads
// below fetch the API's own answer only to CROSS-CHECK the rendering: the API is never the subject, it is
// the expected value.
//
// The API's answer is fetched through the CONSOLE'S RELAY with the browser's own session cookie, so even the
// cross-check travels the public-API-only path and no spec here ever holds a credential.
test.beforeAll(() => announceProfile("sessions.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

const ORIGIN = `http://127.0.0.1:${NEXT_PORT}`;

/** served reads a /v1 collection through the relay — the expected value the screen is judged against. */
async function served(page: Page, path: string): Promise<{ data: SessionRow[]; has_more?: boolean }> {
  const res = await page.request.get(`${ORIGIN}/api/palai/v1${path}`, { headers: await sessionHeaders(page) });
  expect(res.status(), `GET ${path} through the relay`).toBe(200);
  return (await res.json()) as { data: SessionRow[]; has_more?: boolean };
}

/** openList lands on the Sessions screen and waits for the panel to hold rows rather than a spinner. */
async function openList(page: Page) {
  await page.goto("/sessions");
  await expect(page.getByTestId("panel-sessions")).toBeVisible({ timeout: 15_000 });
}

/**
 * openTranscript lands on one session's transcript and waits for the JOURNAL TO FINISH ARRIVING.
 *
 * `networkidle` is the honest settle signal here rather than a row count or a timeout: the journal is an SSE
 * stream, so the connection stays open for exactly as long as frames are still coming and closes on the
 * terminal one (api/events.go's pump returns after it). Asserting over a half-arrived transcript is how a
 * count assertion becomes a race.
 */
async function openTranscript(page: Page, id: string) {
  await page.goto(`/sessions/${encodeURIComponent(id)}`);
  await expect(page.getByTestId("session-transcript")).toBeVisible({ timeout: 15_000 });
  await page.waitForLoadState("networkidle");
}

/** The nth cell of every row of a table, in document order — one column, read down. */
const column = (page: Page, testId: string, n: number) => page.getByTestId(testId).locator(`tbody tr td:nth-child(${String(n)})`);

// --- THE LIST ------------------------------------------------------------------------------------------

test("every column on the Sessions list is the field it is named after", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  expect(data.length, "the sessions collection is empty, so this test would assert over nothing").toBeGreaterThan(0);
  await openList(page);

  const rows = page.getByTestId("panel-sessions").locator("tbody tr");
  await expect(rows).toHaveCount(data.length);

  // Row one, cell by cell, against the row the API actually served. A column rendering a CONSTANT would pass
  // a "the table has six columns" test and fails here.
  const first = data[0];
  const cells = rows.first().locator("td");

  // THE ID CELL IS A SHORT FORM, and the short form is DERIVED from the id rather than being a constant:
  // its head is a prefix of the real id and its tail is a suffix. A cell rendering a fixed placeholder would
  // pass "the column is not empty" and fails here. The WHOLE value stays reachable — in `title`, in the
  // link's href, and on the clipboard.
  const link = cells.nth(0).getByTestId("session-link");
  await expect(link).toHaveAttribute("title", first.id);
  await expect(link).toHaveAttribute("href", `/sessions/${first.id}`);
  const shown = (await link.textContent()) ?? "";
  expect(shown).toBe(shortId(first.id));
  const [head, tail] = shown.split("…");
  expect(first.id.startsWith(head), `${shown} is not a clipping of ${first.id}`).toBe(true);
  expect(first.id.endsWith(tail)).toBe(true);
  expect(shown.length).toBeLessThan(first.id.length);

  // The NAME is a link to the same place — measured: whichever cell the operator aims at, they arrive.
  if (first.name !== "") await expect(cells.nth(1)).toContainText(first.name);
  await expect(cells.nth(1).getByTestId("session-name-link")).toHaveAttribute("href", `/sessions/${first.id}`);
  await expect(cells.nth(2)).toContainText(first.status);
  await expect(cells.nth(4)).toHaveText(`${compactTokens(first.input_tokens)} / ${compactTokens(first.output_tokens)}`);
  // The Created cell is BOTH forms: the relative one a reader sees, and the machine one in `datetime`.
  await expect(cells.nth(5).locator("time")).toHaveAttribute("datetime", first.created_at);
  await expect(cells.nth(5)).toHaveText(/ago|just now|in /);

  // THE AGENTS CELL, IN BOTH DIRECTIONS, AND THE EMPTY ONE IS THE ASSERTION THAT MATTERS.
  //
  // Measured on the live control plane 2026-07-31: `GET /v1/sessions?limit=100` answers 62 sessions with
  // has_more false, and only 3 carry any agent — this column is empty on 95% of real rows. The cause is
  // structural (response-create.json declares agent_revision_id and no agent_id; `runs` has no
  // agent_profile_id), so the empty cell has to name the MECHANISM rather than shrug: an em dash alone in a
  // column an operator expects to be full reads as a rendering fault in this screen.
  //
  // The exact words are pinned. A later edit softening them to a bare dash, or "backfilling" the cell from
  // the agent a picker was set to, fails here.
  const noAgent = data.findIndex((r) => (r.agents ?? []).length === 0);
  if (noAgent !== -1) await expect(rows.nth(noAgent).locator("td").nth(3)).toHaveText("— no revision pinned");
  const withAgent = data.findIndex((r) => (r.agents ?? []).length > 0);
  if (withAgent !== -1) await expect(rows.nth(withAgent).locator("td").nth(3)).toContainText(data[withAgent].agents[0]);

  // PLURAL, and session.json is explicit that the singular would be a claim the data does not make: there is
  // no session-to-agent association, only an aggregate over runs, so a session may carry several.
  await expect(page.getByTestId("panel-sessions").locator("thead th").nth(3)).toHaveText("Agents");
});

test("a derived label is marked as one, because nobody chose it", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openList(page);
  const rows = page.getByTestId("panel-sessions").locator("tbody tr");

  const derived = data.findIndex((r) => r.name_source === "derived");
  const operator = data.findIndex((r) => r.name_source === "operator");
  const none = data.findIndex((r) => r.name_source === "none");
  expect(
    derived !== -1 || operator !== -1,
    "no row carries a name_source this run can distinguish, so the marker would be unfalsifiable here",
  ).toBe(true);

  // The marker is the DISTINCTION, so it is asserted in both directions: present on a cut of the first
  // prompt, absent on a label a human typed. One direction alone would pass over a marker on every row.
  if (derived !== -1) await expect(rows.nth(derived).locator(".name-mark")).toHaveCount(1);
  if (operator !== -1) await expect(rows.nth(operator).locator(".name-mark")).toHaveCount(0);
  // A session with neither says so in words rather than rendering an empty cell.
  if (none !== -1) await expect(rows.nth(none).locator("td").nth(1)).toContainText("unnamed");
});

test("the search box is scoped to the ID, and proves it by not finding a name that is on screen", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  const named = data.find((r) => r.name.trim() !== "");
  expect(named, "no row has a name, so 'the box does not search names' would be unfalsifiable").not.toBe(undefined);
  await openList(page);

  // The name IS on screen — asserted first, so the miss below is a miss about the BOX and not about the row.
  await expect(page.getByTestId("panel-sessions")).toContainText(named?.name ?? "");
  await page.getByTestId("panel-sessions-filter").fill(named?.name ?? "");
  await expect(page.getByTestId("panel-sessions-no-match")).toBeVisible();

  await page.getByTestId("panel-sessions-filter").fill(data[0].id);
  await expect(page.getByTestId("panel-sessions").locator("tbody tr")).toHaveCount(1);
  await expect(page.getByTestId("panel-sessions").getByTestId("session-link")).toHaveAttribute("title", data[0].id);
});

// THE DROPDOWN IS OPERABLE WITH NO MOUSE, AND IT IS THE HEADLINE OF THE COMPONENT LAYER SO IT IS MEASURED
// RATHER THAN CITED (E29).
//
// The seven native <select>s became components/ui/Select over @base-ui/react, and every other test in this
// file drives one by CLICKING — tests/profile.ts's chooseOption clicks the trigger and clicks a row. That
// proves the popup opens and the value lands; it proves nothing at all about the keyboard, which is exactly
// what a hand-rolled dropdown loses first and what a headless library is adopted FOR. A claim that the
// library "supplies keyboard navigation" is a citation, and this repository does not accept citations for
// behaviour on the shipped page.
//
// So this drives the whole interaction from the keyboard: Tab to the control, open it, move within the
// listbox, commit, and dismiss the next one with Escape. The ROLES are asserted alongside, because they are
// what a screen reader reads and they were absent from this console entirely before — measured on d8ca934b,
// `grep -rn 'role="listbox"' components app` → 0.
test("the status dropdown opens, moves and commits from the keyboard alone, and Escape abandons", async ({ page }) => {
  await openList(page);
  const trigger = page.getByTestId("session-status-filter");

  // THE ROLES FIRST. A <button> that merely looks like a dropdown announces itself as a button; these three
  // attributes are what make a screen reader say "combo box, collapsed" and then read the list.
  await expect(trigger).toHaveJSProperty("tagName", "BUTTON");
  await expect(trigger).toHaveAttribute("role", "combobox");
  await expect(trigger).toHaveAttribute("aria-haspopup", "listbox");
  await expect(trigger).toHaveAttribute("aria-expanded", "false");

  // GENUINELY REACHED BY Tab, not by .focus() — which succeeds on a tabindex=-1 element Tab can never reach.
  // The same rule tests/a11y.spec.ts's tabToTestId follows, and the reason it exists.
  for (let i = 0; i < 40 && !(await trigger.evaluate((el) => el === document.activeElement)); i++) {
    await page.keyboard.press("Tab");
  }
  await expect(trigger, "Tab never reached the status dropdown — it is not keyboard-reachable").toBeFocused();

  await page.keyboard.press("Enter");
  const listbox = page.getByRole("listbox");
  await expect(listbox).toBeVisible();
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  const options = listbox.getByRole("option");
  await expect(options).not.toHaveCount(0);

  // ARROW MOVES THE HIGHLIGHT WITHOUT COMMITTING, which is the half a click can never exercise: the control
  // still reads its old value while the reader walks the list.
  const before = await trigger.getAttribute("data-value");
  await page.keyboard.press("ArrowDown");
  await expect(listbox.locator('[role="option"][data-highlighted]')).toHaveCount(1);
  expect(await trigger.getAttribute("data-value"), "arrowing through the list changed the value before it was committed").toBe(before);

  // ENTER COMMITS the highlighted row, and the control closes carrying it.
  const highlighted = await listbox.locator('[role="option"][data-highlighted]').getAttribute("data-value");
  await page.keyboard.press("Enter");
  await expect(listbox).toHaveCount(0);
  await expect(trigger).toHaveAttribute("data-value", String(highlighted));
  await expect(trigger).toHaveAttribute("aria-expanded", "false");

  // AND ESCAPE ABANDONS rather than committing. A dropdown an operator cannot back out of with the keyboard
  // is one they will back out of by reloading the page.
  const committed = await trigger.getAttribute("data-value");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("listbox")).toBeVisible();
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  expect(await trigger.getAttribute("data-value"), "Escape committed the highlighted row instead of abandoning it").toBe(committed);
  // Focus comes BACK to the control it left, so the reader is not returned to the top of the document.
  await expect(trigger).toBeFocused();
});

test("the Status control narrows the COLLECTION on the server, not the rows on screen", async ({ page }) => {
  const { data: all } = await served(page, "/sessions");
  const target = all.find((r) => r.status !== all[0].status)?.status ?? all[0].status;
  const { data: expected } = await served(page, `/sessions?status=${encodeURIComponent(target)}`);
  await openList(page);

  // The REQUEST is the claim. A client-side filter would render the same rows and send nothing, so the wire
  // is what separates "narrowed the collection" from "hid some of what was already here".
  const [request] = await Promise.all([
    page.waitForRequest((r) => r.url().includes("/api/palai/v1/sessions?") && r.url().includes(`status=${encodeURIComponent(target)}`)),
    chooseOption(page, "session-status-filter", target),
  ]);
  expect(new URL(request.url()).searchParams.get("status")).toBe(target);

  const rows = page.getByTestId("panel-sessions").locator("tbody tr");
  await expect(rows).toHaveCount(expected.length);
  for (const chip of await page.getByTestId("session-status").all()) await expect(chip).toContainText(target);
});

test("the Created control sends an RFC3339 lower bound the API actually parses", async ({ page }) => {
  await openList(page);
  const [request] = await Promise.all([
    page.waitForRequest((r) => r.url().includes("/api/palai/v1/sessions?") && r.url().includes("created_after=")),
    chooseOption(page, "session-created-filter", "24h"),
  ]);
  const bound = new URL(request.url()).searchParams.get("created_after") ?? "";
  // Parseable AND in the past AND roughly a day back: a bound the server rejects is a filter that 400s, and
  // a bound of "now" is a filter that empties the list.
  const parsed = Date.parse(bound);
  expect(Number.isNaN(parsed), `created_after=${bound} is not a timestamp`).toBe(false);
  expect(Date.now() - parsed).toBeGreaterThan(23 * 3_600_000);
  expect(Date.now() - parsed).toBeLessThan(25 * 3_600_000);
  // The screen must still be a screen afterwards — a 400 would render the panel's error region instead.
  await expect(page.getByTestId("panel-sessions-error")).toHaveCount(0);
});

test("the Agent control narrows what is on screen, and the count says both numbers", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  const total = data.length;
  const without = data.filter((r) => (r.agents ?? []).length === 0).length;
  expect(without > 0 && without < total, "every session has the same agent-ness, so a narrowing cannot be observed").toBe(true);
  await openList(page);

  // The row's words are the cell's words — they select the same rows, so they must not drift apart. Read
  // by OPENING the listbox rather than off the closed control: a trigger shows the CURRENT choice and no
  // longer carries every option's text, so `toContainText` on it would have quietly become a test of the
  // placeholder. chooseOptionByLabel asserts exactly one row carries the words and returns its value, which
  // binds the two halves — the words and the filter they apply — in one assertion instead of two.
  const chosen = await chooseOptionByLabel(page, "session-agent-filter", "No revision pinned");
  expect(chosen, "the row reading 'No revision pinned' does not select the no-agent filter").toBe("__no_agent__");
  await expect(page.getByTestId("panel-sessions").locator("tbody tr")).toHaveCount(without);
  // "N of M rows" is the honesty: this control hid rows rather than un-fetching them, and the header says so.
  await expect(page.getByTestId("panel-sessions-count")).toHaveText(`${String(without)} of ${String(total)} rows`);
});

test("the list is forward-only: the cut is stated in words and continued with the server's own cursor", async ({ page }) => {
  const { data, has_more } = await served(page, "/sessions");
  expect(has_more, "the sessions collection fits in one page here, so truncation cannot be observed").toBe(true);
  const backward: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("before=")) backward.push(r.url());
  });
  await openList(page);

  await expect(page.getByTestId("panel-sessions-more")).toContainText("more are available");
  await page.getByTestId("panel-sessions-load-more").click();
  await expect(page.getByTestId("panel-sessions").locator("tbody tr")).not.toHaveCount(data.length);
  // beginList answers ?before= with a 400, so a backward control could never work — and nothing here asks.
  expect(backward, "a request carried ?before=, which the API refuses with a 400").toEqual([]);
});

// --- THE TRANSCRIPT ------------------------------------------------------------------------------------

test("the transcript replays the session's own journal, in sequence, with an elapsed stamp per frame", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);

  // The breadcrumb's id is a COPY BUTTON carrying the short form (measured §3), so the assertion is that the
  // clipping is derived from the id — not that the cell prints 36 characters nobody can read.
  await expect(page.getByTestId("breadcrumb-session")).toContainText(shortId(data[0].id));
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
  const count = await rows.count();
  expect(count).toBeGreaterThan(1);

  // Elapsed is measured from the journal's FIRST frame, so row one is zero and the column never goes
  // backwards. A stamp computed from the reader's clock would fail both halves.
  const elapsed = await column(page, "session-transcript", 5).allTextContents();
  expect(elapsed[0]).toBe("0:00:00");
  const seconds = elapsed.map((s) => {
    const [h, m, sec] = s.split(":").map(Number);
    return h * 3600 + m * 60 + sec;
  });
  expect([...seconds].sort((a, b) => a - b)).toEqual(seconds);
});

test("a session that never ran shows an em dash for its span, not a zero and not the word undefined", async ({ page }) => {
  // THE KEY IS ABSENT, NOT NULL, and that is measured rather than read off the schema: `GET /v1/sessions/{id}`
  // for a session with no runs answers eleven keys and omits first_activity_at / last_activity_at /
  // duration_ms entirely (the Go projection carries them omitempty). A console typed against `duration_ms:
  // number | null` renders `undefined` here; one that defaulted the absence to 0 would claim a run happened
  // instantly, which session.json spells out is the one thing null must NOT be confused with.
  const { data } = await served(page, "/sessions");
  const never = data.find((r) => r.duration_ms === undefined || r.duration_ms === null);
  expect(never, "every session on this stack has run, so the absent-span rendering cannot be observed").not.toBe(undefined);
  expect(Object.keys(never ?? {}), "the row this test needs still carries a duration_ms key").not.toContain("duration_ms");

  await openTranscript(page, never?.id ?? "");
  // toHaveText, not toContainText: the SPACE between the chip's key and its value is part of the assertion.
  // A flex chip drops it (the gap is layout, not text), which puts an em dash against a word in the rendered
  // copy — the defect tests/rendered-copy.spec.ts exists to catch and cannot see here, because the session it
  // happens to visit has a duration.
  await expect(page.getByTestId("chip-duration")).toHaveText("Duration —");
  await expect(page.getByTestId("chip-tokens")).toHaveText("Tokens 0 / 0");
});

test("the transcript names LANES and never a speaker — it does not pretend to be a chat", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });

  // THE STRONG FORM: every badge is exactly what lib/timeline's laneFor computes for the type in the same
  // row. A weaker assertion — "the badge text is one of the eight lane words" — would pass over a screen
  // that put the same badge on every row, which is precisely how a fabricated role column would look.
  const types = await page.getByTestId("event-open").allTextContents();
  const badges = await page.getByTestId("event-lane").allTextContents();
  expect(badges.length).toBe(types.length);
  expect(badges).toEqual(types.map((t) => LANE_LABEL[laneFor(t)]));

  // And no badge is a speaker. This is the assertion the reference screen would fail: it labels its rows
  // User / Tool / Agent, and this journal has no author to put in such a column.
  const speakers = badges.filter((b) => /^(user|assistant|human|system|you)$/i.test(b.trim()));
  expect(speakers, "a transcript row is labelled with a SPEAKER, which is a claim no event in this system makes").toEqual([]);
});

test("the scrubber puts one mark per frame inside the journal's own span, and says so in words", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
  const events = await rows.count();

  const marks = page.getByTestId("transcript-scrubber").locator(".scrubber-mark");
  await expect(marks).toHaveCount(events);
  // Every mark is INSIDE the track. A NaN or an out-of-range percentage puts a mark nowhere, and a track of
  // marks nobody can see is the failure this assertion exists for.
  for (const mark of await marks.all()) {
    const left = Number((await mark.getAttribute("style"))?.match(/left:\s*([\d.]+)%/)?.[1] ?? "-1");
    expect(left).toBeGreaterThanOrEqual(0);
    expect(left).toBeLessThanOrEqual(100);
  }
  // The caption is the non-visual half: the track is aria-hidden, so this sentence and the Elapsed column
  // are what a screen reader gets. It must carry the same count the marks do.
  await expect(page.getByTestId("scrubber-caption")).toContainText(`${String(events)} events`);
});

test("selecting an event opens the detail pane, and Raw is the frame the journal actually served", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });

  // DISABLED, NOT HIDDEN, BEFORE A SELECTION — the reference's rule and the reverse of this console's habit
  // of rendering only what applies. An affordance that does not exist until after the interaction that
  // needs it cannot be discovered.
  await expect(page.getByTestId("detail-empty")).toBeVisible();
  await expect(page.getByTestId("detail-rendered")).toBeVisible();
  await expect(page.getByTestId("detail-raw")).toBeVisible();
  await expect(page.getByTestId("detail-raw")).toBeDisabled();

  const type = await rows.first().getByTestId("event-open").textContent();
  await rows.first().getByTestId("event-open").click();

  // Rendered: the frame's fields, labelled.
  await expect(page.getByTestId("detail-rendered-body")).toContainText(type ?? "");
  await expect(page.getByTestId("detail-rendered-body")).toContainText("Sequence");

  await expect(page.getByTestId("detail-raw")).toBeEnabled();

  // Raw: the same frame as JSON. The two must agree — a Rendered view assembled from something other than
  // the frame in front of it is the defect a Raw toggle exists to make visible.
  await page.getByTestId("detail-raw").click();
  const raw = (await page.getByTestId("detail-raw-body").textContent()) ?? "";
  const parsed = JSON.parse(raw) as { type: string; sequence: number };
  expect(parsed.type).toBe(type);
  expect(parsed.sequence).toBe(1);
});

test("the event-type control narrows the transcript and the count keeps both numbers", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
  const total = await rows.count();

  const type = (await rows.first().getByTestId("event-open").textContent()) ?? "";
  await chooseOption(page, "transcript-type", type);
  const kept = await rows.count();
  expect(kept).toBeGreaterThan(0);
  expect(kept).toBeLessThan(total);
  await expect(page.getByTestId("transcript-count")).toHaveText(`${String(kept)} of ${String(total)} events`);

  // A search that matches nothing is a filter, not an empty journal, and it does not borrow the empty
  // journal's sentence.
  await page.getByTestId("transcript-search").fill("zzsessionsprobe");
  await expect(page.getByTestId("transcript-no-match")).toBeVisible();
  await expect(page.getByTestId("transcript-empty")).toHaveCount(0);
});

test("a refused run wears an error pill and a completed one does not", async ({ page }) => {
  // A compose run journals six types and `canceled` is not one of them, so a failing frame cannot be
  // produced there at all — the fixture is what makes this pill falsifiable rather than decorative.
  skipOnReal("DIV-UI-002");
  const { data } = await served(page, "/sessions");
  const refused = data.find((r) => r.name === "Refused release push");
  expect(refused, "the fixture's refused session is missing, so the error pill would be asserted over nothing").not.toBe(undefined);

  await openTranscript(page, refused?.id ?? "");
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("session-transcript")).toContainText("run.canceled.v1", { timeout: 15_000 });
  expect(await page.getByTestId("event-error").count()).toBeGreaterThan(0);

  // The other direction. Without it, a pill rendered on every row would pass the assertion above.
  const completed = data.find((r) => r.name !== "Refused release push");
  await openTranscript(page, completed?.id ?? "");
  await expect(page.getByTestId("session-transcript").locator("tbody tr").first()).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("session-transcript")).toContainText("run.completed.v1", { timeout: 15_000 });
  await expect(page.getByTestId("event-error")).toHaveCount(0);
});

test("the selected event and the open tab are in the URL, so a reload lands where the reader was", async ({ page }) => {
  // THE SINGLE BIGGEST THING THIS CONSOLE DID NOT DO. No screen anywhere puts state in the address bar, so
  // every selection dies on reload, the back button leaves the page, and a row cannot be sent to anybody.
  // The reference's shape is `?event=…&segment=debug`, and all four consequences are asserted here.
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);
  const rows = page.getByTestId("session-transcript").locator("tbody tr");
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });

  // Nothing selected, nothing in the URL: an empty `?event=` would be a URL claiming a selection it cannot name.
  expect(new URL(page.url()).search).toBe("");

  // toHaveURL, never expect(page.url()): router.push resolves AFTER the click does, so a synchronous read of
  // the address bar is a race that passes on a fast machine and fails on a loaded one.
  await rows.nth(2).getByTestId("event-open").click();
  await expect(rows.nth(2).getByTestId("event-open")).toHaveAttribute("aria-pressed", "true");
  await expect(page).toHaveURL(/[?&]event=3(&|$)/);

  // A RELOAD LANDS ON THE SAME ROW. This is the assertion local state cannot pass: the selection is read
  // back out of the address bar rather than remembered.
  await page.reload();
  await expect(page.getByTestId("session-transcript")).toBeVisible({ timeout: 15_000 });
  await page.waitForLoadState("networkidle");
  await expect(page.getByTestId("session-transcript").locator("tbody tr").nth(2).getByTestId("event-open")).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByTestId("detail-rendered-body")).toBeVisible();

  // The tab too, and it composes with the selection rather than replacing it.
  await page.getByTestId("tab-debug").click();
  await expect(page.getByTestId("session-debug")).toBeVisible();
  await expect(page).toHaveURL(/segment=debug/);
  // The two parameters COMPOSE — opening a tab must not throw away which row was being read.
  await expect(page).toHaveURL(/[?&]event=3(&|$)/);

  // THE BACK BUTTON UNDOES A STEP rather than leaving the page — which is what `push` buys and `replace`
  // would have thrown away.
  await page.goBack();
  await expect(page.getByTestId("session-transcript")).toBeVisible();
  await expect(page).not.toHaveURL(/segment=/);
  await expect(page).toHaveURL(/[?&]event=3(&|$)/);

  // And the close control drops the parameter, so the URL never outlives the state it describes.
  await page.getByTestId("detail-close").click();
  await expect(page.getByTestId("detail-empty")).toBeVisible();
  await expect(page).not.toHaveURL(/event=/);
});

test("a session id is copied WHOLE, and a refused clipboard is said out loud rather than swallowed", async ({ page, context }) => {
  const { data } = await served(page, "/sessions");
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await openList(page);

  // THE VALUE ON THE CLIPBOARD IS THE FULL ID, not the clipping on screen. That is the whole point of the
  // button: the cell is short so it can be read, and the copy is complete so it can be used.
  await page.getByTestId("panel-sessions").getByTestId("session-copy-id").first().click();
  await expect(page.getByTestId("session-copy-id-said").first()).toHaveText("session ID copied.");
  const clipped = await page.evaluate(() => navigator.clipboard.readText());
  expect(clipped).toBe(data[0].id);
  expect(clipped.length).toBeGreaterThan(shortId(data[0].id).length);
});

test("a clipboard that refuses the write is reported, never silent", async ({ page }) => {
  // NotAllowedError is what writeText throws when the clipboard is denied (MDN). A button that swallows it
  // leaves an operator believing they hold an id they do not — and this console reaches for the clipboard on
  // a surface it may be served to over plain http, where the API is not there at all.
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: () => Promise.reject(new DOMException("Write permission denied.", "NotAllowedError")) },
      configurable: true,
    });
  });
  await openList(page);
  await page.getByTestId("panel-sessions").getByTestId("session-copy-id").first().click();
  await expect(page.getByTestId("session-copy-id-said").first()).toContainText("NotAllowedError");
  await expect(page.getByTestId("session-copy-id-said").first()).toContainText("by hand");
});

test("the transcript's title IS the rename control, and its accessible name still contains what it reads", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);

  // Measured: the heading itself is the button, not a heading with a button beside it.
  const title = page.getByTestId("session-title").getByTestId("session-rename-open");
  await expect(title).toBeVisible();
  const visible = (await page.getByTestId("session-title").locator(".name").textContent()) ?? "";
  expect(visible).not.toBe("");

  // SC 2.5.3 Label in Name: the accessible name must CONTAIN the visible label. An aria-label of "Rename
  // session" over a button reading the session's name would fail the criterion while looking helpful — the
  // sr-only suffix is what makes this both discoverable and conforming, and this is the assertion that says
  // so rather than the comment.
  const accessible = (await title.evaluate((el) => (el as HTMLElement).innerText || el.textContent || "")) ?? "";
  const name = await title.getAttribute("aria-label");
  expect(name, "an aria-label here would REPLACE the visible label and break SC 2.5.3").toBe(null);
  expect((await title.textContent()) ?? "").toContain(visible);
  expect((await title.textContent()) ?? "").toContain("rename");
  expect(accessible.length).toBeGreaterThan(0);
});

test("the Debug tab shows the row and the frames as served, and the tab strip is keyboard-operable", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  await openTranscript(page, data[0].id);
  await expect(page.getByTestId("session-transcript").locator("tbody tr").first()).toBeVisible({ timeout: 15_000 });

  // The APG keyboard model: focus the selected tab, press ArrowRight, the next tab is selected AND focused.
  // A tab strip that only answers a mouse is a screen half this console's operators cannot reach.
  await page.getByTestId("tab-transcript").focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByTestId("tab-debug")).toBeFocused();
  await expect(page.getByTestId("tab-debug")).toHaveAttribute("aria-selected", "true");
  await expect(page.getByTestId("session-transcript")).toBeHidden();

  await expect(page.getByTestId("debug-session")).toContainText(data[0].id);
  await expect(page.getByTestId("debug-session")).toContainText("name_source");
  await expect(page.getByTestId("debug-journal")).toContainText("\"sequence\":1");
});

// --- THE WRITES, LAST: both of these change the collection every test above reads. -----------------------

test("renaming from the list replaces a derived label with an operator one", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  const target = data.find((r) => r.name_source === "derived");
  expect(target, "no derived row to rename").not.toBe(undefined);
  const id = target?.id ?? "";
  const label = `Named by the console ${String(Date.now())}`;
  await openList(page);

  // FILTERED BY THE LINK'S title, NOT BY ROW TEXT: the ID cell renders the SHORT form now, so a row no
  // longer contains its own full id as text and `hasText: id` matches nothing. The title attribute is where
  // the whole value lives, which makes this both correct and a check that it still lives there.
  const row = page
    .getByTestId("panel-sessions")
    .locator("tbody tr")
    .filter({ has: page.locator(`[data-testid="session-link"][title="${id}"]`) });
  await row.getByTestId("session-menu").click();
  // THE ITEM IS ADDRESSED ON THE PAGE, NOT IN THE ROW, and that is the portal rather than a looser selector
  // (E29 component layer). components/ui/Menu.tsx renders the popup as a child of <body> so it escapes the
  // panel's horizontal scroll clip — app/globals.css used to keep the old panel in flow precisely because it
  // could not — so the item is no longer a descendant of its own <tr>. One menu is open at a time, which is
  // what keeps this unambiguous.
  await page.getByTestId("session-rename-open").click();

  // ESCAPE ABANDONS. The control is deliberately not a <form> (see components/Session.tsx: a form with no
  // method=post is the CON-013 hazard), so Enter and Escape are wired BY HAND — which makes them the two
  // behaviours most likely to be silently absent, and the two this test drives with the keyboard.
  await row.getByTestId("session-rename-input").fill("abandoned");
  await row.getByTestId("session-rename-input").press("Escape");
  await expect(row.getByTestId("session-rename-input")).toHaveCount(0);
  await expect(row).not.toContainText("abandoned");

  await row.getByTestId("session-menu").click();
  await page.getByTestId("session-rename-open").click();
  await row.getByTestId("session-rename-input").fill(label);
  await row.getByTestId("session-rename-input").press("Enter");

  // On screen: the new label, and the `auto` marker GONE — which is the whole point of the write. A rename
  // that left the row still marked as derived would be a console saying nobody chose a name somebody typed.
  // Matched on the link's `title`, not on the row's text — the SECOND place this file made that mistake.
  // The ID cell renders the SHORT form now, so a row no longer contains its own id as text and `hasText`
  // matches nothing. Filtering on the title is also a check in its own right: the whole value must still be
  // there for a copy button and an href to be honest.
  const renamed = page
    .getByTestId("panel-sessions")
    .locator("tbody tr")
    .filter({ has: page.locator(`[data-testid="session-link"][title="${id}"]`) });
  await expect(renamed).toContainText(label, { timeout: 15_000 });
  await expect(renamed.locator(".name-mark")).toHaveCount(0);

  // And durably: re-read the row through the API. A screen that updated only its own state would pass
  // everything above and lose the label on the next load.
  const { data: after } = await served(page, "/sessions");
  const stored = after.find((r) => r.id === id);
  expect(stored?.name).toBe(label);
  expect(stored?.name_source).toBe("operator");
});

test("the transcript renames the session its heading names, and the Save button is the one that lands it", async ({ page }) => {
  const { data } = await served(page, "/sessions");
  const id = data[0].id;
  const label = `Renamed from the transcript ${String(Date.now())}`;
  await openTranscript(page, id);

  await page.getByTestId("session-rename-open").click();
  await page.getByTestId("session-rename-input").fill(label);
  await page.getByTestId("session-rename-cancel").click();
  await expect(page.getByTestId("session-title")).not.toContainText(label);

  await page.getByTestId("session-rename-open").click();
  await page.getByTestId("session-rename-input").fill(label);
  await page.getByTestId("session-rename-save").click();
  // The HEADING is the name, so the heading is where the write has to show — and the panel re-reads the row
  // the API answered with rather than the string that was typed, so this also fails if the PATCH's response
  // is being discarded.
  await expect(page.getByTestId("session-title")).toContainText(label, { timeout: 15_000 });
  await expect(page.getByTestId("session-title").locator(".name-mark")).toHaveCount(0);
});

test("creating a session adds a row, and the empty state offers the same action", async ({ page }) => {
  await openList(page);

  // THE EMPTY STATE FIRST, reached by a filter that matches nothing rather than by an empty deployment: it
  // must be one sentence and an action, never a blank region with "None yet." on it.
  await chooseOption(page, "session-status-filter", "deleted");
  const empty = page.getByTestId("panel-sessions-empty");
  await expect(empty).toBeVisible();
  // THE MEASURED SHAPE: a title, then one sentence saying what the thing IS, then the action. "None yet."
  // is three words that teach nothing, and an empty screen is the one moment a reader can be taught.
  await expect(empty).not.toContainText("None yet");
  await expect(page.getByTestId("session-empty-title")).toHaveText("No sessions yet");
  await expect(empty.locator(".empty-body")).toContainText("A session is one conversation");
  await expect(page.getByTestId("session-create-empty")).toBeVisible();

  await chooseOption(page, "session-status-filter", "");
  const firstBefore = (await page.getByTestId("panel-sessions").getByTestId("session-link").first().getAttribute("title")) ?? "";
  expect(firstBefore).not.toBe("");
  await page.getByTestId("session-create").click();

  // NEWEST FIRST, so the created session is row one — and the ROW COUNT is deliberately not the assertion:
  // the list is cut at a page, so creating a row pushes the last one off and the count never moves. A test
  // that watched the count would pass whether or not the write landed.
  await expect(page.getByTestId("panel-sessions").getByTestId("session-link").first()).not.toHaveAttribute("title", firstBefore, { timeout: 15_000 });
  const { data } = await served(page, "/sessions");
  await expect(page.getByTestId("panel-sessions").getByTestId("session-link").first()).toHaveAttribute("title", data[0].id);
  // A session created through the API has never run, so there is no first prompt to derive a label from.
  expect(data[0].name_source).toBe("none");
  await expect(page.getByTestId("panel-sessions").locator("tbody tr").first()).toContainText("unnamed");
  await expect(page.getByTestId("session-create-error")).toHaveCount(0);
});
