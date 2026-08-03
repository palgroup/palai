import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Request as NetRequest, type Response as NetResponse } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, chooseOptionByLabel, signIn, skipOnReal } from "./profile";

// THE SLACK WORKSPACE CREDENTIAL, PROVISIONED FROM THE PANEL.
//
// THE GAP, MEASURED RATHER THAN ASSERTED (2026-08-03):
//
//   grep -rn 'slack-connections' apps/web-console/app apps/web-console/lib  ->  0 hits
//
// Zero. The console had nineteen routes and not one of them could register a Slack workspace. The ONLY way
// to get one was `palai up` reading four values out of .env.local — SLACK_TEAM_ID, SLACK_SIGNING_SECRET,
// SLACK_BOT_TOKEN, SLACK_APP_TOKEN — sealing three of them into the platform's secret store and POSTing the
// handles. A fresh machine could be given a Slack workspace by exactly one method: a human editing a file.
//
// That is the thing the owner has said three times they do not want, and this spec is the proof it is over.
//
// WHAT THE API TAKES, AND WHY THE FORM HAS THE SHAPE IT HAS. api/slack_connections.go's
// slackRegistrationBody has NO signing_secret, bot_token or app_token field at all — DisallowUnknownFields
// turns an inline value into a 400 at the boundary, deliberately, so that "the edge cannot be handed a raw
// credential" is structural rather than remembered. Every credential is a *_ref HANDLE. So this form is the
// same two-phase shape /registry and /repositories already use: seal the values, then name them.
//
// THE HANDLE NAMES ARE up.go's, NOT THIS PAGE'S, and that pairing is the whole point of naming them off one
// table. cmd/cli/internal/stack/up.go's slackSecretSlots derives the registered handle and the stored
// secret's name from the same row, because the failure it closes is a PAIRING failure: `palai up` once
// registered `signing_secret_ref: slack-signing-<team>` while nothing stored a value under that name, and
// every consumer of an unresolvable handle fails SILENTLY by design. A console that invented its own naming
// would re-open exactly that hole from the other side — a workspace registered from the panel and a
// workspace registered by the CLI would resolve differently.
//
// THE SENTINELS ARE DISTINCTIVE ON PURPOSE and there are THREE, one per credential, because a single shared
// needle cannot tell "the bot token went into the signing secret's slot" from "everything worked".
const SIGNING = "PALAI-SLACK-SIGNING-SENTINEL-9d2a41c8e07b-DO-NOT-LEAK";
const BOT_TOKEN = "PALAI-SLACK-BOT-SENTINEL-3e91b6d0a4f2-DO-NOT-LEAK";
const APP_TOKEN = "PALAI-SLACK-APP-SENTINEL-71c5f8ba29e4-DO-NOT-LEAK";

test.beforeAll(() => announceProfile("slack-workspace.spec.ts"));
test.beforeEach(async ({ page }) => signIn(page));

// A Slack team id is unique per DEPLOYMENT — the store refuses a second registration of the same workspace
// (ErrSlackWorkspaceBoundElsewhere, the E19 T1 cross-tenant hijack fix) — so a fixed id would pass once and
// 409 on every re-run of this file.
const teamID = () => `T${Date.now().toString(36).toUpperCase()}${Math.floor(Math.random() * 1000)}`;

