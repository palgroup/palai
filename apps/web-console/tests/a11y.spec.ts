
import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

import { CONSOLE_ROUTES, DYNAMIC_CONSOLE_ROUTES } from "../lib/routes";
import {
  FORM_DIALOGS,
  formDialogMountCount,
  IS_REAL,
  PRIMITIVE_DIALOG_EXEMPT,
  PRIMITIVE_DIALOGS,
  primitiveDialogMountCount,
  WCAG_TAGS,
} from "./constants";
import { announceProfile, concreteDynamicPath, openDeclaredDialog, runToTerminal, signIn } from "./profile";

// tabToTestId genuinely presses Tab until the element carrying data-testid=id holds focus — proving KEYBOARD
// REACHABILITY. This is stronger than `.focus()`, which succeeds even on a tabindex=-1 element that Tab can
// never reach; here a control dropped from the tab order would never be reached and this fails.
async function tabToTestId(page: Page, id: string, max = 30) {
  for (let i = 0; i < max; i++) {
    const onTarget = await page.evaluate((t) => document.activeElement?.getAttribute("data-testid") === t, id);
    if (onTarget) return;
    await page.keyboard.press("Tab");
  }
  throw new Error(`Tab never reached [data-testid="${id}"] within ${max} presses — not keyboard-reachable`);
}

// UI-001 (accessibility). The AUTOMATED ceiling: axe-core finds zero violations on EVERY route the console
// declares, keyboard navigation reaches the skip link first and operates the core run flow with no mouse, and
// status is conveyed by a glyph + word (never color alone).
//
// E19 T7 — THIS FILE RUNS ON BOTH PROFILES, AND THAT IS THE POINT. Against the real compose stack these scans
// cover surfaces the fixture cannot produce: an admin panel rendering a genuinely EMPTY collection, and
// whatever a capability the real stack does not mount renders (an ERROR panel). Both are states a real
// operator meets on day one and no fake had ever rendered.
//
// E25 T2 — THE SCANNED ROUTES ARE DERIVED FROM lib/routes.ts, NOT TYPED HERE (plan §3.6 D17). They were typed
// here, in two places, while Playwright collected new SPECS automatically — so a new PAGE got zero axe
// coverage and nothing said so. One scan is now generated per route, which makes the test COUNT of this file
// a function of the list's length, and the coverage assertion at the bottom fails if any route went unscanned.
//
// E25 T2 — AND THE TAG SET IS WIDER (plan §3.6 D16): WCAG_TAGS adds wcag21a/wcag21aa/wcag22aa to the
// wcag2a/wcag2aa this file used to pass, because those two are WCAG 2.0 ONLY and the 2.2 form criteria were
// entirely outside the scan. What that buys is MEASURED below rather than assumed — the widening adds exactly
// three axe rules, and one of the three tags adds none at all.
//
// What still does NOT narrow: a manual VoiceOver/screen-reader pass over a DEPLOYED console. Compose is not
// a deployment, and axe is not a screen reader. That remains §6 operator leg 8, and axe's own published rate
// is "on average 57% of WCAG issues" — so what is proven here is "axe-clean with the WCAG 2.0+2.1+2.2 tags",
// never "accessible".
test.beforeAll(() => announceProfile("a11y.spec.ts"));
// The relay answers 401 without an operator session (E25 T1), so every page in this file needs the door
// opened first. It goes through the REAL login route with the REAL password — no forged cookie.
test.beforeEach(async ({ page }) => signIn(page));

// scanned records which declared routes this run actually put through axe. The coverage test at the bottom
// compares it to lib/routes.ts, so a route that is never scanned is a FAILURE rather than an absence.
const scanned = new Set<string>();

for (const route of CONSOLE_ROUTES) {
  test(`axe-core reports zero violations on ${route.path}`, async ({ page }) => {
    await page.goto(route.path);
    // The route's OWN readiness signal. axe on a page still rendering "Loading…" scans a spinner and reports a
    // clean bill of health for markup it never saw, which is why lib/routes.ts requires this field.
    await expect(page.getByTestId(route.readyTestId)).toBeVisible({ timeout: 15_000 });
    const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
    // A scan that ran ZERO rules would report zero violations too. Rules are what makes this a measurement.
    expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
    scanned.add(route.path);
  });
}

