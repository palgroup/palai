import { test, expect, type Response as NetResponse } from "@playwright/test";

import { announceProfile, signIn } from "./profile";

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

  await page.getByTestId("connection-provider-select").selectOption("openai-compatible");
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
