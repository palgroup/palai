import { expect, test, type Page } from "@playwright/test";

import { announceProfile, signIn } from "./profile";

// THE DESIRED CONFIGURATION SCREEN — the write half of /deployment.
//
// EVERY LEG BELOW DRIVES THE FORM, AND THAT IS THE RULE THIS FILE IS WRITTEN AGAINST rather than a habit.
// CLAUDE.md, 2026-07-31: five defects in one day and all five the same shape — nine auth legs proved
// identity-less access, showed the attack first, counted every relay export with an AST, and every one of
// them went to the endpoint with `fetch`. None drove the form, and the form had no `method`. So here: the
// button is clicked, the field is typed into, the submit is pressed, and what is asserted is the request
// that leaves the browser as a result. A `page.request.put()` would prove the route works and say nothing
// about whether an operator can reach it.
//
// AND THE WIRE IS ASSERTED, not the outcome. The bug class this surface is most exposed to is a form that
// posts something subtly different from what it displays — an empty field sent as "" (which the server
// refuses, because "" is a third state that looks like "no opinion"), or a PATCH where the server replaces.
// Those are invisible in a screenshot and obvious in the request body.
test.beforeAll(() => announceProfile("desired-config.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

/** A GET /v1/deployment body in api/deployment.go's exact shape, with a desired document and a drift. */
function deploymentBody(options: { desired?: Record<string, string>; drifted?: string[]; revision?: number }) {
  const desiredValues = options.desired ?? {};
  const drifted = options.drifted ?? [];
  const row = (
    name: string,
    value: string,
    writable: boolean,
    grammar: string,
    refusal: string,
  ) => ({
    name,
    group: "execution",
    value,
    set: value !== "",
    default: "1",
    kind: name.endsWith("_FILE") ? "path" : "value",
    effect: `what ${name} does`,
    mutability: "bring_up",
    change_with: "save it here and then run `palai up` on the machine",
    reader_file: "apps/control-plane/api/deployment.go",
    reader_func: "DispatchWorkers",
    writable,
    not_writable_because: refusal,
    value_grammar: grammar,
    desired: desiredValues[name] ?? "",
    desired_set: name in desiredValues,
    drift: drifted.includes(name),
  });
  return {
    object: "deployment",
    settings: [
      row("PALAI_DISPATCH_WORKERS", "1", true, "integer", ""),
      row("PALAI_QUEUE_DEADLINE", "", true, "duration", ""),
      row(
        "PALAI_SECRET_MASTER_KEY_FILE",
        "/run/secrets/master_key",
        false,
        "",
        "a path, and the sharpest one: it names the file the ENTIRE secret store redeems through.",
      ),
    ],
    warnings: [],
    desired:
      Object.keys(desiredValues).length === 0 && options.revision === undefined
        ? null
        : {
            plane: "control_plane",
            scope_id: "",
            revision: options.revision ?? 1,
            written_at: "2026-08-01T00:00:00Z",
            written_by: "prin_local",
            pending: drifted.length > 0,
            drifted,
          },
  };
}

/**
 * serveDeployment intercepts GET /v1/deployment AT THE BROWSER BOUNDARY and records every PUT the page
 * makes. Rewriting the response rather than scripting a fixture is the rule tests/deployment.spec.ts
 * already follows: those are the bytes a control plane in this state sends, the route is the same on both
 * profiles, and what is asserted is this console's rendering — true on both profiles or on neither.
 *
 * The PUT is captured and REFUSED-OR-FULFILLED per the caller, never forwarded: a real profile's stack
 * would otherwise take a desired document from a test run and hand it to the next `palai up`.
 *
 * THE `before`/`after` PAIR SWITCHES ON THE WRITE, NOT ON A CALL COUNT, and that is a correction rather
 * than a preference. /deployment issues THREE GET /v1/deployment reads on one visit — this panel, the
 * warning banner (components/DeploymentNotice) and the settings table (components/Panel) each fetch it —
 * so a fixture that handed out bodies in call order gave different components different worlds, and which
 * one saw the "no document" body depended on which fetch landed first. Switching on the PUT models what
 * actually happens: the document does not exist until the operator saves it, and after that every reader
 * sees it.
 */
async function serveDeployment(
  page: Page,
  bodies: { before: ReturnType<typeof deploymentBody>; after?: ReturnType<typeof deploymentBody> },
  onPut: { status: number; json: unknown },
): Promise<{ puts: { method: string; body: unknown }[] }> {
  const puts: { method: string; body: unknown }[] = [];
  await page.route("**/api/palai/v1/deployment", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const saved = puts.length > 0 && onPut.status < 400;
    return route.fulfill({ status: 200, json: saved ? (bodies.after ?? bodies.before) : bodies.before });
  });
  // THE FLEET COUNTS the scope sentence names. Served here because the panel reads them, and a spec that
  // let them fall through to the fake upstream would be asserting on the fixture's fleet rather than on
  // this console's sentence.
  await page.route("**/api/palai/v1/runners", (route) =>
    route.fulfill({ status: 200, json: { object: "list", data: [{ id: "rnr_1" }, { id: "rnr_2" }, { id: "rnr_3" }] } }),
  );
  await page.route("**/api/palai/v1/runner-pools", (route) =>
    route.fulfill({ status: 200, json: { object: "list", data: [{ id: "pool_a" }, { id: "pool_b" }] } }),
  );
  await page.route("**/api/palai/v1/deployment/desired", (route) => {
    puts.push({ method: route.request().method(), body: JSON.parse(route.request().postData() ?? "null") });
    return route.fulfill({ status: onPut.status, json: onPut.json });
  });
  return { puts };
}

// LEG 1 — THE ROUND TRIP, AS A HUMAN MAKES IT. Open the dialog with the button, type into the field, press
// the submit, and assert the request the browser actually sent.
test("saving a desired value sends the whole document as a PUT and says a bring-up is still needed", async ({ page }) => {
  const recorded = await serveDeployment(
    page,
    {
      before: deploymentBody({}), // no document yet
      after: deploymentBody({ desired: { PALAI_DISPATCH_WORKERS: "4" }, drifted: ["PALAI_DISPATCH_WORKERS"], revision: 1 }),
    },
    { status: 200, json: { object: "deployment_desired", revision: 1, settings: { PALAI_DISPATCH_WORKERS: "4" } } },
  );

  await page.goto("/deployment");
  await expect(page.getByTestId("panel-desired-config")).toBeVisible({ timeout: 15_000 });

  // WITH NO DOCUMENT THE SCREEN SAYS WHAT IS IN CONTROL. A blank panel would read as "nothing is
  // configured" when the truth is that something ELSE is configuring it, and that is the sentence an
  // operator has to see before they believe this panel is the machine's configuration.
  await expect(page.getByTestId("desired-config-absent")).toContainText("compose file");
  await expect(page.getByTestId("desired-config-absent")).toContainText("not in control");

  await page.getByTestId("desired-config-edit").click();
  await expect(page.getByTestId("desired-config-dialog")).toBeVisible();

  // ONLY THE WRITABLE SETTINGS HAVE A FIELD, and the path has none. The field set is built from the rows
  // the API marks writable, so this is a check that the console reads that flag rather than carrying its
  // own list of names — the list that would keep offering a control after the server withdrew it.
  await expect(page.getByTestId("desired-PALAI_DISPATCH_WORKERS")).toBeVisible();
  await expect(page.getByTestId("desired-PALAI_SECRET_MASTER_KEY_FILE")).toHaveCount(0);
  // AND THE LABEL IS PROGRAMMATICALLY BOUND. `field-<name>` is the control's id and ResourceForm's header
  // records it as a contract this suite reads; the pairing is what WCAG 2.2 §3.3.2 asks for, and a form
  // whose fields are addressable only by testid is one whose labels nothing checks.
  await expect(page.locator('label[for="field-PALAI_DISPATCH_WORKERS"]')).toHaveCount(1);
  await expect(page.locator('label[for="field-PALAI_SECRET_MASTER_KEY_FILE"]')).toHaveCount(0);

  await page.getByTestId("desired-PALAI_DISPATCH_WORKERS").fill("4");
  await page.getByTestId("desired-config-save").click();

  await expect.poll(() => recorded.puts.length, { timeout: 10_000 }).toBe(1);
  // THE METHOD. PUT, because the server REPLACES the document — which is what makes "stop deciding this
  // setting" expressible as an absent key. A PATCH would advertise a merge the server does not perform,
  // and the relay would answer 405 for it besides.
  expect(recorded.puts[0].method).toBe("PUT");
  // THE BODY, EXACTLY. The empty PALAI_QUEUE_DEADLINE field is OMITTED, never sent as "": the document is
  // presence-keyed, so a key that is present means the operator decided and a key that is absent means
  // "this deployment's own default". The server refuses "" outright, so a console that sent it would put a
  // refusal in front of an operator who left a field alone.
  expect(recorded.puts[0].body).toEqual({ settings: { PALAI_DISPATCH_WORKERS: "4" } });

  // AND THE SCREEN DOES NOT CLAIM THE MACHINE CHANGED. This is the line that separates this panel from the
  // "declared, and nothing happens" control the /deployment screen was built to expose.
  await expect(page.getByTestId("desired-config-pending")).toContainText("bring-up is pending");
  await expect(page.getByTestId("desired-config-pending")).toContainText("palai up");
  await expect(page.getByTestId("desired-config-pending")).toContainText("will NOT apply it");
});

// LEG 2 — THE REFUSAL IS THE SERVER'S OWN SENTENCE. Twenty-four of the thirty-five settings are refused and
// each refusal names the setting and says why; a console that replaced them with "invalid request" would
// send an operator to read Go source. It is announced in the role="alert" region without moving focus
// (WCAG 2.2 §3.3.1: the error is described IN TEXT).
test("a refused value shows the control plane's own reason, in the alert region", async ({ page }) => {
  await serveDeployment(page, { before: deploymentBody({}) }, {
    status: 400,
    json: {
      code: "invalid_request",
      detail:
        "desired configuration refused: PALAI_QUEUE_DEADLINE: not a Go duration (time.ParseDuration: time: unknown unit \"min\" in duration \"10min\"). `10min` is ten minutes to a reader and UNPARSEABLE to this binary, which then uses its default and says nothing — write `10m`",
    },
  });

  await page.goto("/deployment");
  await expect(page.getByTestId("panel-desired-config")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("desired-config-edit").click();
  await page.getByTestId("desired-PALAI_QUEUE_DEADLINE").fill("10min");
  await page.getByTestId("desired-config-save").click();

  const alert = page.getByTestId("desired-config-form").locator('[role="alert"]');
  await expect(alert).toContainText("PALAI_QUEUE_DEADLINE");
  await expect(alert).toContainText("write `10m`");
  // THE DIALOG STAYS OPEN on a refusal. Closing it would throw away everything the operator typed and give
  // them a screen with no error on it — the refusal announced to nobody.
  await expect(page.getByTestId("desired-config-dialog")).toBeVisible();
});

// LEG 3 — THE TABLE SAYS DESIRED AGAINST EFFECTIVE, IN THE SAME ROW, IN TEXT.
//
// The whole cost of this configuration being invisible was that an operator had to go somewhere else to
// find it. A desired value on a different screen from the effective one reproduces that with two screens
// instead of a `docker inspect`. And the drift is a WORD ("pending"), never a colour: colour alone says
// nothing to a screen reader and nothing to a colourblind operator (the rule Status.tsx already follows).
test("each setting's row shows what was saved for this machine against what the process holds", async ({ page }) => {
  await serveDeployment(
    page,
    { before: deploymentBody({ desired: { PALAI_DISPATCH_WORKERS: "4" }, drifted: ["PALAI_DISPATCH_WORKERS"], revision: 3 }) },
    { status: 200, json: {} },
  );
  await page.goto("/deployment");
  await expect(page.getByTestId("panel-deployment-settings")).toBeVisible({ timeout: 15_000 });

  const table = page.getByTestId("panel-deployment-settings").locator("table");

  const drifting = table.locator("tr", { hasText: "PALAI_DISPATCH_WORKERS" });
  await expect(drifting).toContainText("pending a bring-up");
  await expect(drifting).toContainText("4");

  // A WRITABLE SETTING WITH NOTHING SAVED SAYS SO AS AN OFFER, not as a blank. An empty cell reads as "this
  // cannot be set", which is the opposite of true for eleven of the thirty-five.
  const offered = table.locator("tr", { hasText: "PALAI_QUEUE_DEADLINE" });
  await expect(offered).toContainText("can be saved here");

  // AND A REFUSED SETTING CARRIES ITS REASON IN THE CELL. Twenty-four rows are in this state; without the
  // reason an operator who does not find the setting they came for concludes the console is unfinished
  // rather than that the refusal is deliberate.
  const refused = table.locator("tr", { hasText: "PALAI_SECRET_MASTER_KEY_FILE" });
  await expect(refused).toContainText("not settable here");
  await expect(refused).toContainText("the ENTIRE secret store redeems through");
});

// LEG 4 — THE SCOPE, IN WORDS, ON THE SCREEN.
//
// This is the leg the owner's question produced: "makine başına ayarlayabiliyor muyum bunu?" — can this be
// configured per machine? There are THREE scopes in this product (the control-plane process, the pool a
// machine belongs to, and the runner) and this panel is the first. A screen that said "this machine's
// configuration" would be wrong about which machines a change reaches and about how many there are.
//
// SO IT SAYS BOTH HALVES, and the second is the one a screen usually omits: what this is NOT, and where
// the thing you came for actually lives. An operator who finds no per-machine setting and is told nothing
// concludes the product cannot do it.
test("the panel says which scope it configures, and names the one it does not", async ({ page }) => {
  await serveDeployment(page, { before: deploymentBody({}) }, { status: 200, json: {} });
  await page.goto("/deployment");
  await expect(page.getByTestId("panel-desired-config")).toBeVisible({ timeout: 15_000 });

  const scope = page.getByTestId("desired-config-scope-sentence");
  await expect(scope).toContainText("control-plane process");
  await expect(scope).toContainText("Nothing here is per-machine");
  // THE COUNTS MAKE IT A SIZE RATHER THAN A HEDGE. "No setting here is per-machine" is a claim a reader
  // cannot size; the same claim with 3 machines and 2 pools in it says how much is not being configured.
  await expect(scope).toContainText("3 machines in 2 pools");

  // AND WHERE MACHINES ARE CONFIGURED. PALAI_RUNNER_CONCURRENCY is the one genuinely per-machine knob and
  // it is absent from the table BY DESIGN — the control plane holds no copy, so reporting it would be this
  // process reporting its own unset variable and labelling it the machine's.
  const elsewhere = page.getByTestId("desired-config-not-per-machine");
  await expect(elsewhere).toContainText("runner pool");
  await expect(elsewhere).toContainText("PALAI_RUNNER_CONCURRENCY");
  await expect(page.getByTestId("panel-deployment-settings").locator("table")).not.toContainText("PALAI_RUNNER_CONCURRENCY");
});
