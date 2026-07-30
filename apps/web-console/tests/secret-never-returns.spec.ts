import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Response as NetResponse } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, browserServedAssets, signIn } from "./profile";

// THE SENTINEL SWEEP — and its GREENNESS IS THE PROOF OF AN ABSENCE (E25 T4, plan §T4).
//
// This spec writes an environment value through the console's own form and then goes looking for it: in the
// DOM, in every response body the browser received, and in every browser-served source map. Every one of the
// three has to come back empty. That is the only kind of proof this screen can offer — there is no positive
// assertion available, because the whole point of the screen is that what you typed is gone.
//
// It could not run before T4: there was no page to type into. It is the sibling of
// public-api-only.spec.ts's credential sweep, and `productionBrowserSourceMaps: true` (next.config.ts:8) is
// what makes the source-map arm a measurement rather than a hope.
//
// THE SENTINEL IS DISTINCTIVE ON PURPOSE and it is not a credential to anything: it exists so a substring
// scan over megabytes of minified JavaScript means something. A generic value ("hunter2") would match by
// accident and a short one would match a base64 fragment; this one appears nowhere in the tree but here.
const SENTINEL = "PALAI-ENV-SENTINEL-c7f1a94e2b3d-DO-NOT-LEAK";

// A second sentinel for the rotation leg, so "the version went up" is not the only thing distinguishing a
// rotation from a re-write of the same bytes.
const ROTATED = "PALAI-ENV-SENTINEL-ROTATED-9a3e5f18c6b2-DO-NOT-LEAK";

