import { expect, test, type Page } from "@playwright/test";

import { CONSOLE_ROUTES, NAV_GROUPS } from "../lib/routes";
import { announceProfile, signIn } from "./profile";

// THE RAIL, DRIVEN AND MEASURED (E31 shell parity).
//
// WHY THIS FILE EXISTS AND WHY IT IS NOT OPTIONAL. This tree's standing finding is that a test for a surface
// must DRIVE THAT SURFACE — ten forms whose tests posted to the endpoint and never submitted the form, and a
// login form with no `method` so the password went in the URL whenever JS had not hydrated. The shell pass
// added seven controls (a workspace readout, a ⌘K opener and its navigator, three group disclosures, a
// documentation link, two pager arrows) and every one of them is a thing a human clicks. So every one of
// them is clicked here.
//
// AND IT CARRIES ONE COVERAGE OBLIGATION THAT IS OWED RATHER THAN OFFERED.
//
// components/Chrome.tsx makes a nav group a native <details>/<summary> — the element for a disclosure, and
// NOT a <button>. tests/contrast.spec.ts's sweep selects `input, select, textarea, button`, so a <summary>
// is outside it. That is correct under SC 1.4.11 (a control identified only by its TEXT is judged by SC
// 1.4.3 at 4.5:1, and 1.4.11 adds no boundary requirement) and it would still be a coverage hole if nothing
// replaced it — a sweep that gets cleaner as its scope shrinks is this repository's own recurring defect.
// So the obligation is paid here, on the criterion that actually applies: every rail row's TEXT is measured
// against the rail's own fill, in BOTH colour schemes, at 4.5:1.
test.beforeAll(() => announceProfile("shell.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/** relativeLuminance / ratio, WCAG 2.2's own definitions. Same formula as tests/contrast.spec.ts. */
function contrastRatio(a: string, b: string): number {
  const parse = (value: string): [number, number, number] => {
    const m = /^rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)\s*(?:[,/]\s*([\d.]+)\s*)?\)$/.exec(value.trim());
    if (m === null) throw new Error(`not a computed rgb colour: ${value}`);
    // A TRANSLUCENT RAIL IS A RAIL NOTHING CAN JUDGE. app/globals.css's --bg-rail tokens are opaque for
    // exactly this reason, and this throw is what would say so if one ever stopped being.
    if (m[4] !== undefined && Number(m[4]) < 1) throw new Error(`a translucent colour cannot be measured on its own: ${value}`);
    return [Number(m[1]), Number(m[2]), Number(m[3])];
  };
  const lum = ([r, g, b]: [number, number, number]) => {
    const c = (v: number) => {
      const s = v / 255;
      return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    };
    return 0.2126 * c(r) + 0.7152 * c(g) + 0.0722 * c(b);
  };
  const [la, lb] = [lum(parse(a)), lum(parse(b))];
  const [hi, lo] = la >= lb ? [la, lb] : [lb, la];
  return Math.round(((hi + 0.05) / (lo + 0.05)) * 100) / 100;
}

const GROUPS = NAV_GROUPS.filter((g) => g.collapsible).map((g) => g.name);
const SINGLETONS = NAV_GROUPS.filter((g) => !g.collapsible).map((g) => g.name);

/** railRow reads one nav row's measured geometry and paint off the rendered page. */
async function railRow(page: Page, text: string) {
  return page.evaluate((label) => {
    const rows = [...document.querySelectorAll<HTMLElement>(".rail-item")];
    const row = rows.find((r) => (r.querySelector(".rail-label")?.textContent ?? "").trim() === label);
    if (row === undefined) return null;
    const cs = getComputedStyle(row);
    const box = row.getBoundingClientRect();
    const labelEl = row.querySelector<HTMLElement>(".rail-label");
    let surface = "rgb(255, 255, 255)";
    for (let node: Element | null = row; node !== null; node = node.parentElement) {
      const bg = getComputedStyle(node).backgroundColor;
      if (bg !== "transparent" && !/rgba?\([^)]*[,/]\s*0\s*\)/.test(bg)) {
        surface = bg;
        break;
      }
    }
    return {
      tag: row.tagName.toLowerCase(),
      height: Math.round(box.height),
      labelX: labelEl === null ? 0 : Math.round(labelEl.getBoundingClientRect().x),
      paddingLeft: cs.paddingLeft,
      radius: cs.borderRadius,
      weight: labelEl === null ? "" : getComputedStyle(labelEl).fontWeight,
      colour: cs.color,
      surface,
      current: row.getAttribute("aria-current"),
    };
  }, text);
}

