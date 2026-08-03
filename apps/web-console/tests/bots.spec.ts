import AxeBuilder from "@axe-core/playwright";
import { test, expect, type Request as NetRequest, type Response as NetResponse } from "@playwright/test";

import { WCAG_TAGS } from "./constants";
import { announceProfile, chooseOption, chooseOptionByLabel, resetFakeFixture, signIn, skipOnReal } from "./profile";

// THE BOT, CREATED FROM THE PANEL (2026-08-03 plan, Task 11).
//
// THIS FILE IS tests/slack-workspace.spec.ts CARRIED ACROSS, NOT REPLACED, and that matters because the
// four properties it pinned are four of this tree's recorded defect families: a credential field that
// survives a REFUSED submit, a credential on a form with no `method` (which once put an operator's
// password in the address bar, the browser history and the access log), an axe sweep that runs with the
// dialog CLOSED and therefore reports a clean bill of health for markup it never saw, and a registration
// that carries a raw token where a handle belongs. The screen underneath them changed — /integrations
// wrote `slack_connections`, /bots writes the kind-agnostic `integration_bots` registry — and the
// properties did not.
//
// ONE OF THEM IS STRICTLY HARDER TO KEEP HERE, AND THAT IS WHY IT IS ASSERTED ON THE WIRE. On the old
// surface the API could not be handed a credential: slackRegistrationBody declared no field for one, so
// DisallowUnknownFields made an inline token a 400 at the edge. POST /v1/bots CAN be: `config` is a
// `json.RawMessage` the control plane never decodes, which is exactly the opacity that lets a new channel
// arrive without a control-plane change. The fixture deliberately ACCEPTS a token inside config
// (tests/fake-control-plane.mjs says why at length) so that "no credential reached the registration" fails
// when the console sends one, rather than passing because the server refused it.
//
// THE SENTINELS ARE DISTINCTIVE AND THERE ARE THREE, one per credential, because a single shared needle
// cannot tell "the bot token went into the signing secret's slot" from "everything worked".
const SIGNING = "PALAI-BOT-SIGNING-SENTINEL-9d2a41c8e07b-DO-NOT-LEAK";
const BOT_TOKEN = "PALAI-BOT-TOKEN-SENTINEL-3e91b6d0a4f2-DO-NOT-LEAK";
const APP_TOKEN = "PALAI-BOT-APP-SENTINEL-71c5f8ba29e4-DO-NOT-LEAK";

test.beforeAll(() => announceProfile("bots.spec.ts"));
// This file CREATES bots, and a name is unique per project — so it mutates a collection two Playwright
// projects share (tests/profile.ts resetFakeFixture carries the measurement).
test.beforeAll(resetFakeFixture);
test.beforeEach(async ({ page }) => signIn(page));

// A bot's name is unique per PROJECT (migration 000061's UNIQUE (organization_id, project_id, name)), so a
// fixed name would pass once and 409 on every re-run of this file — and on the real profile, forever.
const botName = () => `probe-bot-${Date.now().toString(36)}-${Math.floor(Math.random() * 1000)}`;
const teamID = () => `T${Date.now().toString(36).toUpperCase()}${Math.floor(Math.random() * 1000)}`;