/** openConnectDialog opens the workspace registration dialog. */
async function openConnectDialog(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/integrations");
  await expect(page.getByTestId("panel-slack-connections")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("slack-connect-open").click();
  await expect(page.getByTestId("slack-connect-dialog")).toBeVisible();
}

/**
 * chooseFirstOption opens a ui/Select and takes its first row, asserting there IS one.
 *
 * It addresses by POSITION rather than by a fixture id on purpose. The principal picker is fed by
 * GET /v1/api-keys, whose rows are minted per deployment — `key_admin` on the fixture, `key_local` and
 * fifty-one others on the live stack — so a spec naming one would be pinning a fixture artefact and would
 * fail on a real stack for a reason that has nothing to do with this form. What the form must do is offer
 * what the deployment holds; which row an operator takes is not the property.
 *
 * The count assertion is the load-bearing half: without it this helper would silently do nothing on an
 * empty listbox and the submit would fail somewhere else entirely.
 *
 * IT IS SCOPED TO THE LISTBOX AND IT SKIPS THE PLACEHOLDER, and both halves were learned the hard way. The
 * first draft used a bare `page.locator('[role="option"]')` and took `.first()`, which matched the
 * PLACEHOLDER row components/ui/Select.tsx renders for a field with a `placeholder` — `data-value=""`, and
 * hidden. The failure said "the listbox offered nothing to choose" on a listbox that was offering exactly
 * what it should, which is an assertion pointing at the wrong file: it reads as "the console rendered no
 * principals" when the console had rendered one and a placeholder. tests/profile.ts's own helpers scope to
 * `getByRole("listbox")` for this reason, so this one does too.
 */
async function chooseFirstOption(page: import("@playwright/test").Page, testId: string): Promise<string> {
  const trigger = page.getByTestId(testId);
  await expect(trigger, `no select carries data-testid="${testId}"`).toBeVisible({ timeout: 15_000 });
  await trigger.click();
  const listbox = page.getByRole("listbox");
  await expect(listbox, `the ${testId} listbox did not open`).toBeVisible();
  // `[data-value]:not([data-value=""])` is the REAL choices: the placeholder carries an empty value, which is
  // how ui/Select spells "nothing chosen" and is not something an operator can pick.
  const options = listbox.locator('[role="option"][data-value]:not([data-value=""])');
  await expect(options, `the ${testId} listbox offered nothing to choose`).not.toHaveCount(0, { timeout: 10_000 });
  const value = await options.first().getAttribute("data-value");
  await options.first().click();
  return String(value);
}

/** fillRegistration fills every control the form requires, leaving the caller to submit. */
async function fillRegistration(page: import("@playwright/test").Page, team: string): Promise<void> {
  await page.getByTestId("slack-team-input").fill(team);
  await page.getByTestId("slack-signing-secret-input").fill(SIGNING);
  await page.getByTestId("slack-bot-token-input").fill(BOT_TOKEN);
  await page.getByTestId("slack-app-token-input").fill(APP_TOKEN);
  // BOTH RUN-TARGET HALVES ARE PICKED FROM WHAT THE DEPLOYMENT HOLDS, never typed. vetSlackDefaultPolicy
  // requires agent_revision_id AND principal_id — "a binding that has not been told what to run, or as whom,
  // admits nothing" — and a free-text box for either is a field whose typo surfaces as a workspace that
  // silently admits nothing.
  //
  // THE AGENT IS NAMED AND THE PRINCIPAL IS NOT, and the asymmetry is real rather than sloppy: this form
  // resolves the chosen agent's newest PUBLISHED revision and refuses an agent that has none, so the row
  // taken here has to be one that has one. `seeded-agent-01` (agt_01) is the fixture's only such agent —
  // which is why the two tests that submit this form are skipped on the real profile, see DIV-UI-010.
  await chooseOptionByLabel(page, "slack-agent-select", "seeded-agent-01");
  await chooseFirstOption(page, "slack-principal-select");
}

test("an operator connects a Slack workspace from the panel, with no file on any machine", async ({ page }) => {
  // Needs an agent with a published revision to pin the workspace to; a bootstrap stack has none.
  skipOnReal("DIV-UI-010");
  const requests: NetRequest[] = [];
  const responses: NetResponse[] = [];
  page.on("request", (r) => requests.push(r));
  page.on("response", (r) => responses.push(r));

  await openConnectDialog(page);
  const team = teamID();
  await fillRegistration(page, team);
  await page.getByTestId("slack-connect-button").click();

  // THE STATUS NAMES THE WORKSPACE AND NEVER A CREDENTIAL.
  const status = page.getByTestId("slack-connect-status");
  await expect(status).toContainText(team, { timeout: 15_000 });
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect((await status.innerText()).includes(secret), "a credential is in the status line").toBe(false);
  }

  // THE ROW EXISTS. This is the operator's evidence that the seals and the registration joined up.
  await expect(page.locator("tr", { hasText: team })).toBeVisible({ timeout: 15_000 });

  // FOUR CALLS: THREE SEALS, THEN THE REGISTRATION. In that order, because a handle registered before its
  // value is stored resolves to nothing and every consumer of it fails silently — which is the exact bug
  // up.go's slackSecretSlots table was written to close.
  const writes = requests
    .filter((r) => r.method() === "POST" && r.url().includes("/api/palai/v1/"))
    .map((r) => new URL(r.url()).pathname.replace("/api/palai/v1", ""));
  expect(writes).toEqual(["/secret-refs", "/secret-refs", "/secret-refs", "/slack-connections"]);

  // THE REGISTRATION CARRIES HANDLES AND NOT ONE BYTE OF A CREDENTIAL. The API would refuse an inline value
  // with a 400, so this is asserting the console never even attempts it — a 400 an operator has to decode is
  // a worse outcome than a console that cannot produce one.
  // THE REGISTRATION IS FOUND BY WHAT IT IS, not by being last. The first draft took
  // `requests[requests.length - 1]` and got an empty body, because a SUCCESSFUL create bumps the panel's
  // reloadKey and the last request on the wire is therefore the list refetch — a GET, with no post data. The
  // assertion then failed with `Expected "slack-signing-…", Received ""`, which reads as "the console sent
  // the wrong handles" when the console had sent the right ones to a call this line was not looking at.
  const registration = requests.filter((r) => r.method() === "POST" && r.url().endsWith("/api/palai/v1/slack-connections")).at(-1);
  expect(registration, "no registration POST was observed").toBeDefined();
  const body = registration?.postData() ?? "";
  expect(body, "the registration carried no body").not.toBe("");
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect(body.includes(secret), "the registration carried a raw credential").toBe(false);
  }
  // AND IT NAMES THE HANDLES up.go WOULD HAVE NAMED. Same team, same three prefixes: a workspace registered
  // from this panel and one registered by the CLI resolve to the same three secret names.
  expect(body).toContain(`slack-signing-${team}`);
  expect(body).toContain(`slack-bot-${team}`);
  expect(body).toContain(`slack-app-${team}`);

  // THE POSITIVE CONTROL: the seals DID carry the values, so the sweep above can fail when it should.
  const seals = requests.filter((r) => r.method() === "POST" && r.url().endsWith("/api/palai/v1/secret-refs"));
  expect(seals).toHaveLength(3);
  const sealed = seals.map((r) => r.postData() ?? "").join("\n");
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect(sealed.includes(secret), "a credential never reached the seal call").toBe(true);
  }

  // NO CREDENTIAL IN ANY URL, ANY RESPONSE BODY, OR ANY DOM NODE.
  for (const r of requests) {
    for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
      expect(r.url().includes(secret), "a credential is in a URL").toBe(false);
    }
  }
  const apiResponses = responses.filter((r) => r.url().includes("/api/palai/v1/"));
  expect(apiResponses.length, "no relay response was examined — this sweep would be vacuous").toBeGreaterThan(0);
  for (const resp of apiResponses) {
    const text = await resp.text().catch(() => "");
    for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
      expect(text.includes(secret), `a credential came back from ${resp.url()}`).toBe(false);
    }
  }
  const content = await page.content();
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect(content.includes(secret), "a credential is in the DOM").toBe(false);
  }
});

