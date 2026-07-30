import { expect, test, type Page } from "@playwright/test";

import { IS_REAL } from "./constants";
import { announceProfile, signIn, skipOnReal } from "./profile";

// THE MCP CONNECTION AND TOOL-APPROVAL SCREEN (E25 T7, plan §T7, CON-007).
//
// WHAT IS BEING PROVEN, and the order is the operator's: register a connection whose credential is a HANDLE
// chosen from a list → discover → read the DRAFT revisions WITH the descriptions the upstream wrote →
// approve one (publish, with the gate declaration) → pin it into a set → publish the set → read the set's
// CONTENTS back. Every step is a call the console makes through the relay to /v1/*, and two of those routes
// did not exist a commit ago (plan §3.6 D14).
//
// THE UNTRUSTED-TEXT LEG IS AN ATTACK, NOT AN ASSERTION. The fixture's discovered description carries a
// <script> and an <a href>, and the test counts ELEMENTS and NAVIGATIONS afterwards rather than reading the
// rendered string and calling it escaped. That is the discipline approval-queue.spec.ts uses on the model's
// arguments, applied to the other untrusted author — and it is why the payload is in the fixture rather than
// typed here: it has to arrive by the path a real server's text would take.
//
// WHY THE DISCOVERY LEGS SKIP ON THE REAL PROFILE (DIV-UI-008, and it is a measured row rather than an
// excuse): `discover` is the one control on this page that leaves the process, a compose stack has no
// upstream MCP server, and giving a hermetic test stack one would be a worse property than the one being
// recorded. The conformance sweep re-derives that row against the running real stack on every run. What does
// NOT skip runs first, below: the page, both empty states, both pickers, and the absence of any credential
// field.

announceProfile("mcp-tools.spec.ts");

/** open signs in and lands on the tools screen with its first panel rendered. */
async function open(page: Page): Promise<void> {
  await signIn(page);
  await page.goto("/tools");
  await expect(page.getByTestId("panel-mcp-connections")).toBeVisible();
}

/** register fills the connection form and submits it, returning the id the console reported. */
async function register(page: Page, name: string): Promise<string> {
  await page.getByTestId("mcp-name-input").fill(name);
  await page.getByTestId("mcp-url-input").fill("https://mcp.atlassian.example/v1/mcp");
  await page.getByTestId("mcp-create-button").click();
  const status = page.getByTestId("mcp-connection-status");
  await expect(status).toBeVisible();
  const text = (await status.textContent()) ?? "";
  const id = /mcpc_[a-z0-9_]+/.exec(text)?.[0] ?? "";
  expect(id, `the console reported no connection id: ${text}`).not.toBe("");
  return id;
}

/** discoverAll runs discovery for the connection the register step selected. */
async function discoverAll(page: Page): Promise<void> {
  await page.getByTestId("discover-button").click();
  await expect(page.getByTestId("discover-result")).toContainText("getJiraIssue");
}

/** chooseTool selects a lineage by the canonical name shown in the picker's option label. */
async function chooseTool(page: Page, canonical: string): Promise<void> {
  const select = page.getByTestId("tool-select");
  await expect(select).toBeVisible();
  // BY THE OPTION'S OWN VALUE, read off the option whose text carries the canonical name. The id is minted
  // by the fixture and the label is `<canonical> (<id>)`, so this is the only way to select without either
  // hardcoding an id or matching a label by shape.
  const option = select.locator("option", { hasText: canonical });
  await expect(option).toHaveCount(1);
  const value = await option.getAttribute("value");
  expect(value, `no option in the tool picker carries ${canonical}`).not.toBeNull();
  await select.selectOption(value ?? "");
}

/**
 * onlyRevisionID resolves the id of the single revision the chosen tool has.
 *
 * IT WAITS ON THE DESCRIPTION, not on the row count, and that is the fix for a real defect this spec found:
 * switching lineages used to leave the PREVIOUS tool's revisions on screen until the next fetch returned, so
 * "there is exactly one row" was satisfied by the row that was already there — and the id read back belonged
 * to another tool. RevisePublish now clears while it loads; this waits for the text that identifies the
 * lineage anyway, because a count is not an identity.
 */
