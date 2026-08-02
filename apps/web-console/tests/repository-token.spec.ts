import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Request as NetRequest, type Response as NetResponse } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, chooseOption, signIn } from "./profile";

// THE TOKEN PATH, DRIVEN THROUGH THE FORM AN OPERATOR ACTUALLY USES.
//
// WHAT THIS EXISTS FOR, measured rather than asserted. A private repository clones under the tenant's own
// Git credential: repository_bindings.connection_ref names a secret_refs row, bindingBroker resolves it and
// hands repositories.NewTokenBroker the bytes (internal/execution/repository.go:131-146). That mechanism is
// proven — repository_binding_component_test.go drives it against a real Postgres, real git and a real
// authenticated HTTP remote. What did NOT exist was the surface: /repositories could PICK a connection_ref
// from GET /v1/secret-refs and could not create one, so an operator binding a private repository had to go
// to /registry — a page for tool-registry connections — seal a secret there, and come back.
//
// Measured on the live stack, 2026-08-02:
//   curl -K <authfile> 'http://127.0.0.1:60351/v1/repository-bindings?limit=100' | jq '[.data[].connection_ref]'
//     -> 20 rows, every one absent. Nothing had ever used the path.
//
// THE SPEC DRIVES THE FORM, NOT THE ENDPOINT, and that is the whole discipline this file is written under.
// This tree proved a login nine ways with `fetch` while never submitting the form whose missing `method` put
// the password in the URL. So every assertion below goes through fill() and click() on the real controls,
// and one of them reads the rendered form's `method` attribute — because a credential field on a form that
// defaults to GET is a credential in the address bar for as long as hydration has not landed.
//
// THE TOKEN IS DISTINCTIVE ON PURPOSE, the same reason secret-never-returns.spec.ts gives: a substring scan
// over request bodies and DOM text means nothing if the needle could match by accident.
const TOKEN = "PALAI-REPO-TOKEN-SENTINEL-4f8c1e07a5b9-DO-NOT-LEAK";