// THE DYNAMIC ROUTES (E29). A screen keyed by a resource id got ZERO axe coverage before this loop, and the
// reason was structural rather than an oversight: CONSOLE_ROUTES holds paths, and `/sessions/[id]` is not
// one. lib/routes.ts now declares how to MAKE the path — which collection to read, and what to POST when
// that collection is empty — so this resolves a real id through the relay and scans the real screen. The
// empty-collection arm creates rather than skips, which is what keeps this a measurement.
const scannedDynamic = new Set<string>();
// The second-tab scans that actually ran. Declared separately from `scannedDynamic` because a route may
// legitimately have no second tab — what must never happen is a route DECLARING one that no scan opened.
const scannedSecondTabs = new Set<string>();

for (const route of DYNAMIC_CONSOLE_ROUTES) {
  test(`axe-core reports zero violations on ${route.pattern}`, async ({ page }) => {
    const path = await concreteDynamicPath(page, route);
    await page.goto(path);
    await expect(page.getByTestId(route.readyTestId)).toBeVisible({ timeout: 15_000 });
    // The transcript's journal is an SSE stream: scanning while frames are still landing scans a page whose
    // rows are still appearing, which is the same defect as scanning a spinner one interaction later.
    await page.waitForLoadState("networkidle");
    const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
    expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
    // eslint-disable-next-line no-console -- the resolved path is the evidence that a real row was scanned.
    console.log(`AXE DYNAMIC ROUTE — ${route.pattern} scanned as ${path}`);
    scannedDynamic.add(route.pattern);
  });

  // THE SECOND TAB IS A SECOND SCREEN, and a scan of the page as it opens never looks at it. `hidden` takes
  // the whole panel out of the accessibility tree, so the first scan reports a clean bill of health for
  // markup it did not see — the same shape as scanning a page still rendering "Loading…", one interaction
  // later.
  //
  // THE PAIR IS READ OFF THE ROUTE (page-parity pass). It was `tab-debug` / `session-debug` written here, in
  // a loop over every dynamic route — so the SECOND dynamic route to exist would have driven the session
  // transcript's own testids against a page that has neither, and the failure would have looked like a
  // broken locator rather than like a missing declaration. A route with no `secondTab` gets no second scan,
  // and the count printed by the coverage test at the bottom says how many were declared.
  const second = route.secondTab;
  if (second !== undefined) {
    test(`axe-core reports zero violations on ${route.pattern} with its second tab open`, async ({ page }) => {
      const path = await concreteDynamicPath(page, route);
      await page.goto(path);
      await expect(page.getByTestId(route.readyTestId)).toBeVisible({ timeout: 15_000 });
      await page.waitForLoadState("networkidle");
      await page.getByTestId(second.tabTestId).click();
      await expect(page.getByTestId(second.panelTestId)).toBeVisible();
      // THE TAB WRITES THE URL, so the click starts a client navigation and the RSC payload for it is still
      // in flight — during which `document.title` is momentarily empty and axe's `document-title` rule fires.
      // That is the same defect as scanning a page still rendering "Loading…", one interaction later, and
      // this file already says so; the settle is what makes the scan a measurement of the page rather than of
      // a navigation. Measured: light passed and dark failed on a machine at load 30.
      await expect(page).toHaveURL(second.url);
      await page.waitForLoadState("networkidle");
      // AND networkidle IS NOT ENOUGH, measured rather than assumed: with the settle above in place this
      // still failed `document-title` on an IDLE machine (load 5.5), because the title is written by React
      // after hydration and the network can fall quiet before that happens. `networkidle` is a proxy for the
      // condition; this waits for the CONDITION. The same rule the rest of this file follows — assert the
      // thing, not a stand-in for it.
      await page.waitForFunction(() => document.title.trim().length > 0);
      const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
      expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
      scannedSecondTabs.add(route.pattern);
    });
  }
}


const scannedDialogs = new Set<string>();