async function onlyRevisionID(page: Page, describes: string): Promise<string> {
  const cells = page.locator('[data-testid^="tool-revision-description-"]');
  await expect(cells).toHaveCount(1);
  await expect(cells.first()).toContainText(describes);
  const testId = (await cells.first().getAttribute("data-testid")) ?? "";
  return testId.replace("tool-revision-description-", "");
}

/** The first words of each fixture description, so a lineage can be identified from its own row. */
const READS = "Get the details of a Jira issue";
const WRITES = "Move a Jira issue to another status";

// --- BOTH PROFILES ---------------------------------------------------------------------------------------

test("the tools screen says which decision it is, and offers nowhere to type a credential", async ({ page }) => {
  await open(page);

  // THE DISTINCTION §3.6 D23 EXISTS FOR, on the screen rather than only in a doc comment: this page decides
  // once per tool and shows the server's description BECAUSE that is what is being decided; /approvals
  // decides one call and does not carry the description at all.
  await expect(page.getByTestId("tools-registration-note")).toContainText("registration screen, not the approval queue");
  await expect(page.getByTestId("tools-registration-note")).toContainText("not on that screen at all");
  await expect(page.getByTestId("tools-untrusted-note")).toContainText("untrusted text");
  await expect(page.getByTestId("tools-correction-note")).toContainText("no way to change or remove a connection");
  // The two-read-routes-are-not-a-flow ceiling, in words.
  await expect(page.getByTestId("tools-correction-note")).toContainText("nothing records who published");

  // NO CREDENTIAL BOX ANYWHERE IN THE REGISTRATION FORM. Enumerated rather than sampled: a `secret` or
  // `token` field added later would have to appear in this list, and the list is exact.
  const form = page.getByTestId("mcp-connection-form");
  await expect(form.locator("input[type=password]")).toHaveCount(0);
  const names = await form.locator("input[type=text], textarea").evaluateAll((els) =>
    els.map((el) => (el as HTMLInputElement | HTMLTextAreaElement).name).sort(),
  );
  expect(names, "a free-text field appeared in the MCP registration form that this spec does not know about").toEqual([
    "mcp-name",
    "mcp-url",
  ]);

  // THE SECRET REF IS CHOSEN, NEVER TYPED — and this is asserted state-independently, because the fixture's
  // secret-refs collection is non-empty while a bootstrap compose stack's is EMPTY. A spec that required
  // options would be a skip in disguise on the real profile, and one that required the note would be a skip
  // in disguise on the fake one.
  const select = page.getByTestId("mcp-secret-select");
  const note = page.getByTestId("mcp-secret-select-empty");
  const shown = (await select.count()) + (await note.count());
  expect(shown, "the secret-ref control is neither a select nor the no-refs note — a free-text fallback is the one thing it may not be").toBe(1);
});

test("both pickers refuse to exist when there is nothing to choose, and say what to do instead", async ({ page }) => {
  await open(page);

  // On the REAL profile these are the genuine first-day states (no connections, no tools). On the fake
  // profile they are the same states, because the fixture's registry starts empty too — which is the point
  // of it starting empty.
  if (IS_REAL) {
    await expect(page.getByTestId("discover-connection-select")).toHaveCount(0);
    await expect(page.getByTestId("discover-connection-select-empty")).toContainText("Register a connection first");
    await expect(page.getByTestId("tool-select")).toHaveCount(0);
    await expect(page.getByTestId("tool-select-empty")).toContainText("Discover a connection first");
  } else {
    // The fake profile's registry is stateful and other tests in this file fill it, so the assertion here is
    // the INVARIANT rather than the empty state: exactly one of the control and its note exists, never both
    // and never a text box.
    for (const id of ["discover-connection-select", "tool-select"]) {
      const both = (await page.getByTestId(id).count()) + (await page.getByTestId(`${id}-empty`).count());
      expect(both, `${id} is neither a select nor a note`).toBe(1);
    }
  }
  // In neither case may a lineage id be typed. The revisions surface is absent until one is chosen.
  await expect(page.locator('input[name="tool-id"]')).toHaveCount(0);
});

// --- THE DISCOVERY CHAIN (fixture profile — DIV-UI-008) --------------------------------------------------

