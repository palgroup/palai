import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Request as NetRequest, type Response as NetResponse } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, chooseOption, chooseOptionByLabel, signIn } from "./profile";

// THE SCREEN THAT MAKES THE HEADLINE PROMISE TRUE (E29 provider wiring).
//
// The product's promise, in the owner's words: an operator installs Palai on their own machine, opens the
// console, adds their OpenAI / Anthropic / custom-endpoint credentials, and points agents at them. Before
// this screen NONE of that was possible from the panel — and the console said so out loud in the wrong
// direction, leading /registry with "This is a read surface — every row here is created by the API or the
// CLI." The API half was true. There was no CLI verb at all.
//
// THIS SPEC DRIVES THE SURFACE, NOT THE ENDPOINT, and that distinction is the reason it exists in this file
// rather than as another Go test. Four defects shipped in this tree in one day through exactly that gap —
// an auth suite that proved /api/console/login while no test ever submitted the form. So every assertion
// below starts from a control an operator can see and ends at a row an operator can read.
// Distinctive on purpose, and not a credential to anything: it exists so a substring scan over megabytes of
// minified JavaScript means something. A generic value would match by accident.
const SENTINEL = "PALAI-PROVIDER-SENTINEL-4f81be2c9d73-DO-NOT-LEAK";