/** openCreateDialog opens the bot registration dialog. */
async function openCreateDialog(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/bots");
  await expect(page.getByTestId("panel-bots")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("bot-create-open").click();
  await expect(page.getByTestId("bot-create-dialog")).toBeVisible();
}

/** fillSlackBot fills every control a Slack bot needs, leaving the caller to submit. */
async function fillSlackBot(page: import("@playwright/test").Page, name: string, team: string): Promise<void> {
  // THE CHANNEL IS CHOSEN, NOT ASSUMED — this is the control the whole screen is organised around, and
  // driving it here means the dialog's default is never what these tests are actually proving.
  await chooseOption(page, "bot-channel-select", "slack");
  await page.getByTestId("bot-name-input").fill(name);
  await page.getByTestId("bot-team-input").fill(team);
  await page.getByTestId("bot-signing-secret-input").fill(SIGNING);
  await page.getByTestId("bot-token-input").fill(BOT_TOKEN);
  await page.getByTestId("bot-app-token-input").fill(APP_TOKEN);
}

test("an operator creates a bot from the panel, and its credentials reach the registry only as handles", async ({ page }) => {
  const requests: NetRequest[] = [];
  const responses: NetResponse[] = [];
  page.on("request", (r) => requests.push(r));
  page.on("response", (r) => responses.push(r));

  await openCreateDialog(page);
  const name = botName();
  const team = teamID();
  await fillSlackBot(page, name, team);
  await page.getByTestId("bot-create-button").click();

  // THE STATUS NAMES THE BOT AND NEVER A CREDENTIAL.
  const status = page.getByTestId("bot-create-status");
  await expect(status).toContainText(name, { timeout: 15_000 });
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect((await status.innerText()).includes(secret), "a credential is in the status line").toBe(false);
  }

  // THE ROW EXISTS. This is the operator's evidence that the seals and the registration joined up.
  await expect(page.locator("tr", { hasText: name })).toBeVisible({ timeout: 15_000 });

  // FOUR CALLS: THREE SEALS, THEN THE REGISTRATION. In that order, because a handle written into a bot's
  // config before its value is stored resolves to nothing, and every consumer of an unresolvable handle
  // fails SILENTLY by design — the exact bug up.go's slackSecretSlots table was written to close.
  const writes = requests
    .filter((r) => r.method() === "POST" && r.url().includes("/api/palai/v1/"))
    .map((r) => new URL(r.url()).pathname.replace("/api/palai/v1", ""));
  expect(writes).toEqual(["/secret-refs", "/secret-refs", "/secret-refs", "/bots"]);

  // THE REGISTRATION CARRIES HANDLES AND NOT ONE BYTE OF A CREDENTIAL — and unlike the surface this
  // replaced, nothing on the server would have stopped it. See the header.
  //
  // IT IS FOUND BY WHAT IT IS, not by being last: a successful create bumps the panel's reloadKey, so the
  // last request on the wire is the list refetch, which carries no post data at all.
  const registration = requests.filter((r) => r.method() === "POST" && r.url().endsWith("/api/palai/v1/bots")).at(-1);
  expect(registration, "no registration POST was observed").toBeDefined();
  const body = registration?.postData() ?? "";
  expect(body, "the registration carried no body").not.toBe("");
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect(body.includes(secret), "the registration carried a raw credential").toBe(false);
  }
  // AND IT NAMES THE HANDLES up.go WOULD HAVE NAMED. Same team, same three prefixes: a bot configured from
  // this panel and a workspace configured by the CLI resolve to the same three secret names, which is the
  // pairing failure lib/channels.ts exists to keep closed.
  expect(body).toContain(`slack-signing-${team}`);
  expect(body).toContain(`slack-bot-${team}`);
  expect(body).toContain(`slack-app-${team}`);
  // THE KIND IS THE CHANNEL'S ID AND THE CHANNEL'S SETTINGS ARE INSIDE `config`, which is what makes the
  // control plane able to store this row without knowing what Slack is.
  const sent = JSON.parse(body) as { kind?: string; config?: Record<string, unknown> };
  expect(sent.kind).toBe("slack");
  expect(sent.config?.team_id).toBe(team);

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

test("the channel list shows what is coming as DISABLED, and does not hide it", async ({ page }) => {
  await openCreateDialog(page);

  // The list opens as a listbox — the channel is a value chosen from a list, which is what makes adding
  // WhatsApp a row in lib/channels.ts rather than a branch on this screen.
  await page.getByTestId("bot-channel-select").click();
  const listbox = page.getByRole("listbox");
  await expect(listbox).toBeVisible();

  await expect(listbox.getByRole("option", { name: "Slack" })).toBeEnabled();
  // A channel with no adapter is VISIBLE and unselectable — a roadmap, not a lie and not a hidden thing.
  for (const soon of ["WhatsApp", "Telegram"]) {
    const option = listbox.getByRole("option", { name: soon });
    await expect(option, `${soon} is not offered at all — the roadmap is hidden rather than disabled`).toHaveCount(1);
    await expect(option).toBeDisabled();
  }
});

test("the credential fields clear themselves on a REFUSED submit", async ({ page }) => {
  await openCreateDialog(page);

  // A refusal the SERVER produces, and one that is reachable on BOTH profiles because this test creates
  // the collision itself: a bot's name is unique per project (migration 000061), so submitting the same
  // name twice is the retry an operator meets — and by then the three seals have already happened, which
  // is the whole reason this arm exists.
  const name = botName();
  await fillSlackBot(page, name, teamID());
  await page.getByTestId("bot-create-button").click();
  await expect(page.getByTestId("bot-create-status")).toContainText(name, { timeout: 15_000 });

  const team = teamID();
  await openCreateDialog(page);
  await fillSlackBot(page, name, team);
  await page.getByTestId("bot-create-button").click();

  const error = page.getByTestId("bot-create-error");
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toHaveAttribute("role", "alert");
  // AND IT SAYS THE CREDENTIALS SURVIVED, by name. The seals happened before the registration was refused,
  // so three values are sealed under handles that now bind nothing — a bare "the bot could not be created"
  // would leave live credentials the operator does not know exist.
  await expect(error).toContainText(`slack-signing-${team}`);

  // EVERY credential field is empty. takeSecret() reads and clears in one call, on every submit, success or
  // failure — three fields, three clears, and a test that checked only the first would miss two.
  for (const id of ["bot-signing-secret-input", "bot-token-input", "bot-app-token-input"]) {
    await expect(page.getByTestId(id)).toHaveValue("");
  }
  const content = await page.content();
  for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
    expect(content.includes(secret), "a credential survived on screen after a refusal").toBe(false);
  }
});