test("an MCP connection is registered, discovered, approved with a gate, and pinned into a published set", async ({ page }) => {
  skipOnReal("DIV-UI-008");
  await open(page);

  const id = await register(page, "jira");
  await expect(page.getByTestId("panel-mcp-connections")).toContainText(id);
  // REGISTERING DIALS NOTHING, and the screen says so — the operator's next step is a separate control.
  await expect(page.getByTestId("mcp-connection-status")).toContainText("Nothing has been dialled yet");

  await discoverAll(page);
  // DISCOVERY LEAVES DRAFTS. Nothing is advertised to a model by finding it.
  await expect(page.getByTestId("panel-tools")).toContainText("mcp.jira.getJiraIssue");
  await chooseTool(page, "mcp.jira.getJiraIssue");
  const revID = await onlyRevisionID(page, READS);
  await expect(page.getByTestId(`revision-state-${revID}`)).toHaveText("draft");

  // APPROVE IT, WITH THE OPERATOR'S OWN SENTENCE. The label rides the publish — there is no second ceremony.
  await page.getByTestId(`gate-label-${revID}`).fill("the shared Jira account may read PAL tickets");
  await page.getByTestId(`publish-${revID}`).click();
  await expect(page.getByTestId("tool-revision-status")).toHaveAttribute("data-revision-id", revID);
  await expect(page.getByTestId(`revision-state-${revID}`)).toHaveText("published");
  // AND THE DECLARATION READS BACK, which is what makes an approved tool auditable rather than asserted.
  await expect(page.getByTestId(`tool-revision-gate-${revID}`)).toContainText("required");
  await expect(page.getByTestId(`tool-revision-gate-${revID}`)).toContainText("the shared Jira account may read PAL tickets");
  // Publishing is irreversible, so the control is GONE rather than disabled.
  await expect(page.getByTestId(`publish-${revID}`)).toHaveCount(0);

  // PIN AND PUBLISH THE SET.
  await page.getByTestId(`pin-${revID}`).click();
  await expect(page.getByTestId("tool-set-pins-list")).toContainText(revID);
  await page.getByTestId("set-name-input").fill("jira");
  await page.getByTestId("tool-set-revision-create-button").click();
  const created = page.getByTestId("tool-set-revision-status");
  await expect(created).toBeVisible();
  const setRevID = /tsrev_[a-z0-9_]+/.exec((await created.textContent()) ?? "")?.[0] ?? "";
  expect(setRevID, "the console reported no set revision id").not.toBe("");
  await page.getByTestId(`publish-${setRevID}`).click();
  await expect(page.getByTestId(`revision-state-${setRevID}`)).toHaveText("published");

  // THE SET'S CONTENTS — the second route this task wrote. The LIST shows a digest, and a digest tells you
  // two sets differ without telling you how; this is the only place the public API says WHICH revisions a
  // set grants.
  await page.getByTestId(`contents-${setRevID}`).click();
  await expect(page.getByTestId("tool-set-contents-summary")).toContainText("published");
  await expect(page.getByTestId("tool-set-contents-summary")).toContainText("pins 1 tool revision");
  await expect(page.getByTestId("tool-set-contents-list")).toContainText(revID);
});

test("a tool description written by the upstream server is inert — no element, no navigation, no execution", async ({ page }) => {
  skipOnReal("DIV-UI-008");
  await open(page);

  // A NAVIGATION WOULD BE A SILENT PASS OTHERWISE, so it is watched from before the payload is on screen.
  const navigations: string[] = [];
  page.on("framenavigated", (frame) => navigations.push(frame.url()));

  await register(page, "hostile");
  await discoverAll(page);
  await chooseTool(page, "mcp.hostile.getJiraIssue");
  const revID = await onlyRevisionID(page, READS);

  const cell = page.getByTestId(`tool-revision-description-${revID}`);
  // The payload IS on the page — an assertion that it is absent would pass on a page that simply failed.
  await expect(cell).toContainText("<script>");
  await expect(cell).toContainText('<a href="https://evil.example/pwned">');

  // …and it is TEXT. No element came of it, in the cell or anywhere else on the document.
  expect(await cell.locator("script, a, img").count(), "the server's markup became an ELEMENT inside the description cell").toBe(0);
  expect(await page.locator('a[href*="evil.example"]').count(), "the server's link became a clickable anchor").toBe(0);
  expect(
    await page.evaluate(() => (window as unknown as { __mcp_xss_executed?: boolean }).__mcp_xss_executed),
    "a payload in an MCP server's tool description EXECUTED on the console's origin — the origin that holds the operator session and drives the relay",
  ).toBeUndefined();
  expect(
    navigations.filter((u) => u.includes("evil.example")),
    "the console navigated to a URL the upstream server's description named",
  ).toEqual([]);

  // The schema is rendered the same way, and for the same reason: it is the server's JSON.
  await expect(page.getByTestId(`tool-revision-schema-${revID}`)).toContainText("issueKey");
  expect(await page.getByTestId(`tool-revision-schema-${revID}`).locator("*").count(), "the input schema was rendered as markup").toBe(0);
});

