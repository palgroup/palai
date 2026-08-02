import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, chooseOption, chooseOptionByLabel, chosenValue, signIn, skipOnReal } from "./profile";

// THE CONFIGURATION JOURNEY — a stranger stands up a repository and an agent from a screen, and RUNS it
// (E25 T6, plan §T6, CON-006).
//
// WHAT `palai up` LEAVES BEHIND, measured rather than assumed: with no Slack credential a fresh stack holds
// ZERO agents and ZERO repository bindings. Palai's promise is an agent that writes code; without a
// repository binding there is no coding run at all, and without an agent there is nothing to pin a run to.
// Both write routes have existed since E11/E09 and the read halves since E13 T4 — what was missing was a
// page, which is why this file's first run failed on "no such page" for every leg below.
//
// THE ORDER HERE IS THE PRODUCT'S ORDER, and it is one sentence: create → SELECT AN ENVIRONMENT → publish →
// RUN. The environment step is not decoration. T3 built the pipe that carries an environment's keys into an
// agent's shell and could only prove it against a hand-written revision row; T6 is what lets an operator
// write that revision, and the two together are the only reason either half is useful.
//
// WHAT THIS FILE CANNOT PROVE, on either profile, and where it IS proven: that the keys arrive in the
// SHELL. A browser cannot see a subprocess's environment, and no console profile reaches a tool call at all
// (the fake upstream scripts events; the compose fake adapter is hardcoded with no ToolCalls —
// DIV-UNX-001). The loop is closed end to end, through the SAME HTTP routes this page drives and with no
// SQL of its own, at the component tier:
// apps/control-plane/internal/execution/console_environment_run_component_test.go.
test.beforeAll(() => announceProfile("config-journey.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// Unique per run: on the real profile these collections ACCUMULATE and an agent name is not unique, but a
// spec that cannot tell its own row from a previous run's is a spec that passes on someone else's data.
const stamp = () => `${Date.now()}-${Math.floor(Math.random() * 10_000)}`;

// THE PINNED MODEL IS DISTINCTIVE ON PURPOSE. The assertion it exists for is "the terminal projection
// carries the model of the revision the run was pinned to", and a value that could also have come from the
// deployment default would make that assertion vacuous. `fake` is what both profiles report for an
// unpinned run, so this is deliberately not that.
const PINNED_MODEL = "t6-pinned-by-revision";

async function expectAxeClean(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  // A scan that ran ZERO rules reports zero violations too — identical output to a clean page.
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
}

// createEnvironment drives T4's screen, because that is the screen an operator uses and a spec that seeded
// an environment through the relay would be proving a pipe the product does not have.
async function createEnvironment(page: Page, name: string, keys: Record<string, string>): Promise<void> {
  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("env-name-input").fill(name);
  await page.getByTestId("env-create-button").click();
  await expect(page.getByTestId("environment-create-status")).toContainText(name, { timeout: 15_000 });
  for (const [key, value] of Object.entries(keys)) {
    await page.getByTestId("value-key-input").fill(key);
    await page.getByTestId("value-secret-input").fill(value);
    await page.getByTestId("value-write-button").click();
    await expect(page.getByTestId("environment-value-status")).toContainText(key, { timeout: 15_000 });
  }
}

// createAgentWithRevision walks the whole lineage from the /agents screen and returns the ids it observed.
// `publish` is a parameter because an UNPUBLISHED revision is one of the things under test: a draft that a
// run refuses is the console's proof that nothing silently fell back to a default.
//
// THE PATH CHANGED AND THE JOURNEY FOLLOWS IT (page-parity pass). The create form is behind the list's own
// `+ New agent` button as a dialog, and a created agent LANDS ON ITS OWN PAGE — /agents/{id} — which is
// where its revisions are drafted, published and diffed. The old walk filled a form on the list screen and
// then read the id out of a "choose an agent" dropdown; that dropdown existed because the revision form had
// no other way to know which lineage it belonged to, and a detail route is what removes the question.
//
// The id is READ OFF THE ADDRESS BAR rather than out of a control, which is the stronger reading of the same
// assertion: the console did not merely select the agent it made, it navigated to it.
async function createAgentWithRevision(
  page: Page,
  opts: { agentName: string; environmentName?: string; model?: string; publish: boolean },
): Promise<{ agentId: string; revisionId: string }> {
  await page.goto("/agents");
  await expect(page.getByTestId("panel-agent-profiles")).toBeVisible({ timeout: 15_000 });

  await page.getByTestId("agent-create-open").click();
  await expect(page.getByTestId("agent-create-dialog")).toBeVisible();
  await page.getByTestId("agent-name-input").fill(opts.agentName);
  await page.getByTestId("agent-create-button").click();

  await expect(page, "the create did not land on the agent it just made, so there is no lineage to revise").toHaveURL(/\/agents\/[^/?]+/, {
    timeout: 15_000,
  });
  const agentId = decodeURIComponent(new URL(page.url()).pathname.split("/").filter((s) => s !== "").pop() ?? "");
  expect(agentId, "the address bar carries no agent id after the create").not.toBe("agents");
  // The NAME the server minted the row with, read back on the screen the create landed on — the same
  // read-back discipline §2 demands of every write form.
  await expect(page.getByTestId("agent-title")).toContainText(opts.agentName, { timeout: 15_000 });
  await expect(page.getByTestId("agent-chips")).toBeVisible({ timeout: 15_000 });

  await page.getByTestId("revision-model-input").fill(opts.model ?? PINNED_MODEL);
  if (opts.environmentName !== undefined) {
    // FOUND BY ITS LABEL, selected by the value that label carries. The label is what an operator reads, so a
    // picker that offered the right id under the wrong name would fail here; going straight to the id would
    // not have noticed.
    await chooseOptionByLabel(page, "revision-environment-select", opts.environmentName);
  }
  await page.getByTestId("agent-revision-create-button").click();
  await expect(page.getByTestId("agent-revision-status")).toContainText("draft", { timeout: 15_000 });

  const revisionId = await page.getByTestId("agent-revision-status").getAttribute("data-revision-id");
  expect(revisionId, "the revision form did not report the id it created").toBeTruthy();

  if (opts.publish) {
    await page.getByTestId(`publish-${revisionId}`).click();
    await expect(page.getByTestId("agent-revision-status")).toContainText("published", { timeout: 15_000 });
  }
  return { agentId, revisionId: String(revisionId) };
}

// runWithRevision starts a run on /runs PINNED to one revision, through the two pickers an operator uses.
async function runWithRevision(page: Page, agentId: string, revisionId: string): Promise<void> {
  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });
  await chooseOption(page, "run-agent-select", agentId);
  await chooseOption(page, "run-revision-select", revisionId);
  await page.getByTestId("run-button").click();
}