test.beforeAll(() => announceProfile("secret-never-returns.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// envName keeps runs from colliding on the real profile, where environments accumulate and the name is
// UNIQUE per organization (migration 000046). On the fake profile it is equally harmless.
const envName = () => `t4-sweep-${Date.now()}-${Math.floor(Math.random() * 10_000)}`;

test("a value written through the console appears in no DOM node, no response body and no source map", async ({ page }) => {
  // Capture EVERY response, not just the JSON ones: a server component that leaked the value into a client
  // prop ships in the page HTML / RSC flight payload, which is neither a request nor a static chunk.
  const responses: NetResponse[] = [];
  page.on("response", (resp) => responses.push(resp));

  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });

  // 1. Create an environment.
  const name = envName();
  await page.getByTestId("env-name-input").fill(name);
  await page.getByTestId("env-create-button").click();
  await expect(page.getByTestId("environment-create-status")).toContainText(name, { timeout: 15_000 });

  // 2. Write a key into it, through the SecretField.
  await page.getByTestId("value-key-input").fill("SWEEP_TOKEN");
  await page.getByTestId("value-secret-input").fill(SENTINEL);
  await page.getByTestId("value-write-button").click();
  await expect(page.getByTestId("environment-value-status")).toContainText("version 1", { timeout: 15_000 });

  // THE FIELD RESET ITSELF. An uncontrolled input that keeps its value after submit leaves the secret in the
  // DOM for as long as the tab is open — which is the leak this whole spec is about, sitting in the one place
  // the operator can see.
  await expect(page.getByTestId("value-secret-input")).toHaveValue("");

  // 3. The key appears by NAME, with a version and an update time. This is what the screen is allowed to know.
  const keys = page.getByTestId("panel-environment-keys");
  await expect(keys).toContainText("SWEEP_TOKEN");
  await expect(keys).toContainText("1");

  // --- ARM 1: THE DOM. The whole rendered document, plus every input's live VALUE (which is not in the
  // serialized HTML at all — `input.value` is a property, so innerHTML would miss exactly the field the
  // operator just typed into). ---
  const domHaystack = await page.evaluate(() => {
    const inputs = [...document.querySelectorAll("input, textarea")].map((el) => (el as HTMLInputElement).value).join("\n");
    return `${document.documentElement.outerHTML}\n${inputs}`;
  });
  // A boolean, never `.not.toContain(SENTINEL)`: a failing matcher prints both operands, and on this suite
  // the operand is the secret. The same discipline public-api-only.spec.ts applies to the API key.
  expect(domHaystack.includes(SENTINEL), "the written value is in the DOM").toBe(false);
  // The scan is only meaningful if it CAN see the page: the key name it is allowed to find must be there.
  expect(domHaystack.includes("SWEEP_TOKEN"), "the DOM scan read nothing — the sweep would be vacuous").toBe(true);

  // --- ARM 2: EVERY RESPONSE BODY the browser received during the whole journey. ---
  //
  // EACH ARM NAMES A TOKEN IT ACTUALLY FOUND (E25 T9). Arm 1 always did — it looks for SWEEP_TOKEN in the
  // DOM — and arms 2 and 3 only ever proved they had SOMETHING to scan (`scannedBodies > 0`,
  // `assets.length > 0`), never that the scan could see a string genuinely in the bytes. A haystack nobody
  // has shown was READ is not a haystack, and the exit gate's byte-scan group refuses a layer that names no
  // probe. The probe must be a token that is ALLOWED to be there: the key NAME comes back from the API by
  // design, which is exactly the difference between a name and a value that this screen is about.
  const BODY_PROBE = "SWEEP_TOKEN";
  let scannedBodies = 0;
  let bodyProbeHits = 0;
  let scannedBodyBytes = 0;
  for (const resp of responses) {
    let body: string;
    try {
      body = await resp.text();
    } catch {
      continue; // not retained (redirect / aborted / 204) — nothing to scan
    }
    scannedBodies += 1;
    scannedBodyBytes += body.length;
    if (body.includes(BODY_PROBE)) bodyProbeHits += 1;
    expect(body.includes(SENTINEL), `the written value came back in a response body from ${resp.url()}`).toBe(false);
  }
  expect(scannedBodies, "no response bodies were retained — arm 2 would be vacuous").toBeGreaterThan(0);
  expect(
    bodyProbeHits,
    `no response body contained "${BODY_PROBE}" — the key NAME is what the API is allowed to return, so a scan ` +
      "that cannot find it has not been shown to read these bodies at all, and its zero is a statement about " +
      "a haystack rather than about a secret",
  ).toBeGreaterThan(0);

  // --- ARM 3: EVERY BROWSER-SERVED ASSET, INCLUDING SOURCE MAPS. Enumerated by walking .next/static
  // recursively — every file under it is fetchable at /_next/static/..., so this is the browser surface, and
  // it is the same walk the credential sweep uses. ---
  //
  // Its probe is a string this console's OWN client code carries and which a browser is meant to receive: the
  // environment panel's test id. It is searched in the SOURCE MAPS specifically, because that is the layer
  // whose whole reason to exist is that it carries text the minified chunk does not — a sentinel planted in a
  // source COMMENT is stripped from the chunk and survives verbatim in `sourcesContent`.
  const MAP_PROBE = "panel-environment-keys";
  const assets = browserServedAssets();
  const maps = assets.filter((a) => a.path.endsWith(".js.map"));
  expect(assets.length, "no browser-served assets were found — arm 3 would be vacuous").toBeGreaterThan(0);
  expect(maps.length, "expected browser source maps (next.config.ts productionBrowserSourceMaps)").toBeGreaterThan(0);
  expect(
    maps.filter((m) => m.body.includes(MAP_PROBE)).length,
    `no source map contained "${MAP_PROBE}" — the maps are the layer that carries text the minified chunk does ` +
      "not, so a scan that cannot find this console's own markup in them has not been shown to read them",
  ).toBeGreaterThan(0);
  for (const asset of assets) {
    expect(asset.body.includes(SENTINEL), `the written value is in ${asset.path}`).toBe(false);
  }

  // eslint-disable-next-line no-console -- the counts ARE the evidence; an absence proven over nothing is
  // nothing, and the PROBE counts are what say the scan read anything at all. tests/uat/admin-console parses
  // this line, so its shape is a contract rather than a log.
  console.log(
    `SENTINEL SWEEP — dom=${domHaystack.length} probe=SWEEP_TOKEN found; ` +
      `response-body=${scannedBodyBytes} bodies=${scannedBodies} probe=${BODY_PROBE} hits=${bodyProbeHits}; ` +
      `source-map=${maps.reduce((n, m) => n + m.body.length, 0)} maps=${maps.length} assets=${assets.length} ` +
      `probe=${MAP_PROBE} hits=${maps.filter((m) => m.body.includes(MAP_PROBE)).length}; ` +
      "sentinel found in 0",
  );
});

