import { expect, test } from "@playwright/test";

import { CONSOLE_ROUTES } from "../lib/routes";
import { announceProfile, signIn } from "./profile";

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

test("no rendered sentence has a space the JSX transform ate", async ({ page }) => {
  const found: string[] = [];
  let scanned = 0;
  for (const route of ["/login", ...CONSOLE_ROUTES.map((r) => r.path)]) {
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
  }
  // eslint-disable-next-line no-console -- the volume scanned is what makes "none found" mean anything.
  console.log(`RENDERED COPY SWEEP — ${String(scanned)} characters over ${String(CONSOLE_ROUTES.length + 1)} route(s), ${String(found.length)} glued`);
  expect(found, "an em dash is touching a word. Every em dash in this console's copy is spaced, so this is a space the JSX transform trimmed away — add an explicit {\" \"}.").toEqual([]);
});