test.beforeAll(() => announceProfile("provider-wiring.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// A distinct ref name per run: on the real profile connections accumulate, and a secret ref is versioned by
// name — reusing one would make "version 1" a lie on the second run.
const refName = () => `openai-${Date.now()}-${Math.floor(Math.random() * 10_000)}`;

test("an operator adds a provider connection through the form and the row appears", async ({ page }) => {
  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-connections")).toBeVisible({ timeout: 15_000 });

  const name = refName();
  await page.getByTestId("connection-secret-ref-input").fill(name);
  await page.getByTestId("connection-secret-input").fill(SENTINEL);
  await page.getByTestId("connection-create-button").click();

  // The status names the connection that now exists. It is a two-call flow — the secret is written first,
  // the connection then NAMES it — so a status that only said "done" would leave an operator unable to tell
  // which half failed.
  await expect(page.getByTestId("connection-create-status")).toContainText(name, { timeout: 15_000 });

  // THE FIELD RESET ITSELF. An uncontrolled input that keeps its value after submit leaves the credential in
  // the DOM for as long as the tab is open.
  await expect(page.getByTestId("connection-secret-input")).toHaveValue("");

  // The row is readable by its REF NAME. That is the whole thing the console is allowed to know about a
  // credential: what it is called.
  await expect(page.getByTestId("panel-model-connections")).toContainText(name);
});

// THE CUSTOM ENDPOINT — the third shape the owner named, and the one that needed a column.
//
// The endpoint field appears only for the family that takes one, and it is REQUIRED by it: a custom
// connection with a blank endpoint would dial api.openai.com carrying a key minted for a private server.
test("the custom family asks for an endpoint and the other families do not", async ({ page }) => {
  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-connections")).toBeVisible({ timeout: 15_000 });

  // OpenAI has an endpoint of its own, so the console does not ask for one.
  await expect(page.getByTestId("connection-endpoint-input")).toHaveCount(0);

  // chooseOption, NOT selectOption. This spec was written against a native <select>; the Base UI primitive
  // layer landed on main in the meantime and the seven native selects became a real listbox, so
  // `selectOption` — which requires a <select> element — no longer addresses this control at all.
  await chooseOption(page, "connection-provider-select", "openai-compatible");
  const endpoint = page.getByTestId("connection-endpoint-input");
  await expect(endpoint).toBeVisible();

  const name = refName();
  await page.getByTestId("connection-secret-ref-input").fill(name);
  await page.getByTestId("connection-secret-input").fill(SENTINEL);
  await endpoint.fill("http://127.0.0.1:11434/v1/chat/completions");
  await page.getByTestId("connection-create-button").click();

  await expect(page.getByTestId("connection-create-status")).toContainText(name, { timeout: 15_000 });
  // The ADDRESS reads back and the credential never does — the asymmetry is the design, not an oversight:
  // an operator must be able to see which of two connections points at their own model server.
  await expect(page.getByTestId("panel-model-connections")).toContainText("127.0.0.1:11434");
});

// THE VERIFY ACTION, and what it is allowed to say.
//
// A "test connection" that only checks the string is non-empty is theatre. This one is a real probe, so the
// screen must render a REJECTED credential as its own visible verdict — in TEXT, not colour — and must
// never render "nothing was checked" as a pass.
test("verify reports the endpoint's verdict in words", async ({ page }) => {
  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-connections")).toBeVisible({ timeout: 15_000 });

  const verify = page.getByTestId("connection-verify-button").first();
  await expect(verify).toBeVisible();
  await verify.click();

  const status = page.getByTestId("connection-verify-status");
  await expect(status).toBeVisible({ timeout: 15_000 });
  // A word, always — never a bare colour or glyph. The three outcomes an operator must be able to tell
  // apart are accepted / rejected / not checked, and each has to be legible as text.
  await expect(status).toContainText(/accepted|rejected|unreachable|not checked|Checking/i);
  // What a probe does NOT establish is on the screen, because an operator reading a green here must not
  // conclude their model id is valid.
  await expect(page.getByTestId("panel-model-connections")).toContainText(/model id/i);
});

// THE FALSE SENTENCE IS GONE. routes.ts led this screen with "This is a read surface — every row here is
// created by the API or the CLI", and both halves were wrong by the time an operator read them: the screen
// writes now, and the CLI verb it promised did not exist until E29 added `palai model`.
test("the screen no longer claims to be read-only", async ({ page }) => {
  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-connections")).toBeVisible({ timeout: 15_000 });
  const text = (await page.locator("main").innerText()).toLowerCase();
  expect(text.includes("this is a read surface"), "the read-surface claim survives on a screen that writes").toBe(false);
});

// THE CREDENTIAL NEVER COMES BACK — the same sweep secret-never-returns.spec.ts runs over the environment
// form, over the form that takes a PROVIDER key. It is a separate test from that one because it is a
// separate form: a screen inherits none of another screen's proofs.
test("a provider key written here appears in no DOM node and no response body", async ({ page }) => {
  const responses: NetResponse[] = [];
  page.on("response", (resp) => responses.push(resp));

  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-connections")).toBeVisible({ timeout: 15_000 });

  const name = refName();
  await page.getByTestId("connection-secret-ref-input").fill(name);
  await page.getByTestId("connection-secret-input").fill(SENTINEL);
  await page.getByTestId("connection-create-button").click();
  await expect(page.getByTestId("connection-create-status")).toContainText(name, { timeout: 15_000 });

  const domHaystack = await page.evaluate(() => {
    const inputs = [...document.querySelectorAll("input, textarea")].map((el) => (el as HTMLInputElement).value).join("\n");
    return `${document.documentElement.outerHTML}\n${inputs}`;
  });
  // A boolean, never `.not.toContain(SENTINEL)`: a failing matcher prints both operands, and here the
  // operand is the credential.
  expect(domHaystack.includes(SENTINEL), "the provider key is in the DOM").toBe(false);
  // The scan is only meaningful if it CAN see the page: the ref name it is allowed to find must be there.
  expect(domHaystack.includes(name), "the DOM scan read nothing — the sweep would be vacuous").toBe(true);

  let sawABody = false;
  for (const resp of responses) {
    let body: string;
    try {
      body = await resp.text();
    } catch {
      continue;
    }
    if (body.length > 0) sawABody = true;
    expect(body.includes(SENTINEL), `the provider key came back in a response from ${resp.url()}`).toBe(false);
  }
  expect(sawABody, "no response body was readable — the sweep would be vacuous").toBe(true);
});

// --- THE ROUTE: WHAT MAKES A SEALED CREDENTIAL ACTUALLY SERVE A RUN --------------------------------
//
// THE GAP THIS CLOSES, MEASURED ON THE LIVE STACK 2026-08-03 (127.0.0.1:60351):
//
//   GET /v1/model-connections -> 4 rows  (anth, pm-openai, anthropic-key, oai)
//   GET /v1/model-routes      -> 2 rows  (`default`, consulted_by_dispatch: true)
//   GET /v1/model-routes/mroute_63bd…/revisions -> 4 rows, exactly ONE with resolved_by_dispatch: true
//
// So three of the four connections on this stack steer nothing at all. Creating a connection is NOT
// choosing a model: coordinator.ProjectModelRoute (packages/coordinator/model_routes.go:95) resolves the
// project's published `default` route and, when the project has none, returns found=false and the caller
// FALLS BACK TO THE DEPLOYMENT DEFAULT — the env route built by main.modelBrokerFromEnv from
// PALAI_SECRET_PROVIDER_ONE, which is the 0600 file `palai up` writes out of OPENAI_API_KEY in .env.local.
//
// That is the whole reason this test exists. Before it, the console could seal a credential and name it,
// and every run went on using the credential from the FILE — silently, because a fallback that works looks
// exactly like a choice that took effect. An operator who added their Anthropic key on /registry and
// watched runs keep answering had no way to learn their key was never called.
//
// `grep -rn 'model-routes' apps/web-console/app` before this change -> ONE hit, a read-only Panel
// fetchPath. The console had no write to POST /v1/model-routes, /revisions or /publish at all.
//
// THE ALIAS IS NOT A FREE FIELD, and the console must not offer it as one. internal/store/model_routes.go
// CreateModelRoute REFUSES any name but `default` — verbatim, "a route by any other name would be created,
// published, and never consulted by a single run". So the form has no name field: the console creates the
// one alias dispatch reads.
test("publishing a route makes a console-sealed connection the credential runs actually use", async ({ page }) => {
  const requests: NetRequest[] = [];
  page.on("request", (r) => requests.push(r));

  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-connections")).toBeVisible({ timeout: 15_000 });

  // Seal a credential and name it, through the form — the flow the first test in this file proves.
  const name = refName();
  await page.getByTestId("connection-secret-ref-input").fill(name);
  await page.getByTestId("connection-secret-input").fill(SENTINEL);
  await page.getByTestId("connection-create-button").click();
  await expect(page.getByTestId("connection-create-status")).toContainText(name, { timeout: 15_000 });

  // NOW POINT RUNS AT IT. This is the control that did not exist.
  await page.getByTestId("route-publish-open").click();
  await expect(page.getByTestId("route-publish-dialog")).toBeVisible();
  // The picker offers the connection just created, BY ITS REF NAME — the only thing the console is allowed
  // to know about a credential is what it is called, so that is what an operator picks by.
  await chooseOptionByLabel(page, "route-connection-select", name);
  await page.getByTestId("route-model-input").fill("gpt-4o-mini");
  await page.getByTestId("route-publish-button").click();

  // THE STATUS SAYS THE THING THAT WAS PREVIOUSLY UNSAYABLE: runs now use THIS credential, and no longer
  // the deployment default. A status that only said "published" would leave an operator unable to tell a
  // stored revision from a steering one — which is the exact confusion the store's own refusal describes.
  const status = page.getByTestId("route-publish-status");
  await expect(status).toContainText(name, { timeout: 15_000 });
  await expect(status).toContainText("gpt-4o-mini");
  await expect(status).not.toContainText(SENTINEL);

  // THREE CALLS, IN THE API'S OWN ORDER, and the last one is the publish. A draft revision steers nothing
  // (api/model_routes.go:76 — "A draft never steers a run; publishing it does"), so a flow that stopped at
  // the revision would leave the operator exactly where they started while reporting success.
  // THE IDS ARE REDACTED BY POSITION, NOT BY PREFIX. The first draft matched `/mroute_\w+/` and the fixture's
  // seeded alias is `mr_1`, so the redaction silently missed and the diff read as a wrong CALL ORDER when the
  // order was right — an assertion failing for a reason unrelated to what it claims. A path shape is a shape:
  // whatever sits after /model-routes/ is the route, whatever sits after /revisions/ is the revision.
  const writes = requests
    .filter((r) => r.method() === "POST" && r.url().includes("/api/palai/v1/"))
    .map((r) =>
      new URL(r.url()).pathname
        .replace("/api/palai/v1", "")
        .replace(/^\/model-routes\/[^/]+/, "/model-routes/{route}")
        .replace(/\/revisions\/[^/]+/, "/revisions/{rev}"),
    );
  expect(writes.slice(-3)).toEqual([
    "/model-routes",
    "/model-routes/{route}/revisions",
    "/model-routes/{route}/revisions/{rev}/publish",
  ]);

  // AND THE CREDENTIAL WENT NOWHERE NEAR THE ROUTING CALLS. A route revision names a CONNECTION id; the
  // bytes were sealed earlier in this test and must not reappear on the three calls that follow.
  //
  // THE SWEEP IS SCOPED TO THE ROUTING CALLS ON PURPOSE, and the first draft was not — it swept EVERY
  // request and failed on `POST /secret-refs`, the one call in this test that is SUPPOSED to carry the
  // credential because this test seals one as its own setup. That failure read as "the console leaked a
  // key" when the console had done nothing wrong. An assertion that cannot distinguish the call that
  // legitimately carries a secret from the calls that must not is not a leak test; it is a tripwire on its
  // own fixture.
  const routingCalls = requests.filter((r) => r.method() === "POST" && r.url().includes("/api/palai/v1/model-route"));
  expect(routingCalls.length, "no routing call was examined — this sweep would be vacuous").toBe(3);
  for (const r of routingCalls) {
    expect((r.postData() ?? "").includes(SENTINEL), `the credential rode ${r.url()}`).toBe(false);
  }
  // THE POSITIVE CONTROL: the needle IS findable when it is genuinely there. Without this the sweep above
  // would report the same green if `SENTINEL` had been mistyped or the body were unreadable.
  const seal = requests.find((r) => r.method() === "POST" && r.url().endsWith("/api/palai/v1/secret-refs"));
  expect(seal, "the seal call was not observed — the sweep above has no positive control").toBeDefined();
  expect((seal?.postData() ?? "").includes(SENTINEL), "the seal call did not carry the credential").toBe(true);

  // AND IT IS IN NO URL, on any call — which is what a form that fell back to a native GET would produce.
  for (const r of requests) expect(r.url().includes(SENTINEL), "the credential is in a URL").toBe(false);
});

// CHOOSING A MODEL TWICE LEAVES ONE ALIAS, NOT TWO.
//
// THE OBVIOUS ASSERTION HERE IS WRONG, and it is written down because it nearly shipped. The first draft of
// this test asserted that a second publish makes NO `POST /model-routes` call — reasoning that the alias
// already exists, so re-creating it must be the defect. Then the store was read:
// packages/coordinator/model_routes.go:318-332 looks the alias up by name and RETURNS THE EXISTING ID
// before it ever inserts. `CreateModelRoute` is get-or-create, which is why cmd/cli's routeThroughConnection
// POSTs it unconditionally and calls that "get-or-create" in its own comment.
//
// So the call is not the defect, and a test that forbade it would have forced the console into a
// client-side GET-then-maybe-POST: more calls, and racy between two operators on one project. The property
// worth pinning is the one an operator can see — ONE alias, and the newest revision is the one that steers.
test("choosing a model twice leaves ONE default alias and the latest choice steering", async ({ page }) => {
  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-routes")).toBeVisible({ timeout: 15_000 });

  const publish = async (model: string) => {
    await page.getByTestId("route-publish-open").click();
    await expect(page.getByTestId("route-publish-dialog")).toBeVisible();
    // THE CONNECTION IS CHOSEN EVERY TIME, and the first draft of this helper forgot to — which failed on a
    // missing status and read as "the publish is broken" when the console was correctly refusing a form with
    // no connection named. The picker holds no selection across two openings of the dialog, and it should
    // not: a remembered credential is one an operator did not look at.
    await chooseOption(page, "route-connection-select", "mc_1");
    await page.getByTestId("route-model-input").fill(model);
    await page.getByTestId("route-publish-button").click();
    await expect(page.getByTestId("route-publish-status")).toContainText(model, { timeout: 15_000 });
  };

  await publish("gpt-4o");
  await publish("gpt-4o-mini");

  // ONE ROW NAMED `default`. A second alias would be a route that steers nothing, which is exactly what the
  // store's create refusal calls out — and the operator would have no way to tell which of the two is live.
  const rows = page.getByTestId("panel-model-routes").locator("tbody tr", { hasText: "default" });
  await expect(rows).toHaveCount(1);
});

// THE DIALOG IS AXE-CLEAN WITH ITS CONTROLS RENDERED — scanned OPEN, for the reason
// tests/repository-token.spec.ts states at length: this console lost 144 controls from a contrast sweep
// that only ever ran on closed routes, and reported a CLEANER number while doing it.
test("the publish-route dialog is axe-clean with the controls RENDERED", async ({ page }) => {
  await page.goto("/registry");
  await expect(page.getByTestId("panel-model-routes")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("route-publish-open").click();
  await expect(page.getByTestId("route-model-input")).toBeVisible();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.length} node(s)`)).toEqual([]);
});