// --- THE REPOSITORY BINDING ------------------------------------------------------------------------------

test("a repository binding is registered from the console and reads back on the list", async ({ page }) => {
  await page.goto("/repositories");
  await expect(page.getByTestId("panel-repository-bindings")).toBeVisible({ timeout: 15_000 });

  // THE FORM IS BEHIND THE LIST'S OWN BUTTON (page-parity pass). It used to be eight fields open on the page
  // under two standing paragraphs, so the list this screen is named for was the third thing on it.
  await page.getByTestId("binding-create-open").click();
  await expect(page.getByTestId("binding-create-dialog")).toBeVisible();

  const identity = `palai-example/console-t6-${stamp()}`;
  await page.getByTestId("binding-provider-input").fill("github");
  await page.getByTestId("binding-identity-input").fill(identity);
  await page.getByTestId("binding-clone-url-input").fill(`https://github.com/${identity}.git`);
  await page.getByTestId("binding-default-branch-input").fill("main");
  await page.getByTestId("binding-operations-input").fill("clone, push");
  await page.getByTestId("binding-classification-input").fill("internal");
  await page.getByTestId("binding-region-input").fill("eu-central-1");
  await page.getByTestId("binding-policy-input").fill('{"require_approval":true}');
  await page.getByTestId("binding-create-button").click();

  await expect(page.getByTestId("repository-binding-status")).toContainText("Registered", { timeout: 15_000 });

  // THE READ-BACK IS THE POINT (§2 forbids a write-and-pray form): the row the API returns carries the
  // authoritative identity, and the console shows it rather than echoing what was typed.
  const panel = page.getByTestId("panel-repository-bindings");
  await expect(panel).toContainText(identity, { timeout: 15_000 });
  await expect(panel).toContainText("main");

  // AND THE CEILING IS ON THE SCREEN: registering a binding did not clone anything. The sentence is on the
  // STATUS the create left behind, which is the one place an operator is certain to be looking at the moment
  // it matters.
  await expect(page.getByTestId("repository-binding-status")).toContainText("Nothing has been cloned");

  await expectAxeClean(page);

  // THE ROW OPENS, AND THE FOUR FIELDS A LIST CANNOT CARRY ARE THERE. Before this pass the console WROTE a
  // clone URL, a data classification, a region constraint and a policy object and showed none of them back —
  // the write-and-pray shape §2 forbids, arriving through the one door a list-only screen leaves open.
  await page.getByTestId("panel-repository-bindings").locator("tbody tr").first().getByTestId("binding-identity-link").click();
  await expect(page.getByTestId("panel-binding-record")).toBeVisible({ timeout: 15_000 });
  const record = page.getByTestId("binding-record");
  await expect(record).toContainText(identity);
  await expect(record).toContainText(`https://github.com/${identity}.git`);
  await expect(record).toContainText("internal");
  await expect(record).toContainText("eu-central-1");
  await expect(page.getByTestId("binding-policy")).toContainText("require_approval");
  await expect(page.getByTestId("binding-operations")).toContainText("clone");

  // AND THE SENTENCE THAT USED TO BE THE LIST'S SECOND PARAGRAPH IS HERE, which is where an operator looks
  // for the edit control that does not exist.
  await expect(page.getByTestId("binding-correction-note")).toContainText("no way to change or remove");
  await expect(page.getByTestId("binding-reachability-note"), "the reachability ceiling is in the create dialog now, not on the record").toHaveCount(0);

  await expectAxeClean(page);
});

