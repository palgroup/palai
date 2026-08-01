import { expect, test, type Page } from "@playwright/test";

import { CONSOLE_ROUTES, DYNAMIC_CONSOLE_ROUTES } from "../lib/routes";
import {
  FORM_DIALOGS,
  formDialogMountCount,
  PRIMITIVE_DIALOG_EXEMPT,
  PRIMITIVE_DIALOGS,
  primitiveDialogMountCount,
} from "./constants";
import { announceProfile, concreteDynamicPath, signIn } from "./profile";

// WHAT AXE CANNOT SEE, MEASURED ON THE RENDERED PAGE (console design pass, spec §2.1 / §4.4 / §7).
//
// tests/a11y.spec.ts is the scanner leg and it is honest about its ceiling. This is the leg for the two
// things that ceiling excludes, and BOTH were live violations on `main` when this file was written:
//
//   1. SC 1.4.11 Non-text Contrast on a CONTROL'S OWN BOUNDARY. axe-core has no rule for it — `color-contrast`
//      compares TEXT to its background and nothing else — so a console whose inputs and buttons take their
//      background from the page has its only visible boundary in a border no scanner looks at. Measured on
//      main: `#d0d7de` on `#ffffff` is 1.45:1 against a required 3:1, and the dark pair `#30363d` on
//      `#0d1117` is 1.55:1. The suite was green and the criterion was failing on every form in the console.
//
//   2. THE SKIP LINK, which is off-screen at `left: -9999px` until it is focused. axe skips hidden elements
//      and no scan in this suite ever focused it, so the FIRST Tab stop on every page — the one control the
//      whole keyboard proof is built around — was outside every measurement ever taken. Measured on main in
//      dark: `#ffffff` on `#4aa3ff` is 2.63:1 against a required 4.5:1, because `.skip-link` hard-codes
//      `color: #fff` while `--accent` lightens in the dark block.
//
// THE FORMULA IS WCAG 2.2's OWN, applied here rather than asserted from a table: relative luminance
// L = 0.2126R + 0.7152G + 0.0722B over sRGB-linearised channels (threshold 0.04045), ratio
// (L1 + 0.05) / (L2 + 0.05). https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html
//
// IT MEASURES THE PAGE, NOT THE TOKEN TABLE. A test that recomputed the design spec's own colour pairs would
// pass while a rule used the wrong token — the failure mode is not "the palette is wrong", it is "this
// control does not use the palette". So every value below comes from getComputedStyle on a real element on a
// real route, and the surface is resolved by WALKING UP the ancestor chain to the first opaque background,
// which is what the eye actually sees behind a transparent control.
//
// AND IT RUNS IN BOTH COLOUR-SCHEME PROJECTS, which is the other half: these are two different palettes and
// only one of them had ever been looked at.
test.beforeAll(() => announceProfile("contrast.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/** A control's measured boundary, as read off the rendered page. */
interface Boundary {
  route: string;
  tag: string;
  testId: string;
  /** The control's border colour against the surface behind it, or null when it draws no border. */
  border: number | null;
  /** The control's own fill against that surface — a solid button carries its boundary this way. */
  fill: number | null;
}

/** relativeLuminance implements WCAG 2.x's own definition over an sRGB triple. */
function relativeLuminance([r, g, b]: [number, number, number]): number {
  const channel = (v: number) => {
    const s = v / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

/** ratio is (L1 + 0.05) / (L2 + 0.05) with the lighter colour on top, rounded the way the spec tables are. */
export function contrastRatio(a: string, b: string): number {
  const la = relativeLuminance(parseRGB(a));
  const lb = relativeLuminance(parseRGB(b));
  const [hi, lo] = la >= lb ? [la, lb] : [lb, la];
  return Math.round(((hi + 0.05) / (lo + 0.05)) * 100) / 100;
}

/**
 * parseRGB reads the `rgb(r, g, b)` / `rgba(r, g, b, a)` form getComputedStyle serialises to.
 *
 * A partially transparent colour is REFUSED rather than blended against a guess: alpha compositing depends on
 * what is behind it, and a test that quietly assumed white would report a ratio nobody sees. Nothing in this
 * console draws a control boundary in a translucent colour, so this throwing is the guard for the day one does.
 */
function parseRGB(value: string): [number, number, number] {
  const m = /^rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)\s*(?:[,/]\s*([\d.]+)\s*)?\)$/.exec(value.trim());
  if (m === null) throw new Error(`not a computed rgb colour: ${value}`);
  if (m[4] !== undefined && Number(m[4]) < 1) throw new Error(`a translucent colour cannot be measured on its own: ${value}`);
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

/**
 * measureBoundaries reports, for every visible form control on the current page, the contrast its border and
 * its own fill each carry against the first opaque surface behind it.
 *
 * `elementFromPoint` is deliberately NOT used: a control inside a scroll container may be off-screen while
 * still being a control the operator will reach. The ancestor walk is the property SC 1.4.11 is about.
 */
async function measureBoundaries(page: Page, route: string): Promise<{ boundaries: Boundary[]; exempted: string[] }> {
  const raw = await page.evaluate(() => {
    const opaqueBehind = (el: Element): string => {
      for (let node: Element | null = el.parentElement; node !== null; node = node.parentElement) {
        const bg = getComputedStyle(node).backgroundColor;
        const alpha = /rgba?\([^)]*[,/]\s*([\d.]+)\s*\)/.exec(bg);
        if (bg !== "transparent" && (alpha === null || Number(alpha[1]) > 0)) return bg;
      }
      return getComputedStyle(document.documentElement).backgroundColor;
    };
    const exempted: string[] = [];
    const out: { tag: string; testId: string; border: string | null; fill: string | null; surface: string }[] = [];
    for (const el of document.querySelectorAll("input, select, textarea, button")) {
      const style = getComputedStyle(el);
      const box = el.getBoundingClientRect();
      // A control that is not rendered has no boundary to judge, and a DISABLED one is exempt by SC 1.4.11's
      // own text ("inactive user interface components"). Both are skipped rather than measured and excused.
      if (style.display === "none" || style.visibility === "hidden" || box.width === 0 || box.height === 0) continue;
      if ((el as HTMLInputElement).disabled) continue;
      // AND A NODE THAT IS NOT A USER INTERFACE COMPONENT AT ALL — which is a narrower exemption than it
      // looks, and it is recorded rather than silent (see `exempted` below).
      //
      // SC 1.4.11 judges "user interface components", defined in WCAG's glossary as "parts of the content
      // that are perceived by users as a single control for a distinct function". An element carrying BOTH
      // aria-hidden="true" AND tabindex="-1" is perceived by nobody: it is out of the accessibility tree for
      // a screen reader and out of the tab order for a keyboard, so there is no user for whom it is a
      // control. Requiring a visible boundary on it would be requiring one on something no one can see.
      //
      // WHAT MADE THIS NECESSARY, MEASURED: @base-ui/react's Select renders a form-serialisation <input>
      // (select/root/SelectRoot.mjs:470) styled with its own `visuallyHidden` — clipPath inset(50%),
      // border 0, 1x1px. It is 1px rather than 0px, so the size guard above does not catch it, and its UA
      // background against the page scores 1.03:1. Before this rule the sweep reported 16 such nodes as
      // failing controls. They are not controls; the seven TRIGGERS are, they are <button>s, and every one
      // is still measured — which the count printed below is the evidence for.
      //
      // BOTH CONDITIONS ARE REQUIRED. aria-hidden alone would exempt a focusable control that is merely
      // mislabelled — a real defect this sweep should keep reporting — and tabindex="-1" alone would exempt
      // a control reachable by click and announced by a screen reader.
      if (el.getAttribute("aria-hidden") === "true" && el.getAttribute("tabindex") === "-1") {
        exempted.push(`${el.tagName.toLowerCase()}[${el.getAttribute("id") ?? el.getAttribute("name") ?? ""}]`);
        continue;
      }
      const surface = opaqueBehind(el);
      const drawsBorder = style.borderTopStyle !== "none" && parseFloat(style.borderTopWidth) > 0;
      const own = style.backgroundColor;
      const filled = own !== "transparent" && !/rgba\([^)]*,\s*0\s*\)/.test(own) && own !== surface;
      out.push({
        tag: el.tagName.toLowerCase(),
        testId: el.getAttribute("data-testid") ?? el.getAttribute("id") ?? el.getAttribute("type") ?? "",
        border: drawsBorder ? style.borderTopColor : null,
        fill: filled ? own : null,
        surface,
      });
    }
    return { out, exempted };
  });
  return {
    boundaries: raw.out.map((r) => ({
      route,
      tag: r.tag,
      testId: r.testId,
      border: r.border === null ? null : contrastRatio(r.border, r.surface),
      fill: r.fill === null ? null : contrastRatio(r.fill, r.surface),
    })),
    exempted: raw.exempted,
  };
}

// EVERY ROUTE THE CONSOLE DECLARES, PLUS /login. The login page is not in lib/routes.ts (it is outside the
// session gate, so the generated axe loop cannot reach it) and it is the console's front door — one password
// field and one button, whose boundaries are exactly what this measures.
const ROUTES = ["/login", ...CONSOLE_ROUTES.map((r) => r.path)];

test("every interactive control carries a 3:1 boundary against the surface behind it (SC 1.4.11)", async ({ page }) => {
  const measured: Boundary[] = [];
  // Every node the sweep DECLINED to judge, so the exemption above cannot grow without showing up in the
  // line this test prints. An exemption nobody can see is the shape of a green suite that measures nothing.
  const exempted: string[] = [];
  // The dynamic routes are resolved to concrete paths and swept with the rest (E29). A transcript screen
  // carries controls no static route does — a row-select per event, the Rendered/Raw pair, a tab strip — and
  // a compact bordered control is exactly the shape that fails this criterion while looking fine.
  const dynamic: string[] = [];
  for (const route of DYNAMIC_CONSOLE_ROUTES) dynamic.push(await concreteDynamicPath(page, route));
  for (const route of [...ROUTES, ...dynamic]) {
    await page.goto(route);
    // The route's own content, not a spinner: a page still loading renders no controls and would report a
    // clean sweep of nothing. `main` is always present; the controls are what is being counted below.
    await expect(page.locator("main")).toBeVisible();
    await page.waitForLoadState("networkidle");
    const swept = await measureBoundaries(page, route);
    measured.push(...swept.boundaries);
    exempted.push(...swept.exempted);
  }

  // EVERY CONTROL BEHIND A DIALOG, WHICH THIS SWEEP HAD STOPPED MEASURING ENTIRELY.
  //
  // The loop above walks routes with their dialogs CLOSED, so a control that moves behind a `+ Create X`
  // button leaves this measurement — and the report gets SMALLER and CLEANER while the coverage shrinks,
  // which is the worst way for a number to move. It is not hypothetical: the page-parity passes moved five
  // forms into dialogs (an agent name, eight repository-binding fields, an API-key mint, two fleet forms),
  // which is roughly forty controls, and app/globals.css:134 records that a `button.danger` inside one went
  // unmeasured for an entire epic exactly this way.
  //
  // The list is `FORM_DIALOGS` in tests/constants.ts, imported rather than re-typed — and it is CHECKED
  // AGAINST THE SOURCE right here, one line down, rather than only in tests/a11y.spec.ts.
  //
  // Those are two separate properties and this comment used to conflate them, which is the defect this file
  // spends its whole length refusing. Importing the rows made this sweep CONSISTENT with the axe loop; it did
  // not make it COMPLETE. A sixth dialog with no row would have reddened that loop and left this sweep
  // quietly measuring five of six, with nothing in this file to say so. A sweep that is correct only because
  // another file runs is a sweep whose coverage somebody else can withdraw — so the mount count is asserted
  // in both places, and one synthetic mount produces two failures rather than one.
  expect(
    formDialogMountCount(),
    "the tree mounts a number of FormDialogs that FORM_DIALOGS does not describe, so this sweep is measuring " +
      "some of them. A control behind a create button is measured by nothing until a test opens it.",
  ).toBe(FORM_DIALOGS.length);
  // AND THE PRIMITIVE'S OWN MOUNT COUNT, for the same reason and with the same independence: this sweep must
  // not be complete only because tests/a11y.spec.ts happens to run. A `<Dialog>` in components/ is invisible
  // to the FormDialog walk above — wrong component, wrong directory — so it needs its own assertion here.
  expect(
    primitiveDialogMountCount(),
    "a component mounts the ui/Dialog primitive that neither PRIMITIVE_DIALOGS nor PRIMITIVE_DIALOG_EXEMPT " +
      "describes, so this sweep is measuring some of them.",
  ).toBe(PRIMITIVE_DIALOGS.length + PRIMITIVE_DIALOG_EXEMPT.length);

  const beforeDialogs = measured.length;
  for (const d of [...FORM_DIALOGS, ...PRIMITIVE_DIALOGS]) {
    await page.goto(d.route);
    // The opener lives in a PANEL's head, so it does not exist until that panel has settled.
    await expect(page.getByTestId(d.open)).toBeVisible({ timeout: 15_000 });
    await page.getByTestId(d.open).click();
    await expect(page.getByTestId(d.dialog)).toBeVisible();
    const inside = await measureBoundaries(page, `${d.route} (${d.dialog})`);
    // THE POSITIVE CONTROL, PER DIALOG. A dialog that rendered nothing measurable would otherwise contribute
    // zero rows and pass — the same empty-haystack shape reveal-once.spec.ts was caught by this morning.
    expect(inside.boundaries.length, `${d.dialog} opened and offered no measurable control — this sweep would be judging an empty dialog`).toBeGreaterThan(1);
    measured.push(...inside.boundaries);
    exempted.push(...inside.exempted);
  }
  // eslint-disable-next-line no-console -- the delta is the evidence that opening them was worth doing.
  console.log(`CONTROL BOUNDARY DIALOGS — ${String(FORM_DIALOGS.length + PRIMITIVE_DIALOGS.length)} dialog(s) opened, ${String(measured.length - beforeDialogs)} control(s) that the closed-route walk never measured`);

  // A SWEEP THAT MEASURED NOTHING WOULD PASS. The count is the evidence that it looked at anything at all.
  expect(measured.length, "no control was measured on any route — the sweep found nothing to judge").toBeGreaterThan(10);

  const strongest = (b: Boundary) => Math.max(b.border ?? 0, b.fill ?? 0);
  const failing = measured.filter((b) => strongest(b) < 3);
  const worst = [...measured].sort((a, b) => strongest(a) - strongest(b))[0];
  // THE COUNT IS NOT A CONSTANT AND MUST NEVER BE ASSERTED ON — measured across the two colour-scheme
  // projects in one run: 415 controls / 146 inside dialogs in the first, 506 / 185 in the second. Same code,
  // same five dialogs. The fixture is STATEFUL and the first project populates it, so by the second several
  // `Picker`s have options and render as a `<select>` where they had rendered their `emptyNote` — a control
  // that exists because a collection stopped being empty.
  //
  // AND THAT ASYMMETRY IS THIS SWEEP'S ALONE, which is worth knowing before anybody tries to make it
  // symmetrical with the served-form sweep in tests/auth.spec.ts. That one derives a count too and CANNOT
  // drift this way: it reads server-rendered bytes with no session cookie, and every page here is a client
  // component whose data arrives in an effect — so no collection has been read at render time and no control
  // can appear because one filled up. Rendered property versus source property, the same line that decides
  // why that sweep does not need to open these dialogs at all. What is asserted below is `0 below 3:1`; the
  // number beside it is evidence of what was looked at, never a threshold.
  // eslint-disable-next-line no-console -- the numbers ARE the measurement; a bare pass/fail proves nothing.
  console.log(
    `CONTROL BOUNDARY SWEEP — ${String(measured.length)} control(s) on ${String(ROUTES.length + dynamic.length)} route(s) ` +
      `plus ${String(FORM_DIALOGS.length + PRIMITIVE_DIALOGS.length)} open dialog(s), ` +
      `${String(failing.length)} below 3:1; weakest ${worst.tag}[${worst.testId}] on ${worst.route} at ${String(strongest(worst))}:1`,
  );
  // eslint-disable-next-line no-console -- the exemptions are part of the measurement, not a footnote.
  console.log(
    `CONTROL BOUNDARY SWEEP — ${String(exempted.length)} node(s) exempted as aria-hidden + tabindex=-1: ${[...new Set(exempted)].sort().join(", ")}`,
  );
  expect(
    failing.map((b) => `${b.route} ${b.tag}[${b.testId}] border=${String(b.border)} fill=${String(b.fill)}`),
    "a control's visible boundary is below SC 1.4.11's 3:1. Inputs and buttons here take their background " +
      "from the page, so the border is the ONLY thing that says where the control is — and axe-core has no " +
      "rule that looks at it.",
  ).toEqual([]);
});

// WEIGHT 580 IS EITHER A WEIGHT OR IT IS 600 WEARING ITS NAME, AND ONLY THE BROWSER KNOWS WHICH (E30).
//
// The reference's type scale ships `400 / 500 / 580 / 600`, and 580 is the tell that it uses a VARIABLE font:
// a semibold genuinely between medium and bold rather than a jump past it. app/globals.css took that scale
// and this console's typeface is `system-ui`, which is a different font on every operating system — SF on
// macOS, Segoe UI Variable on Windows, whatever fontconfig resolves on Linux. Some of those carry a weight
// axis and some ship discrete faces, and a discrete stack ROUNDS 580 to the nearest cut it has.
//
// `getComputedStyle().fontWeight` cannot answer this: it reports the SPECIFIED value, so it says "580" on a
// machine that rendered 600 and on a machine that rendered a real 580. What can answer it is the rendered
// GEOMETRY — the same string, in the same family and size, at three weights, measured. If 580 is a weight,
// its advance width sits strictly between 500's and 600's. If it rounded, it is identical to one of them.
//
// IT IS A REPORT AND NOT A FLOOR, deliberately. A platform without a variable system font is not a defect
// this console can fix, and failing the suite on it would fail on the operator's laptop rather than on ours.
// What must not happen is the scale claiming an intermediate weight that nobody's screen has ever drawn —
// so the measurement is printed on every run, and a `580 rounded to 600` line in the log is the honest form
// of "we picked the nearest".
test("the type scale's 580 is measured on the rendered page rather than asserted from the token file", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator("main")).toBeVisible();

  const measured = await page.evaluate(() => {
    const family = getComputedStyle(document.body).fontFamily;
    const probe = document.createElement("div");
    // Off-screen but LAID OUT: `display:none` has no geometry to read, and `visibility:hidden` still boxes.
    probe.style.cssText = "position:absolute;left:-9999px;top:0;white-space:nowrap;font-size:15px;";
    probe.style.fontFamily = family;
    document.body.appendChild(probe);
    const width = (weight: number) => {
      const span = document.createElement("span");
      span.style.fontWeight = String(weight);
      // A string with the widest spread of stems and round forms in it — a weight axis moves these most.
      span.textContent = "Multiagent Skills Tools 0123456789";
      probe.appendChild(span);
      const w = span.getBoundingClientRect().width;
      span.remove();
      return Math.round(w * 100) / 100;
    };
    const out = { family, w400: width(400), w500: width(500), w580: width(580), w600: width(600) };
    probe.remove();
    return out;
  });

  const distinct = measured.w580 !== measured.w500 && measured.w580 !== measured.w600;
  const verdict = distinct
    ? "580 renders as its own weight — the scale's intermediate semibold is real here"
    : measured.w580 === measured.w600
      ? "580 ROUNDED TO 600 on this platform — the scale's intermediate step is not drawn"
      : "580 ROUNDED TO 500 on this platform — the scale's intermediate step is not drawn";
  // eslint-disable-next-line no-console -- the measurement IS the deliverable; a pass/fail would prove nothing.
  console.log(
    `TYPE WEIGHT 580 — ${verdict}. Rendered widths at 15px in ${measured.family.split(",")[0]}: ` +
      `400=${String(measured.w400)}px 500=${String(measured.w500)}px 580=${String(measured.w580)}px 600=${String(measured.w600)}px`,
  );

  // THE ONLY THING ASSERTED IS THAT THE SCALE IS A SCALE. 400 and 600 must differ, or the stack resolved to a
  // single-weight face and every weight token in this file is decoration — which IS a defect, and a different
  // one from 580 rounding.
  expect(
    measured.w400,
    "400 and 600 render at the same width, so this font stack has one weight and the whole scale is inert",
  ).not.toBe(measured.w600);
});