// --- THE RAIL'S SHAPE ------------------------------------------------------------------------------------

test("the rail is DARKER than the page, which is the inversion of what this console shipped", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();

  const pair = await page.evaluate(() => ({
    rail: getComputedStyle(document.querySelector("header.sidebar") as HTMLElement).backgroundColor,
    pageBg: getComputedStyle(document.querySelector("main.content") as HTMLElement).backgroundColor,
    railWidth: Math.round((document.querySelector("header.sidebar") as HTMLElement).getBoundingClientRect().width),
    // The radius is what makes the page read as sitting ON the rail rather than beside it.
    contentRadius: getComputedStyle(document.querySelector("main.content") as HTMLElement).borderTopLeftRadius,
  }));

  const lum = (rgb: string) => {
    const [r, g, b] = /(\d+),\s*(\d+),\s*(\d+)/.exec(rgb)!.slice(1).map(Number);
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  // eslint-disable-next-line no-console -- the numbers ARE the measurement.
  console.log(`RAIL — ${pair.railWidth}px, fill ${pair.rail} under a page of ${pair.pageBg}, radius ${pair.contentRadius}`);

  expect(pair.railWidth, "the rail is 256px, measured on the reference").toBe(256);
  expect(
    lum(pair.rail),
    "the rail must be DARKER than the page. Measured on the reference 2026-08-01: rail rgb(13,13,13) under a " +
      "page of rgb(26,26,25). This console shipped the inversion — --bg-subtle, one step LIGHTER — which is " +
      "the single most visible difference between the two consoles on a full screen.",
  ).toBeLessThan(lum(pair.pageBg));
  expect(parseFloat(pair.contentRadius), "the page is a panel ON the rail; without the radius it is a second column").toBeGreaterThan(0);
});

test("a top-level link and a group header are w500, a CHILD is w400, and an OPEN group header is w580", async ({ page }) => {
  // THE DOC SAYS w400 FOR ALL OF THEM AND THAT IS WRONG — measured on the reference's own 32 rows,
  // 2026-08-01. Building them at one weight flattens exactly the hierarchy the grouping creates, so this is
  // asserted rather than left to a stylesheet nobody re-reads.
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();

  const overview = await railRow(page, "Overview");
  expect(overview, "no rail row is labelled Overview").not.toBeNull();
  expect(overview!.weight, "a top-level link is w500").toBe("500");

  // Operate holds /sessions, so it is the group the route opened.
  const operate = await railRow(page, "Operate");
  expect(operate!.tag, "a collapsible group's header is a <summary>, not a <button> — see the file header").toBe("summary");
  expect(operate!.weight, "an OPEN group header is w580").toBe("580");

  // A group NOT holding the current route is closed, so its header is w500.
  const closed = GROUPS.find((g) => !CONSOLE_ROUTES.some((r) => r.group === g && r.path === "/sessions"));
  const closedRow = await railRow(page, closed!);
  expect(closedRow!.weight, `a CLOSED group header (${closed!}) is w500`).toBe("500");

  const child = await railRow(page, "Sessions");
  expect(child!.weight, "a group CHILD is w400").toBe("400");
});

test("a child's label sits at the same x as its parent's, which is the whole of why the rail reads as a tree", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();

  const parent = await railRow(page, "Operate");
  const child = await railRow(page, "Sessions");
  // eslint-disable-next-line no-console -- see above.
  console.log(`RAIL INDENT — group label x=${String(parent!.labelX)}, child label x=${String(child!.labelX)}, child padding-left ${child!.paddingLeft}`);

  expect(
    child!.labelX,
    "8px padding + a 20px icon + a 12px gap = 40, and a child carries `padding-left: 40px` with no icon so " +
      "its label lands under its parent's. Measured on the reference: both at x=52.",
  ).toBe(parent!.labelX);
  expect(child!.paddingLeft).toBe("40px");
  expect(parent!.height, "the item box is 36px; the 40px pitch the doc reports is 36 + a 4px gap").toBe(36);
  expect(child!.height).toBe(36);
});

