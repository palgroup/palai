import { expect, test, type Page } from "@playwright/test";

import { announceProfile, signIn } from "./profile";

// CONFIGURING A POOL AND CONFIGURING ONE MACHINE — the write half of /fleet, and the column that refuses to
// let "saved" be read as "running".
//
// EVERY LEG DRIVES THE SURFACE, and that is the rule this file inherits from tests/desired-config.spec.ts
// rather than a habit. Its header records why: nine auth legs once proved identity-less access, showed the
// attack first, counted every relay export with an AST, and every one of them went to the endpoint with
// `fetch` — none drove the form, and the form had no `method`. So here the row's `⋯` is opened, the item is
// clicked, the field is typed into, the submit is pressed, and what is asserted is the request that leaves
// the browser as a result. A `page.request.put()` would prove the route works and say nothing about whether
// an operator can reach it — which on this surface is the whole question, because the openers live inside a
// portalled row menu that does not exist in the document until somebody clicks.
//
// AND THE WIRE IS ASSERTED, NOT THE OUTCOME. There are THREE planes behind one write route
// (PUT /v1/deployment/desired with `plane` + `scope_id`), so the failure this surface is most exposed to is
// a form that saves the right settings into the wrong scope: a machine edit landing on its pool reaches
// every other machine in it, and a pool edit landing on the control plane is refused by a validator whose
// sentence names a setting rather than the mistake. Neither is visible in a screenshot and both are obvious
// in the request body.
//
// THE PUT IS NEVER FORWARDED. tests/desired-config.spec.ts states the reason and it applies with more force
// here: a real profile's stack would otherwise take a desired document out of a test run and hand it to the
// next machine that asks for its configuration.
test.beforeAll(() => announceProfile("fleet-config.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/**
 * A GET /v1/deployment body in api/deployment.go's exact shape, holding the three rows that make the field
 * selector a real narrowing.
 *
 * THE SECOND WRITABLE RUNNER ROW IS `PALAI_RUNNER_POSTURE`, AND IT IS NOT AN INVENTION. The live catalogue
 * carries three runner-plane entries and marks exactly one writable:
 *
 *	curl -s .../v1/deployment | jq -r '.settings[] | select(.plane=="runner_pool") | .name'
 *	  PALAI_RUNNER_CONCURRENCY   (writable)
 *	  PALAI_RUNNER_POSTURE       (not writable — no DesiredValue grammar)
 *	  PALAI_RUNNER_POOL          (not writable — no DesiredValue grammar)
 *
 * (api/deployment.go:126-129.) Declaring a grammar for POSTURE here models the ONE change that makes it
 * writable, which is exactly the change this console must survive without being edited — and with a single
 * writable row nothing in this file could tell "renders what the server declares" apart from "renders a
 * hardcoded PALAI_RUNNER_CONCURRENCY", which is the property the whole selector exists for.
 */
function deploymentBody() {
  const row = (name: string, plane: string, writable: boolean, grammar: string) => ({
    name,
    group: "execution",
    value: "",
    set: false,
    default: "1",
    kind: "value",
    effect: `what ${name} does`,
    mutability: "bring_up",
    change_with: "save it here; the machine takes it on its next settings poll",
    reader_file: plane === "control_plane" ? "apps/control-plane/api/deployment.go" : "cmd/runner/main.go",
    reader_func: "main",
    writable,
    not_writable_because: writable ? "" : "read by a RUNNER, and this deployment declares no value grammar for it",
    value_grammar: grammar,
    plane,
    observable: plane === "control_plane",
    desired: "",
    desired_set: false,
    drift: false,
  });
  return {
    object: "deployment",
    settings: [
      // WRITABLE, AND ON THE WRONG PLANE FOR THIS SCREEN. A control-plane setting in a runner document is
      // refused by the server (deployment_desired.go:201), so a field for it here would be a control whose
      // every value produces a 400 — which is what /deployment's own panel shipped until machine-config.
      row("PALAI_DISPATCH_WORKERS", "control_plane", true, "integer"),
      row("PALAI_RUNNER_CONCURRENCY", "runner_pool", true, "integer"),
      row("PALAI_RUNNER_POSTURE", "runner_pool", true, "token"),
      row("PALAI_RUNNER_POOL", "runner_pool", false, ""),
    ],
    warnings: [],
    desired: null,
  };
}

/** One desired document, as api/deployment_desired.go's DesiredDocument serialises it. */
function document(plane: string, scopeId: string, revision: number, settings: Record<string, string>) {
  return {
    revision,
    plane,
    scope_id: scopeId,
    settings,
    written_at: "2026-08-03T08:08:00Z",
    written_by: "key_console_fixture",
  };
}

type Documents = Record<string, ReturnType<typeof document>>;

/**
 * serveFleetConfig intercepts the four reads this surface makes AT THE BROWSER BOUNDARY and records every
 * write, exactly as tests/desired-config.spec.ts does for /deployment.
 *
 * REWRITING THE RESPONSE RATHER THAN SCRIPTING A FIXTURE is what makes every leg here run on BOTH profiles:
 * these are the bytes a control plane in this state sends, the routes are the same on both sides, and what
 * is asserted is this console's rendering — true on both or on neither.
 *
 * THE DOCUMENTS ARE KEYED `<plane>:<scope_id>` because that is how the store keys them (migration
 * 000052/000060), and a missing key answers `desired: null` rather than 404 — which is the real route's own
 * decision: "a 404 would mean 'there is no such pool', which this route cannot know and must not imply"
 * (api/runners.go's desiredForScope). A form must render EMPTY FIELDS for that answer and never an error.
 */
async function serveFleetConfig(
  page: Page,
  options: { documents?: Documents; machines?: Record<string, unknown>[]; onPut?: { status: number; json: unknown } } = {},
): Promise<{ writes: { method: string; body: unknown }[] }> {
  const documents = options.documents ?? {};
  const onPut = options.onPut ?? { status: 200, json: { object: "deployment_desired", revision: 3 } };
  const writes: { method: string; body: unknown }[] = [];

  const envelope = (plane: string, scopeId: string) => ({
    object: "deployment_desired",
    plane,
    scope_id: scopeId,
    desired: documents[`${plane}:${scopeId}`] ?? null,
  });

  await page.route("**/api/palai/v1/deployment", (route) =>
    route.request().method() === "GET" ? route.fulfill({ status: 200, json: deploymentBody() }) : route.fallback(),
  );
  await page.route("**/api/palai/v1/deployment/desired", (route) => {
    writes.push({ method: route.request().method(), body: JSON.parse(route.request().postData() ?? "null") });
    return route.fulfill({ status: onPut.status, json: onPut.json });
  });
  await page.route("**/api/palai/v1/runner-pools/*/desired", (route) => {
    const id = decodeURIComponent(new URL(route.request().url()).pathname.split("/").slice(-2)[0]);
    return route.fulfill({ status: 200, json: envelope("runner_pool", id) });
  });
  await page.route("**/api/palai/v1/runners/*/desired", (route) => {
    const id = decodeURIComponent(new URL(route.request().url()).pathname.split("/").slice(-2)[0]);
    return route.fulfill({ status: 200, json: envelope("runner_machine", id) });
  });
  if (options.machines !== undefined) {
    // THE PATHNAME IS COMPARED EXACTLY. A glob loose enough to catch `/runners` would also catch
    // `/runners/{id}/desired`, and the column's own reads would then be answered with a machine list.
    await page.route("**/api/palai/v1/runners**", (route) => {
      const url = new URL(route.request().url());
      if (route.request().method() !== "GET" || url.pathname !== "/api/palai/v1/runners") return route.fallback();
      return route.fulfill({ status: 200, json: { object: "list", data: options.machines, has_more: false } });
    });
  }
  return { writes };
}

/** open loads /fleet and waits for the panel that holds the row this file is about to drive. */
async function open(page: Page, panel: "panel-runner-pools" | "panel-runners") {
  await page.goto("/fleet");
  await expect(page.getByTestId(panel)).toBeVisible({ timeout: 15_000 });
}

/**
 * openRowConfig opens the FIRST row of `panel` whose `⋯` offers `<prefix><id>` and returns that id.
 *
 * THE ID IS READ OFF THE ROW RATHER THAN PINNED, which is what lets every leg below run on the real profile:
 * a compose stack seeds `pool_default` and enrols exactly one machine whose id is a fresh hash per stack.
 * The same claim tests/fleet.spec.ts's firstMachineWith makes, and the same reason.
 *
 * The menu is AWAITED before it is probed: components/ui/Menu.tsx portals its popup and positions it a frame
 * later, so an immediate `count()` — which does not auto-wait — reads zero on every row.
 */
async function openRowConfig(page: Page, panel: string, menuPrefix: string, itemPrefix: string): Promise<string> {
  const toggles = page.getByTestId(panel).locator(`[data-testid^="${menuPrefix}"]`);
  await expect(toggles.first(), `${panel} rendered no rows at all`).toBeVisible({ timeout: 15_000 });
  const count = await toggles.count();
  for (let i = 0; i < count; i++) {
    const toggle = toggles.nth(i);
    const id = (await toggle.getAttribute("data-testid"))?.replace(menuPrefix, "") ?? "";
    await toggle.click();
    await expect(page.getByRole("menu"), `the ${id} row menu did not open`).toBeVisible();
    const item = page.getByTestId(`${itemPrefix}${id}`);
    if ((await item.count()) > 0) {
      await item.click();
      return id;
    }
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu")).toHaveCount(0);
  }
  throw new Error(`no row in ${panel} offers ${itemPrefix} — ${String(count)} menu(s) were opened`);
}

// LEG 1 — THE POOL FORM, AS A HUMAN REACHES IT, AND THE SCOPE IT WRITES.
test("configuring a pool sends a runner_pool document naming that pool, and omits the field left empty", async ({ page }) => {
  const recorded = await serveFleetConfig(page, {
    documents: { "runner_pool:__none__": document("runner_pool", "__none__", 1, {}) },
  });

  await open(page, "panel-runner-pools");
  const poolId = await openRowConfig(page, "panel-runner-pools", "pool-menu-", "pool-config-");
  await expect(page.getByTestId("pool-config-dialog")).toBeVisible();

  // NOTHING SAVED IS A SENTENCE AND NOT AN ERROR. `desired: null` is the answer for every scope nobody has
  // configured, which is every scope until somebody does — a form that rendered a failure for it would
  // report a fault for the normal case.
  await expect(page.getByTestId("pool-config-revision")).toContainText("Nothing has been saved");
  await expect(page.getByTestId("pool-config-form").locator('[role="alert"]')).toHaveCount(0);

  // ONE FIELD PER WRITABLE RUNNER-PLANE SETTING, AND NOTHING ELSE. The control-plane row is writable and
  // still gets no field (its value in a runner document is refused by the server); the runner row that is
  // not writable gets none either. This is the check that the field set is READ from the server's two flags
  // rather than carried here — a console with its own list keeps offering a control after the server
  // withdraws it.
  await expect(page.getByTestId("pool-config-PALAI_RUNNER_CONCURRENCY")).toBeVisible();
  await expect(page.getByTestId("pool-config-PALAI_RUNNER_POSTURE")).toBeVisible();
  await expect(page.getByTestId("pool-config-PALAI_DISPATCH_WORKERS")).toHaveCount(0);
  await expect(page.getByTestId("pool-config-PALAI_RUNNER_POOL")).toHaveCount(0);
  // AND THE LABEL IS PROGRAMMATICALLY BOUND (WCAG 2.2 §3.3.2). `field-<name>` is the control's id and
  // components/ResourceForm.tsx records it as a contract this suite reads; a form whose fields are
  // addressable only by testid is one whose labels nothing checks.
  await expect(page.locator('label[for="field-PALAI_RUNNER_CONCURRENCY"]')).toHaveCount(1);
  await expect(page.locator('label[for="field-PALAI_DISPATCH_WORKERS"]')).toHaveCount(0);

  await page.getByTestId("pool-config-PALAI_RUNNER_CONCURRENCY").fill("4");
  await page.getByTestId("pool-config-save").click();

  await expect.poll(() => recorded.writes.length, { timeout: 10_000 }).toBe(1);
  // THE METHOD IS PUT, because the server REPLACES the document — which is what makes "stop deciding this
  // setting" expressible as an absent key. A PATCH would advertise a merge the server does not perform.
  expect(recorded.writes[0].method).toBe("PUT");
  // THE BODY, EXACTLY. `plane` and `scope_id` are what route this document to a pool rather than to the
  // control-plane singleton, and the empty POSTURE field is OMITTED rather than sent as "": the document is
  // presence-keyed, and the server refuses "" outright — so a console that sent it would put a refusal in
  // front of an operator who left a field alone.
  expect(recorded.writes[0].body).toEqual({
    plane: "runner_pool",
    scope_id: poolId,
    settings: { PALAI_RUNNER_CONCURRENCY: "4" },
  });

  // AND THE SCREEN DOES NOT CLAIM A MACHINE CHANGED. This is the line that separates this surface from the
  // "declared, and nothing happens" control it was built to replace.
  await expect(page.getByTestId("runner-config-status")).toContainText("No machine has it yet");
  await expect(page.getByTestId("runner-config-status")).toContainText(poolId);
});

// LEG 2 — THE MACHINE FORM, THE SAME WAY, INTO THE OTHER SCOPE.
//
// The two forms offer the SAME fields, which is the model rather than a bug: `runner_pool` and
// `runner_machine` differ in SCOPE and not in READER — both documents are read by cmd/runner — so
// deployment_desired.go compares `readerOf(plane)` and a machine document legitimately carries a setting the
// catalogue files under `runner_pool`. What must differ is the two words on the wire, and that is what this
// asserts: same field, same value, different scope.
test("configuring one machine sends a runner_machine document naming that machine and no other scope", async ({ page }) => {
  const recorded = await serveFleetConfig(page);

  await open(page, "panel-runners");
  const runnerId = await openRowConfig(page, "panel-runners", "runner-menu-", "machine-config-");
  await expect(page.getByTestId("machine-config-dialog")).toBeVisible();
  // THE ACCESSIBLE NAME SAYS WHICH SCOPE. Two dialogs write through one route, and the name is what tells a
  // screen-reader operator whether this reaches one machine or every machine in its pool.
  await expect(page.getByTestId("machine-config-dialog")).toHaveAccessibleName("Configure this machine");

  await page.getByTestId("machine-config-PALAI_RUNNER_CONCURRENCY").fill("2");
  await page.getByTestId("machine-config-save").click();

  await expect.poll(() => recorded.writes.length, { timeout: 10_000 }).toBe(1);
  expect(recorded.writes[0].method).toBe("PUT");
  expect(recorded.writes[0].body).toEqual({
    plane: "runner_machine",
    scope_id: runnerId,
    settings: { PALAI_RUNNER_CONCURRENCY: "2" },
  });
});

// LEG 3 — A SAVED DOCUMENT COMES BACK INTO THE FIELDS, and it comes from the SCOPE'S OWN READ.
//
// A form that opened empty over an existing document would make every save a silent clear: the write
// replaces, so an untouched field that rendered blank would delete the value it failed to show.
test("the fields open holding what is already saved for that scope", async ({ page }) => {
  await open(page, "panel-runner-pools");
  const first = page.getByTestId("panel-runner-pools").locator('[data-testid^="pool-menu-"]').first();
  await expect(first).toBeVisible({ timeout: 15_000 });
  const poolId = (await first.getAttribute("data-testid"))?.replace("pool-menu-", "") ?? "";
  expect(poolId, "the pool panel rendered no row, so this leg would be about nothing").not.toBe("");

  await serveFleetConfig(page, {
    documents: { [`runner_pool:${poolId}`]: document("runner_pool", poolId, 6, { PALAI_RUNNER_CONCURRENCY: "4" }) },
  });
  await open(page, "panel-runner-pools");
  await openRowConfig(page, "panel-runner-pools", "pool-menu-", "pool-config-");

  await expect(page.getByTestId("pool-config-revision")).toContainText("Revision 6");
  await expect(page.getByTestId("pool-config-PALAI_RUNNER_CONCURRENCY")).toHaveValue("4");
  // The setting the document does NOT decide opens empty, which is what "no opinion" looks like in a form.
  await expect(page.getByTestId("pool-config-PALAI_RUNNER_POSTURE")).toHaveValue("");
});

// LEG 4 — THE REFUSAL IS THE SERVER'S OWN SENTENCE, IN THE ALERT REGION, WITH THE DIALOG STILL OPEN.
//
// The control plane refuses a value its own reader would silently coerce, and its refusal names the setting
// and says what would happen. A console that replaced it with "invalid request" would send an operator to
// read Go source. It is announced in the role="alert" region without moving focus (WCAG 2.2 §3.3.1: the
// error is described IN TEXT).
test("a refused value shows the control plane's own reason and the dialog stays open", async ({ page }) => {
  await serveFleetConfig(page, {
    onPut: {
      status: 400,
      json: {
        code: "invalid_request",
        detail:
          "desired configuration refused: PALAI_RUNNER_CONCURRENCY: not an integer this binary's own reader would parse " +
          '(strconv.Atoi: parsing "4x": invalid syntax). It reads an unparseable value as its default and says nothing, ' +
          "so the panel would show a number the process is not running",
      },
    },
  });

  await open(page, "panel-runner-pools");
  await openRowConfig(page, "panel-runner-pools", "pool-menu-", "pool-config-");
  await page.getByTestId("pool-config-PALAI_RUNNER_CONCURRENCY").fill("4x");
  await page.getByTestId("pool-config-save").click();

  const alert = page.getByTestId("pool-config-form").locator('[role="alert"]');
  await expect(alert).toContainText("PALAI_RUNNER_CONCURRENCY");
  await expect(alert).toContainText("would show a number the process is not running");
  // THE DIALOG STAYS OPEN. Closing it would throw away everything the operator typed and leave a screen with
  // no error on it — the refusal announced to nobody.
  await expect(page.getByTestId("pool-config-dialog")).toBeVisible();
  await expect(page.getByTestId("pool-config-PALAI_RUNNER_CONCURRENCY")).toHaveValue("4x");
});

// --- THE COLUMN, AND THE FOUR THINGS IT HAS TO KEEP APART -------------------------------------------------
//
// This is the half that matters most, because the failure it prevents is the one this whole surface exists
// to remove: a panel that shows a value as configured when no machine has taken it. The four states are
// driven from the bytes api/runners.go's runnerView actually sends — all three config keys together or none
// at all — laid against the revision the plane would answer each machine now, which is
// max(pool document, machine document) exactly as DesiredSettingsForMachine computes it.
const MACHINES = [
  { id: "cfg_never", object: "runner", pool_id: "pool_cfg", label: "never-reported", state: "active", os: "darwin", arch: "arm64", posture: "unsandboxed-host", created_at: "2026-08-01T00:00:00Z" },
  {
    id: "cfg_applied", object: "runner", pool_id: "pool_cfg", label: "all-applied", state: "active", os: "darwin", arch: "arm64", posture: "unsandboxed-host", created_at: "2026-08-01T00:00:00Z",
    config_revision: 7, config_applied: { PALAI_RUNNER_CONCURRENCY: "applied" }, config_reported_at: "2026-08-03T08:08:31Z",
  },
  {
    id: "cfg_notread", object: "runner", pool_id: "pool_cfg", label: "reads-nothing", state: "active", os: "darwin", arch: "arm64", posture: "unsandboxed-host", created_at: "2026-08-01T00:00:00Z",
    config_revision: 7, config_applied: { PALAI_RUNNER_POSTURE: "not_read" }, config_reported_at: "2026-08-03T08:08:31Z",
  },
  {
    id: "cfg_behind", object: "runner", pool_id: "pool_cfg", label: "behind", state: "active", os: "darwin", arch: "arm64", posture: "unsandboxed-host", created_at: "2026-08-01T00:00:00Z",
    config_revision: 5, config_applied: { PALAI_RUNNER_CONCURRENCY: "applied" }, config_reported_at: "2026-08-03T08:00:00Z",
  },
];

const MACHINE_DOCUMENTS: Documents = {
  "runner_machine:cfg_applied": document("runner_machine", "cfg_applied", 7, { PALAI_RUNNER_CONCURRENCY: "4" }),
  "runner_machine:cfg_notread": document("runner_machine", "cfg_notread", 7, { PALAI_RUNNER_POSTURE: "unsandboxed-host" }),
  "runner_machine:cfg_behind": document("runner_machine", "cfg_behind", 9, { PALAI_RUNNER_CONCURRENCY: "8" }),
};

test("a machine that has never reported says so, and is not rendered as running revision 0", async ({ page }) => {
  // THE ABSENCE IS THE MESSAGE. api/runners.go renders the three config keys only when the machine has
  // actually reported, and says why: "a screen that showed `config_revision: 0` would render that as a
  // number, and an operator reads a number as an answer. Absence they have to ask about." So the assertion
  // is not only on the words — it is that the cell carries NO DIGIT at all, which is what a rendered 0 or a
  // fabricated revision would break.
  await serveFleetConfig(page, { machines: MACHINES, documents: MACHINE_DOCUMENTS });
  await open(page, "panel-runners");

  const cell = page.getByTestId("runner-config-cfg_never");
  await expect(cell).toBeVisible({ timeout: 15_000 });
  await expect(cell).toHaveAttribute("data-config-state", "never-reported");
  await expect(cell).toContainText("never reported");
  await expect(cell, "a machine this control plane has never heard from was rendered with a number").not.toHaveText(/\d/);
});

test("a machine that applied what was saved says which revision and which settings", async ({ page }) => {
  await serveFleetConfig(page, { machines: MACHINES, documents: MACHINE_DOCUMENTS });
  await open(page, "panel-runners");

  const cell = page.getByTestId("runner-config-cfg_applied");
  await expect(cell).toBeVisible({ timeout: 15_000 });
  await expect(cell).toHaveAttribute("data-config-state", "applied");
  await expect(cell).toContainText("Revision 7 applied");
  await expect(cell).toContainText("PALAI_RUNNER_CONCURRENCY");
  // AND IT DOES NOT CLAIM A WAIT. The machine's revision equals the one its plane would answer it now, so
  // there is nothing outstanding — a column that said so anyway would make the transient state meaningless.
  await expect(cell).not.toContainText("not reached it yet");
});

test("a machine that reported not_read NAMES the setting, because that value will never do anything on it", async ({ page }) => {
  // THIS IS THE STATE THE VERDICT EXISTS FOR. packages/runner/settings.go: `not_read` is reported "rather
  // than silently dropped because the alternative is a panel showing a saved value against a machine that
  // will never act on it". A column that said "reported" and stopped would reproduce exactly that.
  await serveFleetConfig(page, { machines: MACHINES, documents: MACHINE_DOCUMENTS });
  await open(page, "panel-runners");

  const cell = page.getByTestId("runner-config-cfg_notread");
  await expect(cell).toBeVisible({ timeout: 15_000 });
  await expect(cell).toHaveAttribute("data-config-state", "not-applied");
  await expect(cell, "the setting the machine will never act on has to be NAMED").toContainText("PALAI_RUNNER_POSTURE");
  await expect(cell).toContainText("no reader for it");
  // "restart or not" is the half an operator otherwise supplies themselves, and wrongly: `not_read` is not
  // `pending_restart`, and settings.go records that the second verdict deliberately does not exist because
  // nothing in cmd/runner would ever write it.
  await expect(cell).toContainText("restart or not");
});

test("a machine behind the saved revision is shown as a wait, with both numbers on screen", async ({ page }) => {
  // THE FOURTH STATE, AND IT NEEDS THE DOCUMENT AS WELL AS THE REPORT: the machine says it acted on 5 and
  // its own document is at 9, so the edit has not reached it. It is a NORMAL transient — a machine asks for
  // its configuration about every 30 seconds (packages/runner/serve.go's defaultSettingsInterval) — and the
  // cell has to say that, or an operator reads a routine 30-second gap as a broken fleet.
  await serveFleetConfig(page, { machines: MACHINES, documents: MACHINE_DOCUMENTS });
  await open(page, "panel-runners");

  const cell = page.getByTestId("runner-config-cfg_behind");
  await expect(cell).toBeVisible({ timeout: 15_000 });
  await expect(cell).toHaveAttribute("data-config-state", "behind");
  await expect(cell, "the revision the machine is running is half of what makes this readable").toContainText("Revision 5");
  await expect(cell, "the revision it has not got yet is the other half").toContainText("Revision 9");
  await expect(cell).toContainText("not reached it yet");
  await expect(cell).toContainText("30 seconds");
});

// LEG — THE SENTENCE THE WHOLE SURFACE EXISTS FOR, ASSERTED RATHER THAN WRITTEN.
//
// Prose that nothing drives is prose that goes stale: the paragraph this replaces on /deployment claimed
// PALAI_RUNNER_CONCURRENCY "lives on the runner container itself", which stopped being the whole story the
// day a pool document could carry it. So the distinction between a saved value and a running one is a test.
test("the machine list says that a saved value is not a running one, and what closes the gap", async ({ page }) => {
  await serveFleetConfig(page, { machines: MACHINES, documents: MACHINE_DOCUMENTS });
  await open(page, "panel-runners");

  const note = page.getByTestId("runner-config-note");
  await expect(note).toBeVisible();
  await expect(note).toContainText("Saved is not the same as running");
  await expect(note, "what closes the gap is the machine asking, and the interval is the operator's whole answer to 'why has nothing happened'").toContainText("30 seconds");
  await expect(note, "the never-reported state is the one an operator will otherwise read as a zero").toContainText("never reported");
});