// THE ABSENCE OF A REVEAL BUTTON, ASSERTED. There is no control anywhere on this screen that offers to show a
// written value, and the screen says so in words. Eyeballing a missing button is not asserting it: this
// enumerates the page's OWN controls and checks that none of them is one.
//
// Shown failing during development by adding a `<button data-testid="reveal-value">Show value</button>` to
// app/environments/page.tsx — this test named it, on both arms.
test("no control on the environment screen offers to reveal a value, and the screen says why", async ({ page }) => {
  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });

  // ARM A: by ROLE, over every button/link/testid on the page. A reveal control has to be operable, so it has
  // to be one of these — and the words are matched case-insensitively across the label AND the test id,
  // because a "Show", an "Unmask" and a "reveal-value" are the same button with three names.
  const forbidden = /reveal|unmask|show\s*(the\s*)?(value|secret)|view\s*(the\s*)?(value|secret)|copy\s*(the\s*)?(value|secret)|plaintext/i;
  const controls = await page.evaluate(() =>
    [...document.querySelectorAll("button, a, [role=button], input[type=button], summary")].map((el) => ({
      text: (el.textContent ?? "").trim(),
      testId: el.getAttribute("data-testid") ?? "",
    })),
  );
  expect(controls.length, "no controls were found — this assertion would be vacuous").toBeGreaterThan(0);
  const offenders = controls.filter((c) => forbidden.test(c.text) || forbidden.test(c.testId));
  expect(offenders, "a control on the environment screen offers to reveal a value").toEqual([]);

  // ARM B: the absence is STATED, not merely true. An operator who cannot read a value back needs to be told
  // that before they lose it, not after.
  await expect(page.getByTestId("env-writeonly-note")).toContainText("cannot be read back");
  await expect(page.getByTestId("env-writeonly-note")).toContainText("rotate");

  // eslint-disable-next-line no-console -- the count is the evidence.
  console.log(`REVEAL-BUTTON ABSENCE — ${controls.length} control(s) enumerated, 0 offer to reveal a value`);
});

test("rotate is the same route as create and it moves the version; unbinding drops the key from the list", async ({ page }) => {
  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });

  const name = envName();
  await page.getByTestId("env-name-input").fill(name);
  await page.getByTestId("env-create-button").click();
  await expect(page.getByTestId("environment-create-status")).toContainText(name, { timeout: 15_000 });

  // The wire is watched, because "rotate is the same route as create" is a claim about a REQUEST, not about a
  // label. Two writes, one path, one method.
  const writes: string[] = [];
  page.on("request", (req) => {
    if (req.method() === "POST" && req.url().includes("/values")) writes.push(new URL(req.url()).pathname);
  });

  await page.getByTestId("value-key-input").fill("ROTATE_ME");
  await page.getByTestId("value-secret-input").fill(SENTINEL);
  await page.getByTestId("value-write-button").click();
  await expect(page.getByTestId("environment-value-status")).toContainText("version 1", { timeout: 15_000 });

  // ROTATE: the same form, the same button, a new value.
  await page.getByTestId("value-key-input").fill("ROTATE_ME");
  await page.getByTestId("value-secret-input").fill(ROTATED);
  await page.getByTestId("value-write-button").click();
  await expect(page.getByTestId("environment-value-status")).toContainText("version 2", { timeout: 15_000 });

  // The key list carries the NEW version. One row, not two: a rotation is a new version of one key.
  // `exact` matters: the row's own cell is `ROTATE_ME` and its unbind control reads "Unbind ROTATE_ME", so a
  // substring match would count two and say nothing about whether the key was duplicated.
  const keys = page.getByTestId("panel-environment-keys");
  await expect(keys.getByText("ROTATE_ME", { exact: true })).toHaveCount(1);
  await expect(keys).toContainText("2");

  expect(writes.length, "expected exactly two value writes").toBe(2);
  expect(new Set(writes).size, `create and rotate used different paths: ${writes.join(" vs ")}`).toBe(1);

  // UNBIND. The confirmation says what it actually does — the BINDING, not the bytes — and it is a real
  // dialog the operator has to answer, not a silent delete.
  let dialogMessage = "";
  page.once("dialog", async (dialog) => {
    dialogMessage = dialog.message();
    await dialog.accept();
  });
  await page.getByTestId("unbind-ROTATE_ME").click();
  await expect(page.getByTestId("panel-environment-keys")).not.toContainText("ROTATE_ME", { timeout: 15_000 });

  // THE CONFIRMATION SAID WHAT THE BUTTON ACTUALLY DOES. Asserted after the fact rather than inside the
  // handler, because an assertion that throws inside a dialog listener never rejects the dialog — the click
  // hangs and the failure arrives as a timeout about the wrong thing.
  expect(dialogMessage.toLowerCase()).toContain("binding");
  expect(dialogMessage.toLowerCase()).toContain("not the stored value");
  expect(dialogMessage).toContain("ROTATE_ME");
  // And the outcome the console reports is the SERVER's own sentence about what was removed, not a paraphrase
  // of it: this console does not author claims about whether a credential still exists.
  await expect(page.getByTestId("env-unbind-status")).toContainText("the binding was removed");
  await expect(page.getByTestId("env-unbind-status")).toContainText("retained and unreachable");
});