test("the approval gate defaults ON, and un-gating a tool is a deliberate click the screen then states", async ({ page }) => {
  skipOnReal("DIV-UI-008");
  await open(page);

  await register(page, "gates");
  await discoverAll(page);

  // DEFAULT ON, FOR EVERY TOOL. The console cannot know which tool writes — the MCP specification says a
  // server's own annotations are untrusted and our client does not decode them — so the safe answer is the
  // default and un-gating is the operator's act. That is HIL-P5 made VISIBLE rather than closed: the gate
  // can still be removed, it just cannot be removed by accident.
  await chooseTool(page, "mcp.gates.transitionJiraIssue");
  const writeID = await onlyRevisionID(page, WRITES);
  await expect(page.getByTestId(`gate-toggle-${writeID}`)).toBeChecked();
  await page.getByTestId(`publish-${writeID}`).click();
  await expect(page.getByTestId(`tool-revision-gate-${writeID}`)).toContainText("required");
  // No label was written, so the screen says what the approval screen will say — honestly, not blankly.
  await expect(page.getByTestId(`tool-revision-gate-${writeID}`)).toContainText("(no operator label)");

  // UN-GATING IS A CLICK, and what it produces is stated in words rather than by the absence of a word.
  await chooseTool(page, "mcp.gates.getJiraIssue");
  const readID = await onlyRevisionID(page, READS);
  await expect(page.getByTestId(`gate-toggle-${readID}`)).toBeChecked();
  await page.getByTestId(`gate-toggle-${readID}`).uncheck();
  await page.getByTestId(`publish-${readID}`).click();
  await expect(page.getByTestId(`tool-revision-gate-${readID}`)).toContainText("NOT required");
  await expect(page.getByTestId(`tool-revision-gate-${readID}`)).toContainText("no human in the loop");
});

test("a refused publish is the server's own sentence on screen, and a draft cannot be pinned at all", async ({ page }) => {
  skipOnReal("DIV-UI-008");
  await open(page);

  await register(page, "drafts");
  await discoverAll(page);
  await chooseTool(page, "mcp.drafts.getJiraIssue");
  const revID = await onlyRevisionID(page, READS);

  // A DRAFT CANNOT BE PINNED, and the control does not exist rather than existing and failing: publishing IS
  // the approval, and the API refuses a draft pin outright.
  await expect(page.getByTestId(`pin-${revID}`)).toHaveCount(0);

  // A REFUSED PUBLISH HAS SOMEWHERE TO LAND. This surface renders no create form (a discovered revision is
  // made by discovery), and the error region used to live inside that form — so this refusal would have been
  // swallowed entirely. It is reachable: the operator label is capped at 300 characters
  // (extensions.MaxApprovalLabelLen) and a longer one is a 400, never a silently truncated sentence on a
  // 2am approval screen.
  await page.getByTestId(`gate-label-${revID}`).fill("x".repeat(301));
  await page.getByTestId(`publish-${revID}`).click();
  const refusal = page.getByTestId("tool-revision-error");
  await expect(refusal).toBeVisible();
  await expect(refusal).toContainText("unsupported field");
  // NOTHING WAS PUBLISHED BY THE ATTEMPT — the refusal is not cosmetic.
  await expect(page.getByTestId(`revision-state-${revID}`)).toHaveText("draft");

  // And the same revision publishes once the label fits, so the refusal above was about the label and not
  // about the surface being broken.
  await page.getByTestId(`gate-label-${revID}`).fill("read-only access to PAL tickets");
  await page.getByTestId(`publish-${revID}`).click();
  await expect(page.getByTestId(`revision-state-${revID}`)).toHaveText("published");
  await expect(page.getByTestId(`pin-${revID}`)).toBeVisible();
});
