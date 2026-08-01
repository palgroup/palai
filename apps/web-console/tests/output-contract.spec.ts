import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Page } from "@playwright/test";

import { UPSTREAM } from "./constants";
import { WCAG_TAGS } from "./constants";
import { announceProfile, signIn, skipOnReal } from "./profile";

// THE OUTPUT CONTRACT, FROM THE SCREEN (spec §22.7).
//
// EVERY CASE HERE DRIVES THE FORM. Not the relay, not the SDK — the textarea an operator types into and the
// button they press. This tree found ten forms whose tests never submitted them, and the sharpest of them
// proved a login nine ways while never once driving the form whose missing `method` put the password in the
// URL. So `page.getByTestId("output-schema-input").fill(...)` followed by a click on the run button is the
// whole point of the file, and an assertion that could pass without those two lines does not belong in it.
//
// AND THE ASSERTION IS ON THE WIRE, NOT THE OUTCOME. The relay calls the upstream SERVER-SIDE, so a
// browser-side route interception cannot see that request; a spec that checked only the rendered result
// could not tell "the console sent the schema" from "the console sent nothing and the fixture answered the
// same either way". The fake upstream records the `output` field of the last create
// (fake-control-plane.mjs introspect.lastCreateOutput) and these cases read it back. That is the difference
// between proving the feature and proving the page renders.
test.beforeAll(() => announceProfile("output-contract.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

const CITY_SCHEMA = '{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}';

async function lastCreateOutput(request: { get: (url: string) => Promise<{ json: () => Promise<unknown> }> }): Promise<unknown> {
  const seen = (await (await request.get(`${UPSTREAM}/__introspect`)).json()) as { lastCreateOutput?: unknown };
  return seen.lastCreateOutput ?? null;
}

async function startRun(page: Page, schema: string): Promise<void> {
  await page.goto("/runs");
  await expect(page.getByTestId("prompt-input")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("prompt-input").fill("Name a city.");
  if (schema !== "") await page.getByTestId("output-schema-input").fill(schema);
  await page.getByTestId("run-button").click();
}

test("the control exists on the screen a human uses", async ({ page }) => {
  await page.goto("/runs");
  const box = page.getByTestId("output-schema-input");
  await expect(box).toBeVisible({ timeout: 15_000 });
  // A programmatic label, not a placeholder — the ResourceForm discipline, WCAG 2.2 §3.3.2.
  await expect(page.locator('label[for="output-schema-input"]')).toContainText("Output JSON Schema");
  // The box starts EMPTY: free-form text is the default and this feature is opt-in.
  await expect(box).toHaveValue("");
});

test("a schema typed into the box reaches the upstream create body", async ({ page, request }) => {
  // The fixture is the only end that can report what the relay sent (DIV-UI-004: a real control plane has
  // no introspection endpoint).
  skipOnReal("DIV-UI-004");

  await startRun(page, CITY_SCHEMA);
  await expect(page.getByTestId("status")).not.toContainText("idle", { timeout: 15_000 });

  const output = (await lastCreateOutput(request)) as { format?: string; schema?: unknown; strict?: boolean } | null;
  expect(output, "the create carried no `output` at all — the schema the operator typed went nowhere").not.toBeNull();
  expect(output?.format).toBe("json_schema");
  expect(output?.strict).toBe(true);
  expect(output?.schema).toEqual(JSON.parse(CITY_SCHEMA));
});

test("a run with an empty box sends NO output key at all", async ({ page, request }) => {
  skipOnReal("DIV-UI-004");

  await startRun(page, "");
  await expect(page.getByTestId("status")).not.toContainText("idle", { timeout: 15_000 });

  // Absent, not `{}` and not `{"format":"text"}`: an unconstrained run's upstream body must stay exactly
  // what it was before §22.7 existed. The opt-in fence, asserted on the wire.
  expect(await lastCreateOutput(request)).toBeNull();
});

test("malformed JSON is refused before submit, and the refusal is shown", async ({ page, request }) => {
  skipOnReal("DIV-UI-004");

  // Establish a known upstream state, so "nothing was sent" is checkable rather than assumed.
  await startRun(page, "");
  await expect(page.getByTestId("status")).not.toContainText("idle", { timeout: 15_000 });

  await page.goto("/runs");
  await page.getByTestId("prompt-input").fill("Name a city.");
  await page.getByTestId("output-schema-input").fill('{"type":"object",');
  await page.getByTestId("run-button").click();

  const error = page.getByTestId("output-schema-error");
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toContainText("not valid JSON");
  // role="alert" so the refusal is ANNOUNCED, not merely drawn (WCAG §3.3.1, the ResourceForm discipline).
  await expect(error).toHaveAttribute("role", "alert");

  // And nothing was sent: a malformed schema must not start a run. Still null from the run above.
  expect(await lastCreateOutput(request)).toBeNull();
});

test("a JSON array is refused — a schema must be an object", async ({ page }) => {
  await page.goto("/runs");
  await page.getByTestId("prompt-input").fill("Name a city.");
  await page.getByTestId("output-schema-input").fill('[{"type":"object"}]');
  await page.getByTestId("run-button").click();
  await expect(page.getByTestId("output-schema-error")).toContainText("must be a JSON object", { timeout: 15_000 });
});

test("the server's refusal of an unenforceable schema is surfaced, not swallowed", async ({ page }) => {
  skipOnReal("DIV-UI-004");

  // `oneOf` is well-formed JSON, so the browser parse accepts it and the refusal can only come from the
  // server. That is the case worth having: it proves the console shows a refusal it did not author.
  await startRun(page, '{"type":"object","properties":{},"oneOf":[{"type":"object"}]}');
  const error = page.getByTestId("run-error");
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toContainText("oneOf");
});

test("the schema box is keyboard reachable and the page stays axe-clean with it filled", async ({ page }) => {
  await page.goto("/runs");
  await expect(page.getByTestId("output-schema-input")).toBeVisible({ timeout: 15_000 });

  // Filled, because an axe scan of an EMPTY control is a scan of a different page than the one an operator
  // has in front of them — and a scan that runs zero rules reports zero violations too.
  await page.getByTestId("output-schema-input").fill(CITY_SCHEMA);
  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  expect(results.passes.length + results.violations.length + results.incomplete.length).toBeGreaterThan(0);

  // Reachable by Tab from the prompt — no tabindex, real elements in document order.
  await page.getByTestId("prompt-input").focus();
  for (let i = 0; i < 12; i += 1) {
    if ((await page.evaluate(() => document.activeElement?.getAttribute("data-testid"))) === "output-schema-input") return;
    await page.keyboard.press("Tab");
  }
  throw new Error("the output-schema textarea was not reachable by Tab from the prompt");
});
