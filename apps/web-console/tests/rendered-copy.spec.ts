import { expect, test } from "@playwright/test";

import { CONSOLE_ROUTES, DYNAMIC_CONSOLE_ROUTES } from "../lib/routes";
import { announceProfile, concreteDynamicPath, signIn } from "./profile";

// WORDS THAT ARRIVE GLUED TOGETHER, AND THE COMPILER IS THE ONE DOING IT (console design pass).
//
// JSX does not preserve every space an author typed. A text child that starts on the same line as a closing
// tag and runs onto the next line has its leading whitespace trimmed by the transform, so
//
//     <strong>Publication approvals are not here</strong> — a push, a pull
//     request or a merge is approved …
//
// ships as "…are not here— a push". The source is correct-looking, the review reads correctly, prettier is
// happy, and the sentence renders wrong. It is invisible to every test in this suite: axe does not read
// prose, and every assertion about these notes uses toContainText on a fragment that happens to sit inside
// one line.
//
// THIS IS THE THIRD TIME. app/approvals/page.tsx already carries a comment about the same transform gluing
// "every 10seconds", fixed there with a template expression. Two more shipped anyway, on main, before this
// pass touched anything. So it gets a guard rather than a third fix.
//
// THE RULE IS THE HOUSE CONVENTION, MEASURED RATHER THAN ASSUMED: every em dash in this console's copy is
// spaced (" — ") or begins a placeholder value ("— none (public)", "— inherited"). A letter or digit
// touching one is therefore a lost space, and the sweep is over what the BROWSER renders, not over the
// source — which is the only place the defect exists.
test.beforeAll(() => announceProfile("rendered-copy.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

const GLUED = /[\p{L}\p{N}]—|—[\p{L}\p{N}]/gu;

// A FOURTH ONE SHIPPED, AND THIS GUARD WATCHED IT GO PAST (console design pass). `/policy` rendered
//
//     This form writes the WHOLE policy.The five fields you see here are…
//
// from a source line that ends `</strong> The five fields you see here are,` and wraps — the same transform,
// the same lost space, on the page this file's own commit landed beside. The em-dash pattern above could
// never see it, because the character the space vanished next to was a FULL STOP.
//
// So the sweep gains a second pattern: a sentence terminator with a capital letter welded to it. It is run
// over PROSE ONLY — text inside <code>, <pre>, <kbd> and <samp> is excluded, because a machine value is
// exactly where `middleware.Scope.HasScope` and `identity/store.go` legitimately look like this — and that
// exclusion is the reason it needs its own walk instead of another regex over innerText.
const RUN_ON = /[\p{Ll}\p{N}][.!?][\p{Lu}]/gu;

/**
 * proseText is what <main> renders MINUS the machine values, read with innerText's block awareness.
 *
 * IT IS innerText AND NOT A TEXT-NODE WALK, and that distinction is the whole test. A walk that joins text
 * nodes concatenates ACROSS BLOCK BOUNDARIES — a heading's last word welds onto the paragraph under it — and
 * the first version of this arm reported 140 of those on a clean tree. innerText inserts the line breaks a
 * reader sees, so what remains adjacent in the string is what is adjacent on the screen.
 *
 * The code elements are blanked in the live DOM and restored, because there is no read-only way to ask for
 * "innerText without these subtrees". Excluding them is not tidiness: a machine value is exactly where
 * `middleware.Scope.HasScope` and `identity/store.go` legitimately look like a missing space.
 */
async function proseText(page: import("@playwright/test").Page): Promise<string> {
  return page.evaluate(() => {
    const main = document.querySelector("main");
    if (main === null) return "";
    const machine = [...main.querySelectorAll("code, pre, kbd, samp")];
    const saved = machine.map((el) => el.textContent ?? "");
    for (const el of machine) el.textContent = " ";
    const text = (main as HTMLElement).innerText;
    machine.forEach((el, i) => (el.textContent = saved[i]));
    return text;
  });
}

test("no rendered sentence has a space the JSX transform ate", async ({ page }) => {
  const found: string[] = [];
  const runOn: string[] = [];
  let scanned = 0;
  let prose = 0;
  // The dynamic routes too (E29): the transcript screen renders more generated copy than any static page —
  // a caption, an empty state, a chip row — and it is the newest, which is where this transform bites.
  const dynamic: string[] = [];
  for (const route of DYNAMIC_CONSOLE_ROUTES) dynamic.push(await concreteDynamicPath(page, route));
  for (const route of ["/login", ...CONSOLE_ROUTES.map((r) => r.path), ...dynamic]) {
    await page.goto(route);
    await expect(page.locator("main")).toBeVisible();
    await page.waitForLoadState("networkidle");
    // innerText, not textContent: this is about what a READER sees, and a <details> that is closed renders
    // nothing to read. Its summary is still text, and its body is checked when something opens it.
    const text = await page.locator("main").innerText();
    scanned += text.length;
    for (const m of text.matchAll(GLUED)) {
      const at = m.index ?? 0;
      found.push(`${route}: …${text.slice(Math.max(0, at - 45), at + 45).replace(/\n/g, " ")}…`);
    }
    const words = await proseText(page);
    prose += words.length;
    for (const m of words.matchAll(RUN_ON)) {
      const at = m.index ?? 0;
      runOn.push(`${route}: …${words.slice(Math.max(0, at - 45), at + 45).replace(/\s+/g, " ")}…`);
    }
  }
  // eslint-disable-next-line no-console -- the volume scanned is what makes "none found" mean anything.
  console.log(
    `RENDERED COPY SWEEP — ${String(scanned)} characters over ${String(CONSOLE_ROUTES.length + 1 + dynamic.length)} route(s), ` +
      `${String(found.length)} glued; ${String(prose)} prose characters (code excluded), ${String(runOn.length)} run-on`,
  );
  expect(found, "an em dash is touching a word. Every em dash in this console's copy is spaced, so this is a space the JSX transform trimmed away — add an explicit {\" \"}.").toEqual([]);
  expect(runOn, "a sentence ends and the next one starts with no space between them. In prose that is always the JSX transform trimming a leading space off a text child that follows a closing tag — add an explicit {\" \"}. (Machine values are excluded from this arm, so a package path is not what tripped it.)").toEqual([]);
});