// BOTH DIALOG FAMILIES GO THROUGH ONE LOOP. The scan does not care which component built the modal — it
// cares that a surface which only exists after a click gets looked at — and keeping two loops would have
// meant the primitive's rows depending on someone noticing the first loop existed. See PRIMITIVE_DIALOGS in
// tests/constants.ts for what was uncounted before this.
for (const d of [...FORM_DIALOGS, ...PRIMITIVE_DIALOGS]) {
  test(`axe-core reports zero violations with ${d.dialog} open`, async ({ page }) => {
    // A DIALOG ON A DYNAMIC ROUTE HAS NO PATH UNTIL ONE IS RESOLVED (E30). `dynamic` names the
    // DYNAMIC_CONSOLE_ROUTES pattern, and concreteDynamicPath reads the first row of that collection or
    // CREATES one — it refuses to skip, which is the property that keeps "the deployment has no bindings"
    // from silently meaning "these two dialogs went unscanned".
    const dynamic = "dynamic" in d && typeof d.dynamic === "string" ? d.dynamic : "";
    if (dynamic !== "") {
      const route = DYNAMIC_CONSOLE_ROUTES.find((r) => r.pattern === dynamic);
      if (route === undefined) {
        throw new Error(`FORM_DIALOGS row for ${d.dialog} names dynamic route ${dynamic}, which lib/routes.ts does not declare`);
      }
      await page.goto(await concreteDynamicPath(page, route));
    } else {
      await page.goto(d.route);
    }
    // The opener is inside a PANEL's head — or, since machine-config, inside a ROW's `⋯` menu, which does
    // not exist in the document until that menu is opened. Either way it does not exist until the panel has
    // settled: the same readiness rule the route loop follows, applied one interaction later.
    // profile.ts's openDeclaredDialog owns both shapes, so this sweep and the contrast sweep drive the
    // declared rows identically rather than each keeping its own copy of how to reach one.
    await openDeclaredDialog(page, d);
    // The ACCESSIBLE NAME, asserted rather than assumed: components/FormDialog.tsx names itself with
    // `aria-label` precisely because an aria-labelledby into ResourceForm's derived heading id would produce
    // an EMPTY name if the wording changed — and an empty name is not something axe reports as a violation.
    // The NAME rather than the attribute: ui/Dialog names itself through Base UI's <Title> and an
    // aria-labelledby it owns, the hand-rolled one used aria-label, and an empty name is what both were
    // written to catch. toHaveAccessibleName fails on an empty one either way.
    await expect(page.getByTestId(d.dialog)).toHaveAccessibleName(d.label);
    await expect(page.getByTestId(d.dialog)).toHaveAttribute("aria-modal", "true");
    const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
    expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
    scannedDialogs.add(d.dialog);
  });
}

// THIS TEST KEEPS ITS NAME, and the reason is a record rather than a preference: tests/uat/cases/UI-001 —
// a SHIPPED case inside the committed extensions-0.1.0 bundle — declares this exact title as one of its
// proofs, and tests/uat/extensions resolves every declared proof to a real `test("<title>"` in the tree. A
// rename would make a shipped case cite evidence that no longer exists. So the generated loop above scans "/"
// as one of the declared routes, and this scans it again with the assertion the loop cannot make generically:
// that the panel held REAL ROWS and not a spinner when axe looked at it.
test("axe-core reports zero violations on the admin surface", async ({ page }) => {
  await page.goto("/");
  // A LOADED-WITH-DATA PRECONDITION, and the substring is chosen rather than casual. This read
  // panel-organizations / "org_local" until A.2 Task 6 unmounted that route, and it named
  // identity/store.go's ProvisionFirstOrg — a function that is now ProvisionFirstTenant. The project panel
  // is the surviving equivalent.
  //
  // It does NOT assert a whole id, because no whole id is true on both profiles: the fixture seeds
  // `proj_local` and identity/store.go seeds `prj_local` (DIV-SHP-002, off by a letter — policy.spec.ts
  // records the same trap). `_local` is the part both carry, so this stays a real content assertion instead
  // of degrading to toBeVisible, which a panel rendering its ERROR state would also satisfy.
  await expect(page.getByTestId("panel-projects")).toContainText("_local", { timeout: 15_000 });
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});