test("the current page carries a fill and full-strength text, and the fill is a LUMINANCE difference", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();

  const current = await railRow(page, "Sessions");
  const sibling = await railRow(page, "Live runs");
  expect(current!.current, "the current row must be machine-readable, not only visible").toBe("page");
  expect(sibling!.current).toBeNull();

  const lum = (rgb: string) => {
    const [r, g, b] = /(\d+),\s*(\d+),\s*(\d+)/.exec(rgb)!.slice(1).map(Number);
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  // eslint-disable-next-line no-console -- see above.
  console.log(`RAIL CURRENT — fill ${current!.surface} vs a resting row's ${sibling!.surface}; text ${current!.colour} vs ${sibling!.colour}`);
  expect(
    Math.abs(lum(current!.surface) - lum(sibling!.surface)),
    "the current row's mark must survive greyscale. A HUE difference alone would fail SC 1.4.1 for a reader " +
      "who cannot separate the accent from the rail; a fill is a luminance difference and does not.",
  ).toBeGreaterThan(1);
  expect(current!.weight, "the reference does NOT change the weight of the current row, and neither do we").toBe(sibling!.weight);
});

test("every rail row's text clears 4.5:1 against the rail — the criterion a <summary> is actually judged by", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();
  // Open every group, so a CHILD row is measured too rather than only the headers that happen to be visible.
  for (const group of GROUPS) {
    const summary = page.locator(".rail-summary").filter({ hasText: group });
    const open = await summary.evaluate((el) => (el.parentElement as HTMLDetailsElement).open);
    if (!open) await summary.click();
  }

  // THE PAINT IS MEASURED AFTER IT HAS SETTLED, AND THAT IS NOT PADDING.
  //
  // `.rail-item` carries `transition: background 90ms` (app/globals.css §5), and `getComputedStyle` during a
  // transition returns the INTERPOLATED value — including an interpolated ALPHA, because the resting state is
  // `rgba(…, 0)`. Measured 2026-08-01, this sweep read `rgba(25, 25, 24, 0.984)` off the row the loop above
  // had just clicked and the parser threw: a translucent colour, 98.4% of the way to opaque, that exists for
  // ninety milliseconds and never afterwards.
  //
  // The mouse is moved OFF the rail first (a hovered row transitions too, and Playwright leaves the pointer
  // wherever it last clicked), then the wait clears --duration-3, the longest step in the scale. Disabling
  // transitions instead would have measured a state the browser never paints; the settled state is the one
  // an operator looks at, so waiting for it is the honest instrument.
  await page.mouse.move(900, 500);
  await page.waitForTimeout(400);

  const measured = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>(".rail-item, .rail-search, .workspace-name, .rail-search-label")].map((el) => {
      let surface = "rgb(255, 255, 255)";
      for (let node: Element | null = el; node !== null; node = node.parentElement) {
        const bg = getComputedStyle(node).backgroundColor;
        if (bg !== "transparent" && !/rgba?\([^)]*[,/]\s*0\s*\)/.test(bg)) {
          surface = bg;
          break;
        }
      }
      return { text: (el.textContent ?? "").trim().slice(0, 28), colour: getComputedStyle(el).color, surface };
    }),
  );

  expect(measured.length, "the sweep found no rail rows at all — an absence proved against an empty haystack").toBeGreaterThan(10);
  const failing = measured.filter((m) => contrastRatio(m.colour, m.surface) < 4.5);
  const worst = [...measured].sort((a, b) => contrastRatio(a.colour, a.surface) - contrastRatio(b.colour, b.surface))[0];
  // eslint-disable-next-line no-console -- the numbers ARE the measurement.
  console.log(
    `RAIL TEXT SWEEP — ${String(measured.length)} row(s), ${String(failing.length)} below 4.5:1; ` +
      `weakest "${worst.text}" at ${String(contrastRatio(worst.colour, worst.surface))}:1`,
  );
  expect(failing.map((m) => `${m.text} ${m.colour} on ${m.surface}`)).toEqual([]);
});