test("the credential fields clear themselves on a REFUSED submit", async ({ page }) => {
  skipOnReal("DIV-UI-010");
  await openConnectDialog(page);

  // A refusal the SERVER produces: a team id the store has already bound. The fixture seeds one, so this is
  // the conflict an operator meets when they register the same workspace twice — and by then the three
  // seals have already happened, which is the retry this arm exists for.
  await fillRegistration(page, "T-FIXTURE-BOUND");
  await page.getByTestId("slack-connect-button").click();

  const error = page.getByTestId("slack-connect-error");
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toHaveAttribute("role", "alert");
  // AND IT SAYS THE CREDENTIALS SURVIVED, by name. The seals happened before the registration was refused,
  // so three values are sealed under handles that now bind nothing — a bare "registration failed" would
  // leave live credentials the operator does not know exist. This is /registry's and /repositories'
  // sentence, on the surface that now has the same shape.
  await expect(error).toContainText("slack-signing-T-FIXTURE-BOUND");

  // EVERY credential field is empty. takeSecret() reads and clears in one call, on every submit, success or
  // failure — three fields, three clears, and a test that checked only the first would miss two.
  for (const id of ["slack-signing-secret-input", "slack-bot-token-input", "slack-app-token-input"]) {
    await expect(page.getByTestId(id)).toHaveValue("");
  }
  const content = await page.content();
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect(content.includes(secret), "a credential survived on screen after a refusal").toBe(false);
  }
});

test("the credentials are on a form that POSTs, so an unhydrated submit cannot put them in the URL", async ({ page }) => {
  await openConnectDialog(page);

  // Read off the rendered form that actually CONTAINS the credential inputs. A <form> with no method
  // defaults to GET, and every named field then goes into the query string — which is how this tree once put
  // an operator's password in the address bar, the browser history and the access log.
  const form = page.locator("form").filter({ has: page.getByTestId("slack-signing-secret-input") });
  await expect(form).toHaveCount(1);
  await expect(form).toHaveAttribute("method", /^post$/i);

  // All three are password fields a browser will not autofill from a saved login, and each is
  // programmatically labelled — asserted where they are USED, not only where SecretField is defined.
  for (const id of ["slack-signing-secret-input", "slack-bot-token-input", "slack-app-token-input"]) {
    const input = page.getByTestId(id);
    await expect(input).toHaveAttribute("type", "password");
    await expect(input).toHaveAttribute("autocomplete", "new-password");
    const domID = await input.getAttribute("id");
    await expect(page.locator(`label[for="${String(domID)}"]`)).toHaveCount(1);
  }
});

test("the connect dialog is axe-clean with the credential controls RENDERED", async ({ page }) => {
  await openConnectDialog(page);
  await expect(page.getByTestId("slack-signing-secret-input")).toBeVisible();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.length} node(s)`)).toEqual([]);
});