test("the connection ref is a HANDLE chosen from the secret-ref list, never a typed value", async ({ page }) => {
  // STATE-INDEPENDENT, which is why the assertion is about the FORM's inputs rather than about the picker's
  // options: a fresh compose stack has an EMPTY secret_refs collection (nothing seeds one) while the fixture
  // and an accumulated real stack both have rows, so a spec that required options would be a skip in
  // disguise. What holds in every state is that no text field on this form takes a credential or a ref.
  await page.goto("/repositories");
  await expect(page.getByTestId("panel-repository-bindings")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("binding-create-open").click();

  const form = page.getByTestId("repository-binding-form");
  // THE SELECTOR EXCLUDES ONE NODE AND PROVES THE EXCLUSION RATHER THAN ASSERTING IT (E29 component layer).
  //
  // `input:not([type])` was written to catch a hand-rolled text box that forgot its type attribute. The
  // connection-ref control is now a components/ui/Select, and @base-ui/react's Select renders a
  // form-serialisation <input> with no type, carrying the chosen handle — so the enumeration below started
  // reporting `binding-connection-ref` as a free-text field, which is the opposite of what this test says.
  //
  // The claim is about what an operator can TYPE. So the exclusion is aria-hidden + tabindex="-1" — the same
  // pair tests/contrast.spec.ts exempts, for the same reason: no screen reader has it and no keyboard reaches
  // it. And the exclusion is CHECKED below rather than trusted, because "the node I excluded is unreachable"
  // is exactly the sentence that would hide a real free-text box the day the markup changes.
  const excluded = form.locator('input[aria-hidden="true"][tabindex="-1"]');
  for (const node of await excluded.all()) {
    expect(await node.evaluate((el) => (el as HTMLInputElement).readOnly || el.getAttribute("aria-hidden") === "true"), "an excluded node is not actually unreachable").toBe(true);
    // A 1x1 clipped box is not a control an operator can put a value into. If one ever renders at a size a
    // pointer could hit, this is what fails.
    const box = await node.boundingBox();
    expect((box?.width ?? 0) <= 1 && (box?.height ?? 0) <= 1, "an excluded input is large enough to be typed into").toBe(true);
  }
  const freeText = async (): Promise<string[]> =>
    (
      await form
        .locator('input[type="text"]:not([aria-hidden="true"]), input:not([type]):not([aria-hidden="true"]), textarea')
        .evaluateAll((els) => els.map((el) => el.getAttribute("data-testid") ?? el.getAttribute("name") ?? "<unnamed>"))
    ).sort();

  const BASE = [
    "binding-classification-input",
    "binding-clone-url-input",
    "binding-default-branch-input",
    "binding-identity-input",
    "binding-operations-input",
    "binding-policy-input",
    "binding-provider-input",
    "binding-region-input",
  ].sort();

  // THE DEFAULT STATE — *Repository access: None*, a public repository. No credential control exists in the
  // DOM at all, so the enumeration is the eight it always was.
  expect(
    await freeText(),
    "the binding form's free-text fields are enumerated: a connection_ref or a credential box among them would " +
      "be a value crossing a screen that is only allowed to carry a handle",
  ).toEqual(BASE);
  await expect(form.locator('input[type="password"]'), "no credential control before one is asked for").toHaveCount(0);

  // THE SEAL MODE — and this arm is why the test above is no longer the whole claim. The page can now take a
  // token, so "this screen takes no secret at all" (what this test used to assert, in one line, with the
  // dialog in its default state) stopped being a true description of the screen the moment sealing moved
  // here. What survives, and is asserted per mode below, is the property that sentence existed to protect:
  // A REF IS NEVER TYPED INTO A FREE-TEXT BOX AS A POINTER TO SOMETHING THAT MUST ALREADY EXIST.
  //
  // In this mode the operator does type a NAME — and that is not the defect the rule guards against. The
  // failure mode is a ref that names nothing, which fails at CLONE TIME inside a run with a refusal about git
  // authentication; a name typed HERE is CREATED by the same submit, so it cannot dangle. The credential
  // beside it is a password input and not a text one, and it is the only password input on the form.
  await chooseOption(page, "binding-connection-mode", "new");
  expect(
    await freeText(),
    "the seal mode adds exactly one free-text field and it is the credential's NAME — a second one would be a " +
      "value or a ref typed where neither belongs",
  ).toEqual([...BASE, "binding-connection-name-input"].sort());
  const credential = form.locator('input[type="password"]');
  await expect(credential, "the credential is one password input and there is exactly one").toHaveCount(1);
  await expect(credential).toHaveAttribute("data-testid", "binding-connection-token-input");
  // NEW-PASSWORD, NOT off: the risk this attribute addresses is the browser dropping the operator's saved
  // console password into the box and sealing it as a credential an agent will then use.
  await expect(credential).toHaveAttribute("autocomplete", "new-password");

  // THE PICKER ARM — reachable only when the organization HAS secret refs, because a mode that leads to an
  // empty dropdown is a dead end an operator has to back out of, so it is not offered. That replaces the old
  // `${testId}-empty` branch: what stands in place of an unsatisfiable control is now a mode that is absent
  // rather than a note under one that is present.
  const modeRows = await page.getByTestId("binding-connection-mode").getAttribute("data-value");
  expect(modeRows, "the mode control is a ui/Select and carries its value").toBe("new");
  await page.getByTestId("binding-connection-mode").click();
  const offersExisting =
    (await page.getByRole("listbox").locator('[role="option"][data-value="existing"]').count()) === 1;
  await page.keyboard.press("Escape");
  if (!offersExisting) {
    // A stack with no secret refs. Nothing more to check here — the seal mode above is the way forward, and
    // it was just proven to exist.
    return;
  }
  await chooseOption(page, "binding-connection-mode", "existing");
  const picker = page.getByTestId("binding-connection-select");
  {
    // The options are the API's OWN list of NAMES. A secret ref's read projection carries {name, version,
    // updated_at} and no value (identity/secrets.go secretRefView), so a name is all there is to offer.
    const listed = await page.evaluate(async () => {
      const res = await fetch("/api/palai/v1/secret-refs", { headers: { Accept: "application/json" } });
      return ((await res.json()) as { data?: { name: string }[] }).data?.map((r) => r.name) ?? [];
    });
    // READ WITH THE LISTBOX OPEN (E29 component layer). A native <select> kept its options in the DOM
    // whether or not it was open, so `picker.locator("option")` found them on a closed control; a listbox is
    // rendered by the popup and there is nothing to enumerate until it exists. Opening it is therefore not a
    // detour around the assertion — it is the assertion, and it now also proves the popup renders at all.
    await picker.click();
    // AND THE POPUP MUST HAVE ROWS BEFORE THEY ARE READ. `evaluateAll` is a SNAPSHOT with no auto-wait, so a
    // read taken between the click and the popup mounting returns [] and compares an empty list against a
    // non-empty one — or, if the expectation were the other way round, passes having looked at nothing. That is
    // the defect tests/reveal-once.spec.ts hit: a container visible before its rows resolved, and a positive
    // control that found nothing. `expect` retries; `evaluateAll` does not, so the wait is explicit.
    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    await expect(listbox.locator('[role="option"]')).not.toHaveCount(0);
    const offered = await listbox
      .locator('[role="option"]')
      .evaluateAll((els) => els.map((el) => el.getAttribute("data-value") ?? "").filter((v) => v !== ""));
    expect(offered.sort()).toEqual([...listed].sort());
  }
  // AND THE CREDENTIAL CONTROL IS GONE AGAIN. Leaving the seal mode must UNMOUNT the password input rather
  // than hide it: a credential field still in the DOM is a credential still in the page, and this is the
  // one transition an operator makes by accident (seal, change their mind, pick an existing ref instead).
  await expect(form.locator('input[type="password"]'), "leaving the seal mode left the credential input mounted").toHaveCount(0);
  expect(await freeText(), "leaving the seal mode left its name field behind").toEqual(BASE);
});

// --- THE AGENT LINEAGE ----------------------------------------------------------------------------------

test("an agent, a revision bound to an environment, and a publish — all from the console", async ({ page }) => {
  const envName = `t6-env-${stamp()}`;
  await createEnvironment(page, envName, { DEPLOY_TARGET: "staging.example.internal" });

  const { revisionId } = await createAgentWithRevision(page, {
    agentName: `t6-agent-${stamp()}`,
    environmentName: envName,
    publish: true,
  });

  // THE LINEAGE READS BACK, and the draft/published distinction is on the screen rather than in a colour:
  // publishing is IRREVERSIBLE (000019_agents.up.sql), so which state a revision is in is the difference
  // between a config an operator can still change and one they can only supersede.
  const rows = page.getByTestId("panel-agent-revisions");
  await expect(rows).toContainText(revisionId);
  await expect(rows).toContainText("published");
  await expect(rows).toContainText(PINNED_MODEL);
  // The environment reads back BY ID on the revision row — the field the API lets you write has a read path.
  await expect(rows.getByTestId(`revision-environment-${revisionId}`)).not.toBeEmpty();

  // BOTH EXTERNAL FIELDS READ BACK, ASSERTED AS CELLS RATHER THAN AS A SENTENCE (page-parity pass). This
  // screen used to close with a paragraph claiming it — "the MCP connection rider has been readable since
  // E22; the tool set, the half that actually GRANTS the tools, was write-only until E25 T7" — and a spec
  // that reads a paragraph proves the paragraph. The two cells are the claim: `tool_sets` is the GRANT and
  // `mcp_connections` is the CEILING, each absence fails quietly and differently, and a projection that
  // dropped either would fail here instead of leaving a true sentence over a missing column. The history is
  // in docs/operations/console.md §4c.
  await expect(rows.getByTestId(`revision-tool-sets-${revisionId}`)).not.toBeEmpty();
  await expect(rows.getByTestId(`revision-mcp-connections-${revisionId}`)).not.toBeEmpty();

  // The publish control is GONE from a published row: a second publish is not a thing an operator can ask
  // for on a lineage that cannot be un-published.
  await expect(page.getByTestId(`publish-${revisionId}`)).toHaveCount(0);

  // AND THE LINEAGE'S SUMMARY AGREES WITH ITS TABLE. The chip row is a SECOND read of the same collection
  // (the page reads it for the chips, RevisePublish reads it for the rows), so a publish that moved one and
  // not the other would leave a screen that contradicts itself — which is the failure a summary invites.
  await expect(page.getByTestId("chip-state")).toContainText("published");
  await expect(page.getByTestId("chip-model")).toContainText(PINNED_MODEL);

  await expectAxeClean(page);

  // THE COMPARE TAB IS THE SECOND SCREEN OF THIS PAGE and it is scanned as one. It carries the diff that
  // used to be panel five of five on the list screen, under four forms that had nothing to do with it.
  await page.getByTestId("tab-compare").click();
  await expect(page).toHaveURL(/segment=compare/);
  await expect(page.getByTestId("panel-agent-diff")).toBeVisible();
});

test("the environment picker on the revision form does not degrade to free text when there is nothing to pick", async ({ page }) => {
  // STUBBED AT THE RELAY BOUNDARY so the empty state is deterministic on BOTH profiles — it is the state a
  // real organization is in on its first day and can never be put back into. Everything else (the session
  // gate, the relay, the page's own fetch) still runs.
  //
  // ALL THREE COLLECTIONS ARE STUBBED SINCE E25 T7, and the third one is why: the revision form gained a
  // tool-set picker and an MCP-connection picker, and the FIXTURE's registry is stateful — tests/
  // mcp-tools.spec.ts fills it. Stubbing only /environments would have made this test's outcome depend on
  // WHICH FILE RAN FIRST, which is the exact trap E25 T4 and T6 each paid for once.
  for (const collection of ["environments", "tool-sets", "mcp-connections"]) {
    await page.route(`**/api/palai/v1/${collection}`, (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ object: "list", data: [] }) }),
    );
  }
  await page.goto("/agents");
  await expect(page.getByTestId("panel-agent-profiles")).toBeVisible({ timeout: 15_000 });

  // AN AGENT IS CREATED FIRST, because the revision form belongs to ONE lineage and lives on that lineage's
  // own page. Creating rather than picking an existing row is what makes this deterministic on both
  // profiles: a bootstrap compose stack holds zero agents.
  await page.getByTestId("agent-create-open").click();
  await page.getByTestId("agent-name-input").fill(`t6-empty-env-${stamp()}`);
  await page.getByTestId("agent-create-button").click();
  await expect(page).toHaveURL(/\/agents\/[^/?]+/, { timeout: 15_000 });
  await expect(page.getByTestId("agent-revision-form")).toBeVisible({ timeout: 15_000 });

  await expect(page.getByTestId("revision-environment-select")).toHaveCount(0);
  const empty = page.getByTestId("revision-environment-select-empty");
  await expect(empty).toContainText("Create an environment first");
  await expect(empty.getByRole("link")).toHaveAttribute("href", "/environments");

  // AND NOTHING TOOK ITS PLACE. The revision form's free-text fields are enumerated, so a hand-rolled
  // "environment id" box would fail here: a typo'd id is accepted by a form, refused at publish, and
  // reported as a refusal about something else entirely.
  const textFields = await page
    .getByTestId("agent-revision-form")
    .locator('input[type="text"], input:not([type]), textarea')
    .evaluateAll((els) => els.map((el) => el.getAttribute("data-testid") ?? el.getAttribute("name") ?? "<unnamed>"));
  expect(textFields.sort(), "an empty environment list degraded into a free-text box").toEqual(
    ["revision-instructions-input", "revision-model-input", "revision-tools-input"].sort(),
  );

  // THE SECOND CEILING WAS AN ABSENCE AND E25 T7 CLOSED IT, SO THIS ASSERTION IS REPAIRED RATHER THAN
  // DELETED — and what it got wrong is worth keeping. It named `revision-tool-sets-select` and
  // `revision-mcp-connections-select`, two testids that have never existed in this tree in any epic: the
  // controls T7 added are `revision-tool-set-select` and `revision-mcp-connection-select` (singular, one
  // choice each). So the loop asserted the absence of things that could not have been present, and it would
  // have kept passing after the controls it was watching for arrived. A `toHaveCount(0)` over a name nobody
  // owns is the shape of a guard that cannot fail.
  //
  // What is asserted now is the rule that DOES bind them, and it is the same rule as the environment
  // picker's: with nothing to choose, the control is not rendered at all and a note with a way forward
  // stands in its place. All three collections are stubbed empty above, so this is one rule measured three
  // times rather than a claim about which epic built what.
  for (const [control, note, wants] of [
    ["revision-tool-set-select", "revision-tool-set-select-empty", "Publish a tool set first"],
    ["revision-mcp-connection-select", "revision-mcp-connection-select-empty", "Register an MCP connection first"],
  ]) {
    await expect(page.getByTestId(control)).toHaveCount(0);
    await expect(page.getByTestId(note)).toContainText(wants);
    await expect(page.getByTestId(note).getByRole("link")).toHaveAttribute("href", "/tools");
  }
});