// --- THE CONTROLS, EACH ONE DRIVEN ------------------------------------------------------------------------

test("a group DISCLOSES: clicking a closed group's header reveals the pages inside it", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();

  // A group that does NOT hold /sessions, so it starts closed and the click has something to prove.
  const closed = GROUPS.find((g) => !CONSOLE_ROUTES.some((r) => r.group === g && r.path === "/sessions"))!;
  const inside = CONSOLE_ROUTES.filter((r) => r.group === closed);
  expect(inside.length, `${closed} holds no routes, so this test proves nothing`).toBeGreaterThan(1);

  const first = page.getByRole("link", { name: inside[0].label, exact: true });
  await expect(first, "a closed group's children must not be reachable — that is what closed MEANS").toBeHidden();

  await page.locator(".rail-summary").filter({ hasText: closed }).click();
  await expect(first).toBeVisible();

  // AND IT NAVIGATES. A disclosure that opens onto links that do not work is a disclosure that proves the
  // animation and nothing about the console.
  await first.click();
  await expect(page).toHaveURL(new RegExp(`${inside[0].path.replace("/", "\\/")}$`));
  await expect(page.getByTestId("page-title")).toHaveText(inside[0].label);
});

test("a reader's open/closed choice survives a navigation", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();
  const closed = GROUPS.find((g) => !CONSOLE_ROUTES.some((r) => r.group === g && r.path === "/sessions"))!;
  await page.locator(".rail-summary").filter({ hasText: closed }).click();

  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });
  // The group the reader opened is still open on the next screen. Without the localStorage half, every
  // navigation would reset the rail to whatever the route implies and the reader's choice would be a flicker.
  //
  // READ OFF `details.open` AND NOT OFF `aria-expanded`. A <summary> has NO aria-expanded attribute — the
  // expanded state is the parent <details>'s `open` property and the accessibility tree derives it. Measured
  // 2026-08-01: `toHaveAttribute("aria-expanded", /true/)` received `""`. Asserting the attribute would have
  // been asserting something no browser writes, which is a test that can only ever be red or vacuous.
  await expect
    .poll(
      async () =>
        page.locator(".rail-summary").filter({ hasText: closed }).evaluate((el) => (el.parentElement as HTMLDetailsElement).open),
      { message: "the group the reader opened closed itself again on the next page", timeout: 15_000 },
    )
    .toBe(true);
});

test("⌘K opens the navigator, and the navigator NAVIGATES", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.locator("main")).toBeVisible();

  // THE KEYBOARD PATH, which is the one the label on the control promises. A button that opens a dialog while
  // the shortcut printed on its face does nothing is the shape of defect this tree keeps finding.
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.getByTestId("command-search-dialog")).toBeVisible();
  await expect(page.getByTestId("command-search-hits").locator("li")).toHaveCount(CONSOLE_ROUTES.length);

  // MATCHED ON THE LEAD, NOT ONLY THE LABEL — "spent" appears in no route's NAME and is in /usage's own
  // sentence, so a label-only match would make an operator guess this console's nouns.
  //
  // THE EXPECTED COUNT IS RE-DERIVED FROM THE ROUTE TABLE rather than written down as a number, and that is
  // not tidiness: a literal `1` here is a claim about today's COPY, and the first lead somebody rewords
  // turns this test red for a reason that has nothing to do with the control. (It also caught the author out
  // once already — the first draft searched "spend", which matches nothing, because the lead says "spent".)
  const term = "spent";
  const expected = CONSOLE_ROUTES.filter((r) => `${r.label} ${r.group} ${r.lead}`.toLowerCase().includes(term));
  expect(expected.length, `no route's label, group or lead contains "${term}", so this test would prove nothing`).toBeGreaterThan(0);
  await page.getByTestId("command-search-input").fill(term);
  const hits = page.getByTestId("command-search-hits").locator("li");
  await expect(hits).toHaveCount(expected.length);
  await expect(hits.first()).toContainText(expected[0].label);

  // AND IT NAVIGATES. A navigator that filters and cannot take you anywhere is a filter.
  await hits.first().locator("a").click();
  await expect(page).toHaveURL(new RegExp(`${expected[0].path.replace("/", "\\/")}$`));
  await expect(page.getByTestId("page-title")).toHaveText(expected[0].label);
});

