import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, signIn, skipOnReal } from "./profile";

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
async function createAgentWithRevision(
  page: Page,
  opts: { agentName: string; environmentName?: string; model?: string; publish: boolean },
): Promise<{ agentId: string; revisionId: string }> {
  await page.goto("/agents");
  await expect(page.getByTestId("panel-agent-profiles")).toBeVisible({ timeout: 15_000 });

  await page.getByTestId("agent-name-input").fill(opts.agentName);
  await page.getByTestId("agent-create-button").click();
  await expect(page.getByTestId("agent-create-status")).toContainText(opts.agentName, { timeout: 15_000 });

  // The agent select is the page's own list — the create selected the new lineage, which is what makes the
  // revision form addressable without a second lookup. Awaited rather than read once: the list REFETCHES
  // after the create, and the option cannot exist before that lands. If it never lands (an agent past the
  // first page, which is what appending to the fixture used to produce), this fails and says so.
  await expect(
    page.getByTestId("agent-select"),
    "the create did not select the agent it just made, so the revision form has no parent",
  ).not.toHaveValue("", { timeout: 15_000 });
  const agentId = await page.getByTestId("agent-select").inputValue();

  await page.getByTestId("revision-model-input").fill(opts.model ?? PINNED_MODEL);
  if (opts.environmentName !== undefined) {
    // FOUND BY ITS LABEL, selected by the value that label carries. The label is what an operator reads, so a
    // picker that offered the right id under the wrong name would fail here; going straight to the id would
    // not have noticed.
    const picker = page.getByTestId("revision-environment-select");
    const option = picker.locator("option").filter({ hasText: opts.environmentName });
    await expect(option, "the environment the operator just created is not on the revision form's picker").toHaveCount(1);
    await picker.selectOption(String(await option.getAttribute("value")));
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
  await page.getByTestId("run-agent-select").selectOption(agentId);
  await page.getByTestId("run-revision-select").selectOption(revisionId);
  await page.getByTestId("run-button").click();
}

// --- THE REPOSITORY BINDING ------------------------------------------------------------------------------

test("a repository binding is registered from the console and reads back on the list", async ({ page }) => {
  await page.goto("/repositories");
  await expect(page.getByTestId("panel-repository-bindings")).toBeVisible({ timeout: 15_000 });

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

  // AND THE CEILING IS ON THE SCREEN: registering a binding did not clone anything.
  await expect(page.getByTestId("binding-reachability-note")).toContainText("does not prove");
  await expect(page.getByTestId("binding-correction-note")).toContainText("no way to change or remove");

  await expectAxeClean(page);
});

test("the connection ref is a HANDLE chosen from the secret-ref list, never a typed value", async ({ page }) => {
  // STATE-INDEPENDENT, which is why the assertion is about the FORM's inputs rather than about the picker's
  // options: a fresh compose stack has an EMPTY secret_refs collection (nothing seeds one) while the fixture
  // and an accumulated real stack both have rows, so a spec that required options would be a skip in
  // disguise. What holds in every state is that no text field on this form takes a credential or a ref.
  await page.goto("/repositories");
  await expect(page.getByTestId("panel-repository-bindings")).toBeVisible({ timeout: 15_000 });

  const form = page.getByTestId("repository-binding-form");
  const textFields = await form.locator('input[type="text"], input:not([type]), textarea').evaluateAll((els) =>
    els.map((el) => el.getAttribute("data-testid") ?? el.getAttribute("name") ?? "<unnamed>"),
  );
  expect(
    textFields.sort(),
    "the binding form's free-text fields are enumerated: a connection_ref or a credential box among them would " +
      "be a value crossing a screen that is only allowed to carry a handle",
  ).toEqual(
    [
      "binding-classification-input",
      "binding-clone-url-input",
      "binding-default-branch-input",
      "binding-identity-input",
      "binding-operations-input",
      "binding-policy-input",
      "binding-provider-input",
      "binding-region-input",
    ].sort(),
  );
  // No password field either: this screen takes no secret at all, unlike T4's.
  await expect(form.locator('input[type="password"]')).toHaveCount(0);

  // WHICHEVER STATE THE COLLECTION IS IN, exactly one of these two is true and the other is absent.
  const picker = page.getByTestId("binding-connection-select");
  const empty = page.getByTestId("binding-connection-select-empty");
  if ((await picker.count()) === 1) {
    // The options are the API's OWN list of NAMES. A secret ref's read projection carries {name, version,
    // updated_at} and no value (identity/secrets.go secretRefView), so a name is all there is to offer.
    const listed = await page.evaluate(async () => {
      const res = await fetch("/api/palai/v1/secret-refs", { headers: { Accept: "application/json" } });
      return ((await res.json()) as { data?: { name: string }[] }).data?.map((r) => r.name) ?? [];
    });
    const offered = await picker.locator("option").evaluateAll((els) => els.map((el) => (el as HTMLOptionElement).value).filter((v) => v !== ""));
    expect(offered.sort()).toEqual([...listed].sort());
  } else {
    await expect(empty).toContainText("no secret refs");
  }
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

  // The publish control is GONE from a published row: a second publish is not a thing an operator can ask
  // for on a lineage that cannot be un-published.
  await expect(page.getByTestId(`publish-${revisionId}`)).toHaveCount(0);

  await expectAxeClean(page);
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

  // AN AGENT IS CREATED FIRST, because the revision form belongs to ONE lineage and does not exist until one
  // is chosen — a page with no agent selected renders a note in its place, which is a different absence from
  // the one under test. Creating rather than picking an existing row is what makes this deterministic on both
  // profiles: a bootstrap compose stack holds zero agents.
  await page.getByTestId("agent-name-input").fill(`t6-empty-env-${stamp()}`);
  await page.getByTestId("agent-create-button").click();
  await expect(page.getByTestId("agent-select")).not.toHaveValue("", { timeout: 15_000 });
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
  await expect(page.getByTestId("revision-t7-note")).toContainText("Both external fields read back");
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
  await page.getByTestId("run-agent-select").selectOption(agentId);
  const option = page.getByTestId("run-revision-select").locator(`option[value="${revisionId}"]`);
  await expect(option).toHaveText(/draft/);
  await expect(option).toHaveText(/cannot be run/);

  await page.getByTestId("run-revision-select").selectOption(revisionId);
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