test("the environment picker is a select over the ids the list returned, never a free-text box", async ({ page }) => {
  // A free-text environment id is a value an operator can typo into a run that then fails at admission with a
  // refusal about something else entirely, so the field refuses to exist rather than accept a guess. T6 puts
  // this same field on the agent-revision form.
  //
  // THE EMPTY HALF OF THE CLAIM IS ITS OWN TEST BELOW, because it depends on the collection being empty —
  // true of a fresh fixture, false of a fixture this file has already written to, and unknowable of a real
  // stack. A branch here would have been a skip in disguise.
  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });

  // Guarantee the non-empty state on BOTH profiles rather than depending on accumulated state.
  await page.getByTestId("env-name-input").fill(envName());
  await page.getByTestId("env-create-button").click();
  await expect(page.getByTestId("environment-create-status")).toContainText("Created", { timeout: 15_000 });

  const picker = page.getByTestId("value-environment-select");
  await expect(picker).toHaveJSProperty("tagName", "SELECT");
  // ResourceForm renders `${testId}-empty` in the control's PLACE when a select has no options, so this
  // locator finding nothing is the positive statement that the control itself is present.
  await expect(page.getByTestId("value-environment-select-empty")).toHaveCount(0);

  // The environment is never a free-text field. Asserted over the whole form rather than over the one testid,
  // so a second hand-rolled text box would fail too.
  const form = page.getByTestId("environment-value-form");
  const textInputs = await form.locator('input[type="text"], input:not([type])').evaluateAll((els) =>
    els.map((el) => el.getAttribute("data-testid") ?? el.getAttribute("name") ?? "<unnamed>"),
  );
  expect(textInputs, "the value form must carry exactly one text field (the KEY NAME) and no free-text environment box").toEqual([
    "value-key-input",
  ]);

  // AND THE ORG-SCOPE FACT IS ON THE SCREEN (plan §3.6 D12). An environment is org-scoped, matching
  // secret_refs, so two projects in one organization see the same environments. An operator who believes
  // otherwise scopes a production credential to a project that does not exist.
  await expect(page.getByTestId("env-scope-note")).toContainText("organization");

  // THE OPTIONS ARE THE API'S OWN LIST — on BOTH profiles. The console does not invent, cache or filter an
  // environment id: what /v1/environments returns is exactly what the dropdown offers, so an operator cannot
  // select something the control plane has never heard of.
  const listed = await page.evaluate(async () => {
    const res = await fetch("/api/palai/v1/environments", { headers: { Accept: "application/json" } });
    return ((await res.json()) as { data?: { id: string }[] }).data?.map((e) => e.id) ?? [];
  });
  expect(listed.length, "the upstream listed no environments after a create — this comparison would be vacuous").toBeGreaterThan(0);
  const offered = await picker.locator("option").evaluateAll((els) => els.map((el) => (el as HTMLOptionElement).value).filter((v) => v !== ""));
  expect(offered.sort()).toEqual([...listed].sort());

  // AXE OVER THE POPULATED FORM, deterministically and on both profiles. The generated scan in
  // tests/a11y.spec.ts visits this route in whatever state the upstream happens to be in — which is the EMPTY
  // picker on a fresh fixture and the SELECT on an accumulated real stack, so which branch got scanned was an
  // accident of profile and file order. Both branches are now scanned on purpose: this one here, the empty one
  // in the next test. `autocomplete-valid` (wcag21aa) and `target-size` (wcag22aa) are in these tags, which is
  // why the credential field and the small controls are inside the claim rather than beside it.
  await expectAxeClean(page);
});