test("the focus ring clears 3:1 against the surface, and the offset that puts it there still exists", async ({ page }) => {
  await page.goto("/login");
  const input = page.getByTestId("password-input");
  await input.focus();

  const ring = await page.evaluate(() => {
    const el = document.activeElement as HTMLElement;
    const style = getComputedStyle(el);
    let surface = "rgb(255, 255, 255)";
    for (let node: Element | null = el.parentElement; node !== null; node = node.parentElement) {
      const bg = getComputedStyle(node).backgroundColor;
      if (bg !== "transparent" && !/rgba\([^)]*,\s*0\s*\)/.test(bg)) {
        surface = bg;
        break;
      }
    }
    return { color: style.outlineColor, width: parseFloat(style.outlineWidth), offset: parseFloat(style.outlineOffset), border: style.borderTopColor, surface };
  });

  const againstSurface = contrastRatio(ring.color, ring.surface);
  const againstBorder = contrastRatio(ring.color, ring.border);
  // eslint-disable-next-line no-console -- see above.
  console.log(
    `FOCUS RING — ${String(ring.width)}px at ${String(ring.offset)}px offset; ${againstSurface.toFixed(2)}:1 vs the surface, ${againstBorder.toFixed(2)}:1 vs the control border`,
  );

  expect(ring.width, "a focus indicator with no width is not an indicator").toBeGreaterThan(0);
  // THE OFFSET IS LOAD-BEARING AND THIS IS THE LINE THAT SAYS SO. The ring sits at only ~1.4-1.8:1 against
  // the control's own border; what makes it conforming is that `outline-offset` moves it OFF that border and
  // onto the surface, where it clears 3:1. Setting the offset to 0 would make the ring non-conforming while
  // changing no colour and failing no other test in this repo.
  expect(ring.offset, "outline-offset is what puts the focus ring against the SURFACE rather than against the control's own border — removing it makes the ring non-conforming without changing a single colour").toBeGreaterThan(0);
  expect(againstSurface, "the focus ring is below 3:1 against the surface it sits on").toBeGreaterThanOrEqual(3);
});

test("the skip link — the first Tab stop, and the one element every scan has looked away from", async ({ page }) => {
  await page.goto("/");
  // FOCUSED, WHICH IS THE WHOLE POINT. It lives at left:-9999px until then, and axe skips what is off-screen,
  // so this is the state no automated scan in this suite has ever examined.
  await page.keyboard.press("Tab");
  const link = page.locator(".skip-link");
  await expect(link).toBeFocused();

  const pair = await page.evaluate(() => {
    const el = document.querySelector(".skip-link") as HTMLElement;
    const style = getComputedStyle(el);
    return { color: style.color, background: style.backgroundColor, left: style.left };
  });
  expect(pair.left, "the skip link did not come on screen when focused").not.toContain("-9999");

  const measured = contrastRatio(pair.color, pair.background);
  // eslint-disable-next-line no-console -- see above.
  console.log(`SKIP LINK — ${pair.color} on ${pair.background} = ${measured.toFixed(2)}:1`);
  expect(
    measured,
    "the skip link's text is below SC 1.4.3's 4.5:1. It is the first Tab stop on every page in this console, " +
      "and it is off-screen during every axe scan — so nothing else in this suite can fail for it.",
  ).toBeGreaterThanOrEqual(4.5);
});