// --- THE LIST, AND WHAT A ROW SAYS ----------------------------------------------------------------------

test("every column on the Agents list is a field, and the row goes to the agent's own page", async ({ page }) => {
  // THE SCREEN THIS REPLACES had ONE column — Name, with the id stacked under it in mono — and the row went
  // nowhere. Measured on the built console 2026-07-31 (`document.querySelectorAll("main table thead th")`):
  // 1 header before, 7 after. The assertion below is not the count, it is that each one is a FIELD: the id
  // and the name come off the row GET /v1/agents returns, and the four lineage columns come off that agent's
  // own revisions collection — which is checked here against a SECOND read through the relay, so a column
  // that showed the wrong lineage's state would fail rather than merely look plausible.
  await page.goto("/agents");
  const panel = page.getByTestId("panel-agent-profiles");
  await expect(panel).toBeVisible({ timeout: 15_000 });

  const headers = await panel.locator("thead th").allInnerTexts();
  expect(headers.slice(0, 6), "the agents table lost a column, or gained one that is not a field").toEqual([
    "ID",
    "Name",
    "Model",
    "Revisions",
    "Latest published",
    "Status",
  ]);

  const first = panel.locator("tbody tr").first();
  const href = await first.getByTestId("agent-link").getAttribute("href");
  expect(href, "the id cell is not a link to the agent's own page").toMatch(/^\/agents\/.+/);
  // BOTH cells go to the same place — an operator aiming at the one part of the row they cannot read is a
  // row that is clickable in the wrong place.
  expect(await first.getByTestId("agent-name-link").getAttribute("href")).toBe(href);

  const id = decodeURIComponent(String(href).replace("/agents/", ""));
  const lineage = await page.evaluate(async (agent) => {
    const res = await fetch(`/api/palai/v1/agents/${encodeURIComponent(agent)}/revisions`, { headers: { Accept: "application/json" } });
    const body = (await res.json()) as { data?: { status?: string; revision_number?: number }[] };
    const rows = body.data ?? [];
    return {
      count: rows.length,
      state: rows.some((r) => r.status === "published") ? "published" : rows.length > 0 ? "draft only" : "no revisions",
    };
  }, id);

  // The cells are not read until the per-row lineage fetch has landed; the count is the last of the four to
  // settle, so waiting on it is waiting on all of them.
  await expect(first.getByTestId("agent-revision-count")).toHaveText(String(lineage.count), { timeout: 15_000 });
  await expect(first.getByTestId("agent-status")).toContainText(lineage.state);

  // AND THE ROW OPENS. Nothing in this console's lists went anywhere before /sessions; this is the second.
  await first.getByTestId("agent-name-link").click();
  await expect(page).toHaveURL(new RegExp(`/agents/${id.replace(/[.*+?^${}()|[\\]\\\\]/g, "\\\\$&")}$`));
  await expect(page.getByTestId("agent-chips")).toBeVisible({ timeout: 15_000 });
});