test("the navigator says a no-match is about SCREENS, because /v1 has no text search on any collection", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("main")).toBeVisible();
  await page.getByTestId("command-search-open").click();
  await expect(page.getByTestId("command-search-dialog")).toBeVisible();

  await page.getByTestId("command-search-input").fill("ses_010010101");
  // A SESSION ID FINDS NOTHING, and the message must say why rather than reading as "that session does not
  // exist" — which is what an operator would otherwise conclude about a row that is one click away on
  // /sessions. components/Panel.tsx's filter box states its own scope for the same reason.
  await expect(page.getByTestId("command-search-empty")).toContainText(/SCREENS/);
  await expect(page.getByTestId("command-search-empty")).toContainText(/no text search/i);
});

test("the rail states the scope ONCE, at the top, and carries no organisation card at the foot", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("panel-projects")).toBeVisible({ timeout: 15_000 });

  // THIS ASSERTED TWO ENDS UNTIL A.2 TASK 6: a project readout at the top and an organisation card at the
  // foot, in the reference's order. The card read the first row of /v1/organizations, and that route is
  // unmounted — so on a real stack it rendered "——" over "—" on every screen while this spec passed,
  // because the fixture still served the route. Removing the route from the fixture is what made it fail.
  const scope = page.getByTestId("scope");
  await expect(scope).toBeVisible();

  // THE CARD IS GONE, ASSERTED RATHER THAN ASSUMED. Without this line the removal is proven by nothing: a
  // reintroduced org card would simply not be looked at.
  await expect(page.getByTestId("rail-org")).toHaveCount(0);

  // THE READOUT IS STILL ABOVE THE NAV, which is the half of the ordering that survives — the reference puts
  // its workspace control under the wordmark, and ours had one bordered "Scope" card at the foot holding
  // everything, which is why the first thing on every screen read as a box that had failed to load.
  const nav = page.getByRole("navigation").first();
  const scopeY = (await scope.boundingBox())!.y;
  const navY = (await nav.boundingBox())!.y;
  expect(scopeY, "the project readout belongs above the nav").toBeLessThan(navY);

  // AND IT SAYS SOMETHING. A scope block whose lines are em dashes is the defect this console shipped for
  // two epics — `String(row.display_name ?? row.id)` keeps an EMPTY display_name because "" is neither null
  // nor undefined.
  await expect(scope).not.toContainText("—");
});

test("Documentation leaves the console, and says so to a screen reader as well as with an arrow", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("main")).toBeVisible();
  const doc = page.getByTestId("rail-documentation");
  await expect(doc).toBeVisible();
  await expect(doc).toHaveAttribute("href", /^https?:\/\//);
  // The ↗ is aria-hidden, so the accessible name has to carry the same fact in words or a screen-reader user
  // is the one reader not told the link leaves.
  await expect(doc).toHaveAccessibleName(/opens in a new tab/i);
});

// --- THE PAGE'S TITLE LINE -------------------------------------------------------------------------------

test("a list page says its own name ONCE, with the count and the action on that line", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.getByTestId("panel-sessions")).toBeVisible({ timeout: 15_000 });

  // THE DEFECT THIS REPLACES: <h1>Sessions</h1>, one sentence, then <h2>Sessions</h2> with the count and the
  // create button beside it. The page said its own name twice on eight screens.
  await expect(page.getByRole("heading", { name: "Sessions", exact: true })).toHaveCount(1);
  await expect(page.getByTestId("page-title")).toHaveText("Sessions");

  const badge = page.getByTestId("panel-sessions-count");
  await expect(badge).toBeVisible();
  await expect(badge).toContainText(/\d+ rows/);
  // ON THE TITLE'S OWN LINE — measured on the reference, where a list page reads `API keys ⁴`. A badge that
  // merely exists somewhere on the page is not the thing that was asked for.
  const titleBox = (await page.getByTestId("page-title").boundingBox())!;
  const badgeBox = (await badge.boundingBox())!;
  expect(Math.abs(badgeBox.y + badgeBox.height / 2 - (titleBox.y + titleBox.height / 2))).toBeLessThan(16);

  // The region is still NAMED for a screen reader even though the <h2> is not drawn.
  await expect(page.getByTestId("panel-sessions")).toHaveAttribute("aria-label", "Sessions");
});