test.beforeAll(() => announceProfile("repository-token.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// Names are unique per organization on the real profile (secret_refs is keyed by (org, name)), and a re-post
// of an existing name ROTATES rather than failing — so a fixed name would silently version up across runs
// and the "version 1" arm would stop meaning anything.
const refName = () => `repo-token-${Date.now()}-${Math.floor(Math.random() * 10_000)}`;

/** openBindingDialog opens the register dialog and switches the credential control to the seal-a-new mode. */
async function openSealMode(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/repositories");
  await expect(page.getByTestId("panel-repository-bindings")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("binding-create-open").click();
  await expect(page.getByTestId("binding-create-dialog")).toBeVisible();
  // chooseOption, NOT selectOption. components/Picker.tsx renders components/ui/Select.tsx over
  // @base-ui/react — a button plus a `[role="listbox"]`, not a native <select> — so `selectOption` does not
  // address this control at all. provider-wiring.spec.ts:62 records the same correction; getting it wrong
  // here would have produced a RED that points at the wrong file, failing with "element is not a <select>"
  // on a page whose real defect is that the mode control does not exist.
  await chooseOption(page, "binding-connection-mode", "new");
}

test("an operator seals a credential and binds a private repository without leaving the page", async ({ page }) => {
  // EVERY request, so "the value went nowhere else" is a measurement over the whole conversation rather than
  // over the one call this test happens to look at.
  const requests: NetRequest[] = [];
  const responses: NetResponse[] = [];
  page.on("request", (r) => requests.push(r));
  page.on("response", (r) => responses.push(r));

  await openSealMode(page);

  const name = refName();
  const identity = `palai/private-${Date.now()}`;

  await page.getByTestId("binding-identity-input").fill(identity);
  // A REAL, REACHABLE, AUTHENTICATED REMOTE rather than a plausible-looking string, and `http` rather than
  // `https` deliberately. On the fake profile nothing clones and any http(s) URL would do; on the real
  // profile this spec's output is a binding an operator could actually run, and the live proof does exactly
  // that — it takes the binding THIS FORM created and starts a run against it. A fixture URL that cannot be
  // cloned would make the real-profile pass a weaker claim than it looks.
  await page.getByTestId("binding-clone-url-input").fill("http://127.0.0.1:8188/private-fixture.git");
  await page.getByTestId("binding-connection-name-input").fill(name);
  await page.getByTestId("binding-connection-token-input").fill(TOKEN);
  await page.getByTestId("binding-create-button").click();

  // THE STATUS NAMES THE REF AND NEVER THE VALUE.
  const status = page.getByTestId("repository-binding-status");
  await expect(status).toContainText(name, { timeout: 15_000 });
  await expect(status).not.toContainText(TOKEN);

  // THE BINDING CARRIES THE REF. The list is the operator's evidence that the two calls joined up.
  const row = page.locator("tr", { hasText: identity });
  await expect(row).toContainText(name, { timeout: 15_000 });

  // TWO CALLS, IN ORDER, AND THE SECOND ONE CARRIES NO CREDENTIAL. Sealing and naming are genuinely two
  // operations — the API has no field for a raw credential on a binding create (api/repository_bindings.go's
  // RepositoryBindingCreate has ConnectionRef and nothing else) — so the console must make both, and the
  // binding call must carry only the NAME.
  const writes = requests.filter((r) => r.method() === "POST" && r.url().includes("/api/palai/v1/"));
  const paths = writes.map((r) => new URL(r.url()).pathname.replace("/api/palai/v1", ""));
  expect(paths).toEqual(["/secret-refs", "/repository-bindings"]);

  const sealBody = writes[0].postData() ?? "";
  const bindBody = writes[1].postData() ?? "";
  expect(sealBody).toContain(TOKEN);
  expect(sealBody).toContain(name);
  expect(bindBody).toContain(name);
  // The load-bearing one: the binding create must never carry the bytes.
  expect(bindBody).not.toContain(TOKEN);

  // THE VALUE IS IN NO URL, EVER. Not in the seal call's query string, not in a navigation — which is what a
  // form that fell back to a native GET would produce.
  for (const r of requests) expect(r.url()).not.toContain(TOKEN);
  expect(page.url()).not.toContain(TOKEN);

  // AND IN NO RESPONSE BODY. The read side of secret_refs projects {name, version, updated_at}; this asserts
  // the projection rather than trusting it.
  //
  // THE COUNT IS ASSERTED BECAUSE A SWEEP OVER NOTHING CANNOT FAIL. If the filter below ever stops matching
  // — a relay path change, a spec that navigates before the responses land — this loop passes having read
  // zero bodies and reports the same green as one that read forty. The positive control is above:
  // `sealBody` DOES contain the token, so the needle is findable when it is genuinely there.
  const apiResponses = responses.filter((r) => r.url().includes("/api/palai/v1/"));
  expect(apiResponses.length, "no relay response was examined — this sweep would pass over an empty set").toBeGreaterThan(0);
  for (const resp of apiResponses) {
    const body = await resp.text().catch(() => "");
    expect(body).not.toContain(TOKEN);
  }

  // AND IN NO DOM NODE. The field cleared itself on submit; nothing copied the value into a status, a hidden
  // input or a React prop that landed in the flight payload.
  expect(await page.content()).not.toContain(TOKEN);
});

test("the credential field clears itself on a REFUSED submit, so a failure leaves no secret on screen", async ({ page }) => {
  await openSealMode(page);

  // A refusal the SERVER produces, on the field next to the credential: a `file:` clone URL is refused by
  // api/repository_bindings.go before the store is asked anything. So the binding call fails while the seal
  // has already happened — the exact retry the operator will hit.
  const name = refName();
  await page.getByTestId("binding-identity-input").fill("palai/refused");
  await page.getByTestId("binding-clone-url-input").fill("file:///etc/passwd");
  await page.getByTestId("binding-connection-name-input").fill(name);
  await page.getByTestId("binding-connection-token-input").fill(TOKEN);
  await page.getByTestId("binding-create-button").click();

  // THE REFUSAL IS ANNOUNCED IN TEXT, in a role="alert" region, without moving focus (ResourceForm rule 2).
  const error = page.getByTestId("repository-binding-error");
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toHaveAttribute("role", "alert");

  // AND IT SAYS THE CREDENTIAL SURVIVED. The secret ref was written before the binding was refused, so the
  // bytes are sealed under a name that now binds nothing — a bare "creation failed" would leave a live
  // credential the operator does not know exists. /registry already says this; so must this page.
  await expect(error).toContainText(name);

  // THE FIELD IS EMPTY. takeSecret() reads and clears in one call, on every submit, success or failure. The
  // alternative — clear only on success — leaves the token in a DOM node after a 400, on a screen the
  // operator may walk away from.
  await expect(page.getByTestId("binding-connection-token-input")).toHaveValue("");
  expect(await page.content()).not.toContain(TOKEN);
});

test("the credential field is on a form that POSTs, so an unhydrated submit cannot put the token in the URL", async ({ page }) => {
  await openSealMode(page);

  // THE ATTRIBUTE IS READ OFF THE RENDERED FORM THAT ACTUALLY CONTAINS THE CREDENTIAL INPUT, not off a form
  // this spec picked by name. A <form> with no method defaults to GET (HTML Living Standard), and every named
  // field then goes into the query string — which is how this tree once put an operator's password in the
  // address bar, the browser history and the access log.
  const form = page.locator("form").filter({ has: page.getByTestId("binding-connection-token-input") });
  await expect(form).toHaveCount(1);
  await expect(form).toHaveAttribute("method", /^post$/i);

  // The credential control is a password field a browser will not autofill from a saved login, and it is
  // programmatically labelled — the two properties SecretField exists to carry, asserted where it is USED
  // rather than only where it is defined.
  const input = page.getByTestId("binding-connection-token-input");
  await expect(input).toHaveAttribute("type", "password");
  await expect(input).toHaveAttribute("autocomplete", "new-password");
  const id = await input.getAttribute("id");
  await expect(page.locator(`label[for="${String(id)}"]`)).toHaveCount(1);
});

test("the register dialog is axe-clean with the credential controls RENDERED", async ({ page }) => {
  // THE SCAN RUNS WITH THE DIALOG OPEN AND THE SEAL MODE SELECTED, and that is the point of this test rather
  // than a detail of it. This console lost 144 controls from a contrast sweep because the sweep only ever ran
  // on closed routes: a form moved into a dialog left the evidence silently and the number got CLEANER as the
  // coverage shrank. The credential controls do not exist in the DOM until this mode is chosen, so a scan of
  // /repositories at rest cannot see them.
  await openSealMode(page);
  await expect(page.getByTestId("binding-connection-token-input")).toBeVisible();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.length} node(s)`)).toEqual([]);
});