test("the create form is a dialog: it is not on the page until asked for, and it closes on Escape", async ({ page }) => {
  // A CREATE FORM THAT IS ALWAYS OPEN IS A FORM THE OPERATOR SCROLLS PAST ON EVERY VISIT. This screen used
  // to carry four of them; the assertion is that the list is the page and the form is a mode.
  await page.goto("/agents");
  await expect(page.getByTestId("panel-agent-profiles")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("agent-name-input")).toHaveCount(0);
  // No form element at all until the dialog exists — the same enumeration tests/auth.spec.ts makes about
  // where a form may live, asked about whether one is on screen.
  expect(await page.locator("main form").count(), "a form is rendered on the agents list before anything asked for one").toBe(0);

  await page.getByTestId("agent-create-open").click();
  const dialog = page.getByTestId("agent-create-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toHaveAttribute("aria-modal", "true");
  // THE ACCESSIBLE NAME IS ASSERTED rather than assumed: a dangling aria-labelledby produces an EMPTY name
  // and no error, which is why components/FormDialog.tsx uses aria-label — this is what makes that a
  // measurement.
  // THE ACCESSIBLE NAME, NOT THE ATTRIBUTE THAT HAPPENS TO CARRY IT. This asserted aria-label while the
  // dialog was hand-rolled; components/ui/Dialog names itself with Base UI's own <Title> and an
  // aria-labelledby it owns, so the attribute changed and the NAME did not. Asserting the name covers both
  // mechanisms and still fails on the case the attribute check existed for — a dangling or empty reference
  // yields an EMPTY accessible name, which axe does not report.
  await expect(dialog).toHaveAccessibleName("Create an agent");
  // Focus moved IN, onto the field the operator opened the dialog to type into.
  await expect(page.getByTestId("agent-name-input")).toBeFocused();

  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  // And BACK, onto the control that opened it. A keyboard operator who cancels and lands at the top of the
  // document has lost the list they were working in.
  await expect(page.getByTestId("agent-create-open")).toBeFocused();
});

// --- THE RUN --------------------------------------------------------------------------------------------

test("a run started with a PUBLISHED revision carries that revision's model in its terminal projection", async ({ page }) => {
  // THE CROWN, and it is fake-profile-only for a MEASURED reason rather than a convenient one: the compose
  // deployment's model adapter reports a hardcoded model name whatever it is asked for, so no pinned model
  // can be observed there at all. DIV-UI-007 records it and the conformance sweep re-derives it from a real
  // revision-pinned run on every sweep — so if the compose adapter ever starts echoing the requested model,
  // the sweep goes red before this skip can go stale.
  skipOnReal("DIV-UI-007");

  const { agentId, revisionId } = await createAgentWithRevision(page, {
    agentName: `t6-run-agent-${stamp()}`,
    publish: true,
  });

  // The model the API says this revision carries — read from the revision list, not from what was typed, so
  // the comparison below is between two things the SERVER said.
  const revisionModel = await page.evaluate(
    async ([agent, revision]) => {
      const res = await fetch(`/api/palai/v1/agents/${encodeURIComponent(agent)}/revisions`, { headers: { Accept: "application/json" } });
      const body = (await res.json()) as { data?: { id: string; model?: string }[] };
      return body.data?.find((r) => r.id === revision)?.model ?? "";
    },
    [agentId, revisionId],
  );
  expect(revisionModel, "the API reported no model for the revision, so the comparison would be vacuous").toBe(PINNED_MODEL);

  await runWithRevision(page, agentId, revisionId);
  // The fake profile parks on an approval before terminal; that is the fixture's scripted stream and not
  // this test's subject.
  await expect(page.getByTestId("approval-panel")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("approve-button").click();

  await expect(page.getByTestId("terminal-status")).toContainText(/completed/i, { timeout: 30_000 });
  // THE ASSERTION. An unpinned run reports `fake` on both profiles, so this value can only have come from
  // the revision the run was pinned to.
  await expect(page.getByTestId("model")).toHaveText(`Model: ${revisionModel}`);
});

test("a run started with an UNPUBLISHED revision is refused honestly and falls back to no default", async ({ page }) => {
  const { agentId, revisionId } = await createAgentWithRevision(page, {
    agentName: `t6-draft-agent-${stamp()}`,
    publish: false,
  });

  // The draft is OFFERED and LABELLED rather than hidden: hiding it would make the refusal below
  // unreachable from the console, and an operator who cannot see a draft cannot tell why their agent is not
  // running.
  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });
  await chooseOption(page, "run-agent-select", agentId);
  // THE ROW IS READ WHERE IT LIVES: inside the open popup. The words are the point of this leg — a draft
  // that is offered without being LABELLED a draft is a control that leads straight to a refusal — so the
  // listbox is opened, the row is read, and the same row is then clicked. One interaction, not two.
  await page.getByTestId("run-revision-select").click();
  const option = page.getByRole("listbox").locator(`[role="option"][data-value="${revisionId}"]`);
  await expect(option).toHaveText(/draft/);
  await expect(option).toHaveText(/cannot be run/);
  await option.click();
  await expect(page.getByTestId("run-revision-select")).toHaveAttribute("data-value", revisionId);
  await page.getByTestId("run-button").click();

  // THE SERVER'S OWN SENTENCE, in a role="alert" region — api/responses.go answers 409
  // `revision_not_published` with "the pinned revision is a draft; publish it before running it", decided
  // BEFORE the idempotency reserve so nothing was created.
  const refusal = page.getByTestId("run-error");
  await expect(refusal).toBeVisible({ timeout: 30_000 });
  await expect(refusal).toContainText("draft");
  await expect(refusal).toContainText("publish it before running it");
  // AND THE CONSOLE'S OWN SENTENCE ABOUT WHAT DID NOT HAPPEN. "Silently fell back to a default" is the
  // failure mode this assertion exists for: a run that started with some other configuration would be
  // worse than one that did not start.
  await expect(refusal).toContainText("did not start");
  await expect(refusal).toContainText("no default");

  // NOTHING RAN. No terminal panel, no model, no session — a refused admission leaves no run behind.
  await expect(page.getByTestId("terminal-status")).toHaveCount(0);
  await expect(page.getByTestId("model")).toHaveCount(0);
});