test("with no environments the picker does not exist at all — a refusal with a link, not a text box", async ({ page }) => {
  // THE LIST RESPONSE IS STUBBED AT THE BROWSER BOUNDARY, and that is what makes this deterministic on BOTH
  // profiles instead of a branch on whatever the upstream happens to hold. What is under test is the console's
  // rendering of an empty collection, not the upstream's ability to be empty — and "the collection is empty"
  // is a state a real organization is in on its first day and can never be put back into afterwards.
  //
  // The stub is the RELAY's own path, so everything else — the session gate, the relay, the page's fetch —
  // still runs exactly as it does normally.
  await page.route("**/api/palai/v1/environments", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ object: "list", data: [] }) }),
  );

  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments-empty")).toBeVisible({ timeout: 15_000 });

  // The control does not exist — not disabled, not empty: absent.
  await expect(page.getByTestId("value-environment-select")).toHaveCount(0);
  const empty = page.getByTestId("value-environment-select-empty");
  await expect(empty).toContainText("Create an environment first");
  await expect(empty.getByRole("link")).toHaveAttribute("href", "#environment-create");

  // AND NO FREE-TEXT FALLBACK TOOK ITS PLACE. The value form's text inputs are enumerated, so a hand-rolled
  // "environment id" box anywhere in it would fail — this is the assertion, and the absence is the claim.
  const textInputs = await page
    .getByTestId("environment-value-form")
    .locator('input[type="text"], input:not([type])')
    .evaluateAll((els) => els.map((el) => el.getAttribute("data-testid") ?? el.getAttribute("name") ?? "<unnamed>"));
  expect(textInputs, "an empty environment list degraded into a free-text box").toEqual(["value-key-input"]);

  // A label pointing at a control that does not exist would be an axe violation and a lie to a screen reader —
  // asserted directly, and then handed to axe, which is the arm that would catch the ones nobody thought of.
  await expect(page.locator('label[for="field-environment"]')).toHaveCount(0);
  await expectAxeClean(page);
});

// expectAxeClean runs the suite's shared tag set over whatever is currently on screen. It is a helper rather
// than an inline builder because two tests here scan two DIFFERENT STATES of the same page, and the point is
// that the tag set is identical in both — a scan that quietly narrowed its tags would be the vacuous one.
async function expectAxeClean(page: import("@playwright/test").Page): Promise<void> {
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  // A scan that ran ZERO rules reports zero violations too, which is the same output as a clean page.
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);
}

// THE SECRET FIELD'S ATTRIBUTES, READ OFF THE LIVE FIELD (plan §3.5 N17). Asserted on the rendered page rather
// than on the component's source, because what protects the operator is what the BROWSER receives.
test("the value field is a new-password password field and is uncontrolled", async ({ page }) => {
  await page.goto("/environments");
  await expect(page.getByTestId("panel-environments")).toBeVisible({ timeout: 15_000 });

  const field = page.getByTestId("value-secret-input");
  await expect(field).toHaveAttribute("type", "password");
  // `new-password`, NOT `off`. MDN: `off` "will not prevent a password manager from asking the user if they
  // would like to save … or from automatically filling in those values", and it is for CAPTCHA / one-time
  // fields; `new-password` is the token that "avoid[s] accidentally filling in an existing password" — which
  // here means the operator's own console password being sealed as a credential an agent will use.
  await expect(field).toHaveAttribute("autocomplete", "new-password");
  await expect(field).toHaveAttribute("spellcheck", "false");
  // It starts empty and it has NO value attribute: an uncontrolled field whose bytes exist only in the DOM.
  await expect(field).toHaveValue("");
  expect(await field.evaluate((el) => el.hasAttribute("value")), "the field carries a value attribute").toBe(false);

  // It is programmatically labelled and keyboard-reachable — the same two rules every other field on this
  // surface follows (ResourceForm's header), and a credential field is the last place to skip them.
  const id = await field.getAttribute("id");
  await expect(page.locator(`label[for="${id}"]`)).toHaveText("Value");
  await field.focus();
  await expect(field).toBeFocused();
});