test("the credentials are on a form that POSTs, so an unhydrated submit cannot put them in the URL", async ({ page }) => {
  await openCreateDialog(page);

  // Read off the rendered form that actually CONTAINS the credential inputs. A <form> with no method
  // defaults to GET, and every named field then goes into the query string — which is how this tree once put
  // an operator's password in the address bar, the browser history and the access log.
  const form = page.locator("form").filter({ has: page.getByTestId("bot-signing-secret-input") });
  await expect(form).toHaveCount(1);
  await expect(form).toHaveAttribute("method", /^post$/i);

  // All three are password fields a browser will not autofill from a saved login, and each is
  // programmatically labelled — asserted where they are USED, not only where SecretField is defined.
  for (const id of ["bot-signing-secret-input", "bot-token-input", "bot-app-token-input"]) {
    const input = page.getByTestId(id);
    await expect(input).toHaveAttribute("type", "password");
    await expect(input).toHaveAttribute("autocomplete", "new-password");
    const domID = await input.getAttribute("id");
    await expect(page.locator(`label[for="${String(domID)}"]`)).toHaveCount(1);
  }
});

test("the create dialog is axe-clean with the credential controls RENDERED", async ({ page }) => {
  await openCreateDialog(page);
  // The credential fields belong to the CHOSEN channel, so a scan that ran before one was chosen would
  // report a clean bill of health for markup it never saw — the shape recorded on 2026-08-01 when five
  // dialogs took 144 controls out of a sweep and it reported a cleaner number while covering less.
  await expect(page.getByTestId("bot-signing-secret-input")).toBeVisible();

  const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.length} node(s)`)).toEqual([]);
});

test("choosing an agent pins the bot to that agent's newest PUBLISHED revision", async ({ page }) => {
  // Needs an agent with a published revision to pin the bot to; a bootstrap stack has none.
  skipOnReal("DIV-UI-010");
  const requests: NetRequest[] = [];
  page.on("request", (r) => requests.push(r));

  await openCreateDialog(page);
  const name = botName();
  await fillSlackBot(page, name, teamID());
  // BY NAME, because what an operator picks is an AGENT and what the registry stores is a REVISION — the
  // resolution in between is the property, and addressing the row by its id would skip past it.
  //
  // seeded-agent-02, NOT -01, AND THE CHOICE IS THE WHOLE TEST. agt_01's published revision is also its
  // NEWEST, so a console that pinned "the newest" would send the same id and this assertion would pass over
  // a bug it names — the shape this tree records as an assertion that cannot fail for its stated reason.
  // agt_02's newest is a DRAFT (agrev_4) over a published agrev_3, so only the rule distinguishes them.
  await chooseOptionByLabel(page, "bot-agent-select", "seeded-agent-02");
  await page.getByTestId("bot-create-button").click();
  await expect(page.getByTestId("bot-create-status")).toContainText(name, { timeout: 15_000 });

  const registration = requests.filter((r) => r.method() === "POST" && r.url().endsWith("/api/palai/v1/bots")).at(-1);
  const sent = JSON.parse(registration?.postData() ?? "{}") as { agent_revision_id?: string };
  expect(sent.agent_revision_id, "the bot was pinned to the newest revision rather than the published one").toBe("agrev_3");
});

test("an agent with no published revision is REFUSED rather than pinned to nothing", async ({ page }) => {
  // Needs an agent whose lineage is known to hold no published revision; a bootstrap stack has no agents.
  skipOnReal("DIV-UI-010");
  const requests: NetRequest[] = [];
  page.on("request", (r) => requests.push(r));

  await openCreateDialog(page);
  await fillSlackBot(page, botName(), teamID());
  // seeded-agent-03 has no revisions at all, which is what an agent an operator has just created looks
  // like. Pinning a bot to it would be pinning it to nothing.
  await chooseOptionByLabel(page, "bot-agent-select", "seeded-agent-03");
  await page.getByTestId("bot-create-button").click();

  const error = page.getByTestId("bot-create-error");
  await expect(error).toBeVisible({ timeout: 15_000 });
  await expect(error).toContainText("PUBLISHED");

  // AND NOTHING WAS SENT — not the registration, and not a seal. The refusal happens before the credentials
  // are sealed, so an operator who fixes the agent and submits again is not leaving three orphaned handles
  // behind them; "Nothing was sent" is a sentence this arm has to be able to make true.
  const writes = requests.filter((r) => r.method() === "POST" && r.url().includes("/api/palai/v1/"));
  expect(writes.map((r) => new URL(r.url()).pathname), "the console wrote something before refusing").toEqual([]);
});