test("a run with NO revision behaves exactly as it did before — the pin is optional", async ({ page }) => {
  // THE REGRESSION GUARD FOR THE OPTIONAL-NESS, and it is why tests/journey.spec.ts needed no edit at all:
  // the relay omits `agent_revision_id` from the upstream body when no revision is chosen, so today's
  // /runs behaviour is bit-unchanged. Asserted on the WIRE rather than on the outcome — an outcome
  // assertion would pass even if the console started sending an empty string, which is a value the API
  // would have to interpret.
  const bodies: string[] = [];
  page.on("request", (req) => {
    if (req.url().includes("/api/palai/stream") && req.method() === "POST") bodies.push(req.postData() ?? "");
  });

  await page.goto("/runs");
  await expect(page.getByTestId("run-button")).toBeVisible({ timeout: 15_000 });
  // Deliberately no picker interaction: the DEFAULT state of this page is the thing under test.
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("status")).toContainText(/stream|complete/i, { timeout: 15_000 });

  expect(bodies.length, "the run made no stream request").toBeGreaterThan(0);
  for (const raw of bodies) {
    const body = JSON.parse(raw) as Record<string, unknown>;
    expect(Object.keys(body).sort(), "an unpinned run sent something other than a bare prompt to the relay").toEqual(["prompt"]);
  }
});