// THE WIDENED TAG SET IS NOT DECORATION, AND THIS IS WHERE THAT STOPS BEING A CLAIM (plan §3.6 D16).
//
// An unknown or misspelled axe tag selects NO rules and analyze() then reports zero violations — identical
// output to a clean page. So the widening is verified by what it adds to the RULE SET, and the numbers are the
// measured ones: `wcag21aa` brings autocomplete-valid and avoid-inline-spacing, `wcag22aa` brings target-size,
// and `wcag21a` brings NOTHING — no axe-core 4.12 rule carries that tag. It stays in WCAG_TAGS because it is
// the correct tag for WCAG 2.1 Level A and a future rule should be picked up for free, but nobody should read
// it as coverage: it is a subscription, not a scan. That is the honest cost of widening a tag set.
test("the WCAG 2.1/2.2 tags genuinely select rules the 2.0 tags do not", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByTestId("panel-projects")).toBeVisible({ timeout: 15_000 });

  const ruleIDs = async (tags: string[]) => {
    const r = await new AxeBuilder({ page }).withTags(tags).analyze();
    return new Set([...r.passes, ...r.violations, ...r.incomplete, ...r.inapplicable].map((x) => x.id));
  };
  const narrow = await ruleIDs(["wcag2a", "wcag2aa"]);
  const wide = await ruleIDs(WCAG_TAGS);
  const added = [...wide].filter((id) => !narrow.has(id)).sort();

  // eslint-disable-next-line no-console -- the number is the evidence; a silent widening is the trap.
  console.log(`AXE TAG WIDENING — 2.0 tags select ${narrow.size} rules, WCAG_TAGS selects ${wide.size}; added: ${added.join(", ")}`);
  expect(added).toEqual(["autocomplete-valid", "avoid-inline-spacing", "target-size"]);
});

// THE POPUPS, WHICH ARE THE ONE SURFACE NO OTHER MEASUREMENT IN THIS SUITE REACHES (E29 component layer).
//
// The generated loop above scans each route as it opens, with every listbox and menu CLOSED — and a closed
// popup renders nothing, so those scans report a clean bill of health for markup they never saw. That is the
// same defect as scanning a page still rendering "Loading…", and this file already says so about the second
// tab; the component layer adds two more instances of it.
//
// tests/contrast.spec.ts cannot close the gap either, and for a structural reason rather than a timing one:
// its sweep selects `input, select, textarea, button`, and a listbox row is a `<div role="option">` while a
// menu row is a `<div role="menuitem">`. Neither would be measured even with the popup open.
//
// So the popups are scanned HERE, open, by the same tag set as every other scan in this file. What that
// buys concretely: colour-contrast on the row text and on the highlighted/selected states, aria-required-
// children on the listbox, target-size (WCAG 2.2) on rows this console sizes itself, and the
// aria-hidden/focus wiring around a portalled popup — none of which any other leg looks at.
for (const surface of [
  { name: "a listbox", open: "session-status-filter", expect: '[role="listbox"]' },
  { name: "a row menu", open: "session-menu", expect: '[role="menu"]' },
]) {
  test(`axe-core reports zero violations on /sessions with ${surface.name} open`, async ({ page }) => {
    await page.goto("/sessions");
    await expect(page.getByTestId("panel-sessions")).toBeVisible({ timeout: 15_000 });
    // A ROW MUST EXIST BEFORE THE ROW MENU DOES. Opening a control that is not there would fail on the click
    // rather than on the scan, which is the right failure — but the wait is what makes the scan a
    // measurement of a populated list rather than of an empty one.
    await expect(page.getByTestId("panel-sessions").locator("tbody tr").first()).toBeVisible({ timeout: 15_000 });
    await page.waitForLoadState("networkidle");

    await page.getByTestId(surface.open).first().click();
    const popup = page.locator(surface.expect);
    await expect(popup, `${surface.name} did not open, so the scan below would be of a closed control`).toBeVisible();
    // AND IT HAS CONTENT. A popup that answers to its name before its rows resolve is exactly the shape
    // tests/reveal-once.spec.ts failed on this morning: the container is visible, the scan finds nothing to
    // judge, and the result is a green scan of an empty box.
    await expect(popup.locator('[role="option"], [role="menuitem"]')).not.toHaveCount(0);

    const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
    expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
    // eslint-disable-next-line no-console -- the row count is the evidence the scan had something to judge.
    console.log(`AXE POPUP — ${surface.name} on /sessions scanned with ${String(await popup.locator('[role="option"], [role="menuitem"]').count())} row(s)`);
  });
}