test("the search box and every filter chip share ONE row, above the table", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.getByTestId("panel-sessions")).toBeVisible({ timeout: 15_000 });

  const boxes = await page.evaluate(() => {
    const tools = document.querySelector(".panel-tools");
    if (tools === null) return null;
    const controls = [...tools.querySelectorAll<HTMLElement>('input[type="search"], .ui-select-trigger')];
    const table = document.querySelector("table");
    return {
      ys: controls.map((c) => Math.round(c.getBoundingClientRect().y)),
      count: controls.length,
      tableY: table === null ? 0 : Math.round(table.getBoundingClientRect().y),
      lastControlBottom: Math.max(...controls.map((c) => c.getBoundingClientRect().bottom)),
    };
  });
  expect(boxes, "no toolbar rendered").not.toBeNull();
  expect(boxes!.count, "a single control is not a row").toBeGreaterThan(2);
  // eslint-disable-next-line no-console -- the numbers ARE the measurement.
  console.log(`TOOLBAR — ${String(boxes!.count)} control(s) at y=${boxes!.ys.join(",")}; table at y=${String(boxes!.tableY)}`);
  expect(new Set(boxes!.ys).size, "the search box and the filter chips must share one row — measured on the reference, all four of its chips and its search box sit at y=96").toBe(1);
  expect(boxes!.tableY, "the toolbar sits ABOVE the table").toBeGreaterThan(boxes!.lastControlBottom);
});

test("the row is 46px and the column head is 13px w500 and NOT uppercased", async ({ page }) => {
  await page.goto("/sessions");
  await expect(page.getByTestId("panel-sessions").locator("tbody tr").first()).toBeVisible({ timeout: 15_000 });

  const t = await page.evaluate(() => {
    const th = document.querySelector("thead th") as HTMLElement;
    const tr = document.querySelector("tbody tr") as HTMLElement;
    const cs = getComputedStyle(th);
    return {
      rowH: Math.round(tr.getBoundingClientRect().height),
      headH: Math.round(th.getBoundingClientRect().height),
      thSize: cs.fontSize,
      thWeight: cs.fontWeight,
      thTransform: cs.textTransform,
    };
  });
  // eslint-disable-next-line no-console -- see above.
  console.log(`TABLE — row ${String(t.rowH)}px, head ${String(t.headH)}px, th ${t.thSize} w${t.thWeight} ${t.thTransform}`);
  expect(t.rowH).toBe(46);
  expect(t.headH).toBe(32);
  expect(t.thSize).toBe("13px");
  expect(t.thWeight).toBe("500");
  expect(t.thTransform, "the reference's column head is NOT uppercased; ours was 11px semibold uppercase, which is three shouting devices on one label").toBe("none");
});

// --- WHAT THE NAV DECLARES IS WHAT THE NAV RENDERS ---------------------------------------------------------

test("every declared route is reachable from the rail, and nothing in the rail is undeclared", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("main")).toBeVisible();
  for (const group of GROUPS) {
    const summary = page.locator(".rail-summary").filter({ hasText: group });
    if (!(await summary.evaluate((el) => (el.parentElement as HTMLDetailsElement).open))) await summary.click();
  }

  const hrefs = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLAnchorElement>('nav[aria-label="Primary"] a')].map((a) => a.getAttribute("href") ?? ""),
  );
  // BOTH DIRECTIONS. A route linked from nowhere is an unlinked page — half of the hole lib/routes.ts exists
  // to close — and a rail row for a path the table does not declare is a link to a page nothing scans.
  expect([...hrefs].sort()).toEqual([...CONSOLE_ROUTES.map((r) => r.path)].sort());

  // AND THE SINGLETONS ARE NOT UNDER A HEADER. A section label over one row is a word that adds a line and
  // says nothing, which is what the owner's "hiç bir şey anlaşılmıyor menülerden" is about.
  for (const name of SINGLETONS) {
    await expect(page.locator(".rail-summary").filter({ hasText: name }), `${name} is rendered as a collapsible group over a single row`).toHaveCount(0);
  }
});