// AND THE ONE SURFACE THE POPUP SCANS ABOVE STILL DO NOT REACH: AN OPEN DIALOG.
//
// CLAUDE.md names this as a shipped defect of this suite, in its own words: "hiçbir axe taraması açık bir
// dialog ile koşmamıştı, çünkü döngü rotayı yüklenirken tarıyor ve expectAxeClean dialog kapandıktan sonra
// oturuyordu — yani bir form dialog'a taşınınca kanıttan sessizce çıkıyor ve süpürme kapsam daralırken daha
// temiz bir sayı raporluyordu." No axe scan had ever run with a dialog open, because the generated loop
// scans a route as it loads and every expectAxeClean sits AFTER the dialog closed. A form moved into a
// dialog therefore leaves the evidence silently, and the sweep reports a CLEANER number while covering less.
//
// That is not hypothetical here: two page agents are moving their create forms into components/ui/Dialog.tsx
// right now, and app/globals.css:134 records the same shape already happening once — `button.danger` went a
// whole epic unmeasured because "its only caller was inside a dialog, which the sweep walks the routes with
// CLOSED".
//
// So this scans one. The dialog is reached the way an operator reaches it — a row menu, then the item — and
// the assertions before the scan are what stop it being a scan of nothing: the dialog is visible, it carries
// the alertdialog role, and its two controls are present.
test("axe-core reports zero violations on /fleet with a destructive dialog OPEN", async ({ page }) => {
  await page.goto("/fleet");
  await expect(page.getByTestId("panel-runners")).toBeVisible({ timeout: 15_000 });
  const toggles = page.getByTestId("panel-runners").locator('[data-testid^="runner-menu-"]');
  await expect(toggles.first(), "the machines panel rendered no rows, so no dialog can be opened").toBeVisible({ timeout: 15_000 });

  // The first machine whose menu offers a revoke. Walked rather than assumed: which machines are revocable
  // is fixture state, and a hard-coded id is the thing that rots.
  let opened = "";
  const count = await toggles.count();
  for (let i = 0; i < count && opened === ""; i++) {
    const toggle = toggles.nth(i);
    const id = (await toggle.getAttribute("data-testid"))?.replace("runner-menu-", "") ?? "";
    await toggle.click();
    // The popup is portalled, so it exists a frame after the click — `count()` does not auto-wait.
    await expect(page.getByRole("menu")).toBeVisible();
    if ((await page.getByTestId(`runner-revoke-${id}`).count()) > 0) {
      await page.getByTestId(`runner-revoke-${id}`).click();
      opened = id;
    } else {
      await page.keyboard.press("Escape");
      await expect(page.getByRole("menu")).toHaveCount(0);
    }
  }
  expect(opened, "no machine on this stack offers a revoke, so this scan would be of a closed dialog").not.toBe("");

  const dialog = page.getByTestId("runner-revoke-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toHaveAttribute("role", "alertdialog");
  // CONTENT BEFORE THE SCAN. A dialog that answers to its name before its review region resolves is the
  // reveal-once defect one interaction later: axe finds a shell, reports nothing, and the number looks fine.
  await expect(page.getByTestId("runner-revoke-dialog-review")).toBeVisible();
  await expect(page.getByTestId("runner-revoke-dialog-confirm")).toBeVisible();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
  // eslint-disable-next-line no-console -- the count is the evidence that the scan had a dialog to look at.
  console.log(
    `AXE DIALOG — /fleet revoke dialog for ${opened} scanned with ${String(await dialog.locator("button, a[href], input").count())} control(s) inside it`,
  );
});

test("axe-core reports zero violations on the live-run surface after a completed run", async ({ page }) => {
  // Render everything the profile can produce and reach terminal, so every live panel present on this
  // profile — timeline, recovery, usage, artifacts, and on the fake profile the approval detail — is in the
  // scan. The real profile's surface is honestly thinner (DIV-UI-002) and is scanned as it actually is.
  await runToTerminal(page);
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});

test("keyboard navigation: skip link is the first stop and the run→approve flow works with no mouse", async ({ page }) => {
  await page.goto("/runs");

  // The FIRST Tab lands on the visible skip link (axe bypass-block, keyboard-first).
  await page.keyboard.press("Tab");
  const firstFocus = await page.evaluate(() => document.activeElement?.textContent ?? "");
  expect(firstFocus).toContain("Skip to main content");

  // Operate the core flow with the keyboard only: genuinely Tab to the run button (not .focus() — this
  // proves it is REACHABLE in the tab order) and activate it with Enter.
  await tabToTestId(page, "run-button");
  await expect(page.getByTestId("run-button")).toBeFocused();
  await page.keyboard.press("Enter");

  if (!IS_REAL) {
    // The approve half of the claim. On the real profile no run can reach an approval at all (DIV-UI-001),
    // so the keyboard-operability claim narrows there to the run flow — stated, not silently dropped.
    await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
    await tabToTestId(page, "approve-button");
    await expect(page.getByTestId("approve-button")).toBeFocused();
    await page.keyboard.press("Enter");
  }

  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 60_000 });

  // Status is NOT color-only: the terminal status carries a visible glyph + the word.
  //
  // U+2714 FOLLOWED BY U+FE0E, and the selector is asserted rather than tolerated. Unicode's emoji-data.txt
  // gives U+2714 the `Emoji` and `Extended_Pictographic` properties and omits it from `Emoji_Presentation`,
  // so a platform may render it as a COLOUR emoji glyph — which would put colour back as a carrier on the
  // one element that exists to avoid it, and change the glyph's width so a column of statuses stops lining
  // up. VS15 pins text presentation, and dropping it fails here rather than looking fine on the one machine
  // the suite ran on.
  const glyph = page.getByTestId("terminal-status").locator(".glyph");
  await expect(glyph).toHaveText("✔︎");
  await expect(page.getByTestId("terminal-status")).toContainText("completed");
});

// COVERAGE — DECLARED LAST ON PURPOSE, so it runs after every generated scan (workers: 1,
// fullyParallel: false, so declaration order is execution order).
//
// This is the assertion that makes an unscanned page impossible rather than unlikely: every path in
// lib/routes.ts must have been put through axe by the loop above. Adding a route without adding its scan is
// not a review question, it is a red test — and if the generated loop were ever replaced by hand-written
// tests that missed one, this is what would catch it. The count is printed so a reader can see it rise as
// E25's pages land.
test("every route lib/routes.ts declares was actually scanned by axe", () => {
  const declared = CONSOLE_ROUTES.map((r) => r.path).sort();
  // eslint-disable-next-line no-console -- the count is the evidence.
  console.log(`AXE ROUTE COVERAGE — ${scanned.size}/${declared.length} declared route(s) scanned: ${[...scanned].sort().join(", ")}`);
  expect(
    [...scanned].sort(),
    "a route declared in lib/routes.ts was never put through axe. This test asserts over the WHOLE file, so a " +
      "filtered run (--grep) fails it too — that direction is deliberate: an unscanned page must be a red test, " +
      "and 'the filter excluded the scans' is exactly the excuse a silent gap would hide behind.",
  ).toEqual(declared);
  expect(CONSOLE_ROUTES.every((r) => r.readyTestId !== "")).toBe(true);
  // AND THE DYNAMIC ONES, on the same terms. A pattern declared in DYNAMIC_CONSOLE_ROUTES that no scan
  // resolved is the same unscanned page, arriving through the one door CONSOLE_ROUTES cannot cover.
  const declaredDynamic = DYNAMIC_CONSOLE_ROUTES.map((r) => r.pattern).sort();
  // eslint-disable-next-line no-console -- the count is the evidence.
  console.log(`AXE DYNAMIC COVERAGE — ${scannedDynamic.size}/${declaredDynamic.length} declared pattern(s) scanned: ${[...scannedDynamic].sort().join(", ")}`);
  expect([...scannedDynamic].sort(), "a dynamic route declared in lib/routes.ts was never put through axe").toEqual(declaredDynamic);
  // AND EVERY DECLARED SECOND TAB WAS OPENED. Without this the `secondTab` field would be a declaration with
  // no consequence — a route could name a tab that no scan reaches and the suite would stay green, which is
  // the same unscanned-screen hole one level down.
  const declaredSecond = DYNAMIC_CONSOLE_ROUTES.filter((r) => r.secondTab !== undefined)
    .map((r) => r.pattern)
    .sort();
  // eslint-disable-next-line no-console -- the count is the evidence.
  console.log(`AXE SECOND-TAB COVERAGE — ${scannedSecondTabs.size}/${declaredSecond.length} declared second tab(s) opened and scanned: ${[...scannedSecondTabs].sort().join(", ")}`);
  expect([...scannedSecondTabs].sort(), "a route declares a second tab that no axe scan opened").toEqual(declaredSecond);
  // AND THE CREATE DIALOGS, DERIVED FROM THE SOURCE RATHER THAN COUNTED BY HAND. A `<FormDialog` mount in a
  // page is a surface that exists only after a click; if the number of them stops matching the rows in
  // FORM_DIALOGS, a dialog has shipped that no scan opens — which is the unscanned page in its newest
  // disguise, and the one this file exists to make impossible.
  // The walk moved to tests/constants.ts so the CONTRAST sweep can make the same assertion independently —
  // it opens these same five dialogs, and it used to be complete only because this file happened to run.
  const mounted = formDialogMountCount();
  const declaredDialogs = FORM_DIALOGS.length + PRIMITIVE_DIALOGS.length;
  // eslint-disable-next-line no-console -- the count is the evidence.
  console.log(`AXE DIALOG COVERAGE — ${scannedDialogs.size}/${String(declaredDialogs)} declared dialog(s) scanned; ${String(mounted)} FormDialog mount(s) under app/ and components/`);
  expect(scannedDialogs.size, "a dialog declared in FORM_DIALOGS or PRIMITIVE_DIALOGS was never opened by a scan").toBe(declaredDialogs);
  expect(
    mounted,
    "the tree mounts a number of FormDialogs that FORM_DIALOGS does not describe. A create form behind a " +
      "button is scanned by nothing until a test opens it, so a mount with no row here is an unscanned modal.",
  ).toBe(FORM_DIALOGS.length);

  // AND THE SAME ASSERTION FOR THE PRIMITIVE, which had none until now. The FormDialog walk above looks for
  // `<FormDialog` in `app/**/page.tsx` and is therefore blind to a `<Dialog>` inside `components/` — which
  // is the component this tree tells new code to prefer. See PRIMITIVE_DIALOGS in tests/constants.ts.
  const primitives = primitiveDialogMountCount();
  const accountedFor = PRIMITIVE_DIALOGS.length + PRIMITIVE_DIALOG_EXEMPT.length;
  // eslint-disable-next-line no-console -- the count is the evidence, and so are the exemptions.
  console.log(
    `AXE PRIMITIVE-DIALOG COVERAGE — ${String(primitives)} component(s) mount <Dialog>; ` +
      `${String(PRIMITIVE_DIALOGS.length)} scanned by the loop, ${String(PRIMITIVE_DIALOG_EXEMPT.length)} exempt: ` +
      PRIMITIVE_DIALOG_EXEMPT.map((e) => `${e.file} (${e.scannedBy})`).join("; "),
  );
  expect(
    primitives,
    "a component mounts the ui/Dialog primitive and appears in neither PRIMITIVE_DIALOGS nor " +
      "PRIMITIVE_DIALOG_EXEMPT. A modal behind a click is scanned by nothing until a test opens it, and the " +
      "FormDialog walk above cannot see this one — wrong component, wrong directory.",
  ).toBe(accountedFor);

  // A ROUTE WITH NO LEAD SENTENCE IS THE SAME OMISSION AS ONE WITH NO READINESS SIGNAL, and it fails the
  // same way. Every page in this console used to open with a panel or a wall of equally-weighted notes,
  // and the only h1 on any of them was the brand — so "which page is this and what is it for" had no
  // answer on screen. The list is where a route is declared; this is what keeps the answer required.
  expect(CONSOLE_ROUTES.filter((r) => r.lead.trim() === "").map((r) => r.path)).toEqual([]);
});
