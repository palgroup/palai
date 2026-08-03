import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { expect, test, type Page, type Request as NetRequest } from "@playwright/test";

import { buildManifest, MANIFEST_LIMITS, SLACK_BOT_EVENTS, SLACK_BOT_SCOPES, SLACK_MANIFEST_DEFAULTS } from "../lib/botManifest";
import { CHANNELS } from "../lib/channels";
import { NEXT_PORT } from "./constants";
import { announceProfile, resetFakeFixture, sessionHeaders, signIn } from "./profile";

// THE MANIFEST WIZARD (2026-08-03 plan, Task 12) — the middle of the operator's own flow: choose a channel,
// receive a manifest, go and install it, come back and paste the tokens.
//
// THE FIRST HALF OF THIS FILE RUNS NO BROWSER. It re-derives what the generator claims about the SHIPPED
// manifest from `deploy/slack/app-manifest.yaml` itself, on every run, because lib/botManifest.ts carries a
// COPY of that file's fixed half — a browser bundle cannot read the repository — and a copy nothing checks is
// a copy that drifts. Adding a scope to the shipped file and forgetting the generator is a red test here,
// which is the only thing that makes "the generated manifest preserves the shipped scope set" a fact rather
// than an intention.
//
// THE SECOND HALF DRIVES THE SCREEN, and the property that matters most is asserted on the WIRE. `POST
// /v1/bots` and `PATCH /v1/bots/{id}` both take `config` as a document the control plane never decodes, so
// nothing at the edge can refuse an inline credential the way POST /v1/slack-connections could — its body
// declared no field for one, so DisallowUnknownFields made it a 400. The console's seal-then-name flow is the
// only boundary left on this surface, and a test that trusted the code path would be asserting a comment.

test.beforeAll(() => announceProfile("bot-manifest.spec.ts"));

// --- THE SHIPPED FILE, PARSED ENOUGH TO ARGUE WITH -----------------------------------------------------
//
// Not with a YAML library: this console depends on none, and adding a package so a test can read four blocks
// would be a dependency for a file that already has a parser pointed at it — adapters/integrations/slack/
// manifest_test.go unmarshals it on every Go run and fails if it is not valid YAML. What is needed here is
// narrower: the ITEMS under two named keys and two literal values. Every extractor below asserts it found
// something, so a parse that silently returns nothing fails instead of passing over an empty list.

function shippedManifest(): string {
  // process.cwd() is apps/web-console under `playwright test` — the same assumption tests/constants.ts makes
  // when it runs scripts/hash-password.mjs and walks components/.
  const path = resolve(process.cwd(), "..", "..", "deploy", "slack", "app-manifest.yaml");
  const text = readFileSync(path, "utf8");
  expect(text.length, `${path} is empty — every assertion below would be vacuous`).toBeGreaterThan(1000);
  return text;
}

/**
 * stripComment removes a trailing comment — and it cuts at ` #`, WITH the space, which is the whole of the
 * difference between this working and not.
 *
 * The first draft cut at the first `#` anywhere in the line and turned `background_color: "#1f2933"` into
 * `background_color: "`, so the arm that asserts the shipped file still sets that colour failed while the
 * file was perfectly correct. That is this repository's most-repeated test defect in miniature: an assertion
 * that fails for a reason unrelated to what it claims. YAML's own rule is the same one — a `#` begins a
 * comment only when it follows a space or starts the line — so this is the file's grammar rather than a
 * patch for one line.
 */
const stripComment = (line: string): string => (line.includes(" #") ? line.slice(0, line.indexOf(" #")) : line).trimEnd();

/**
 * blockItems reads the `- item` values under a key, skipping the CONTINUATION COMMENT lines between them —
 * the shipped file writes several lines of reasoning after a scope, indented past it, and a reader that
 * stopped at the first non-item line would find one scope and pass.
 */
function blockItems(source: string, key: string): string[] {
  const lines = source.split("\n");
  const start = lines.findIndex((l) => l.trim() === key);
  expect(start, `the shipped manifest has no \`${key}\` line`).toBeGreaterThan(-1);
  const items: string[] = [];
  for (const line of lines.slice(start + 1)) {
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;
    if (!trimmed.startsWith("- ")) break;
    items.push(stripComment(trimmed.slice(2)).trim());
  }
  expect(items.length, `nothing was parsed under \`${key}\` — this comparison would be vacuous`).toBeGreaterThan(0);
  return items;
}

/** foldedValue reads a `key: >-` block: the lines under it joined with a space, which is what a fold means. */
function foldedValue(source: string, key: string): string {
  const lines = source.split("\n");
  const start = lines.findIndex((l) => l.trim() === `${key}: >-`);
  expect(start, `the shipped manifest has no folded \`${key}\``).toBeGreaterThan(-1);
  const indent = lines[start].length - lines[start].trimStart().length;
  const parts: string[] = [];
  for (const line of lines.slice(start + 1)) {
    if (line.trim() === "" || line.trim().startsWith("#")) break;
    if (line.length - line.trimStart().length <= indent) break;
    parts.push(line.trim());
  }
  expect(parts.length, `nothing was parsed under \`${key}\``).toBeGreaterThan(0);
  return parts.join(" ");
}

/** quotedValues reads every `key: "…"` value, wherever it is indented and whether or not it opens an item. */
function quotedValues(source: string, key: string): string[] {
  return source
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.startsWith(`${key}: "`) || l.startsWith(`- ${key}: "`))
    .map((l) => l.slice(l.indexOf('"') + 1, l.lastIndexOf('"')));
}

/** hasSetting is true when some line, comment stripped, is exactly this `key: value`. */
const hasSetting = (source: string, setting: string): boolean => source.split("\n").some((l) => stripComment(l).trim() === setting);

const BENIGN = {
  name: "iOS Bot",
  displayName: "ios-bot",
  description: "Runs the iOS work where it is asked for.",
  prompts: [{ title: "What can you do?", message: "What can you do here?" }],
};

test("the generated manifest carries the SHIPPED template's scopes, events and settings", () => {
  const shipped = shippedManifest();

  // THE NINE SCOPES AND THE FIVE EVENTS, re-derived from the file rather than retyped. Each scope is claimed
  // by a UAT case and each event has a verdict in adapters/integrations/slack/manifest_test.go; a generator
  // that quietly dropped one would produce an app whose events Slack simply never delivers.
  expect(blockItems(shipped, "bot:"), "the generator's scope list has drifted from deploy/slack/app-manifest.yaml").toEqual([...SLACK_BOT_SCOPES]);
  expect(blockItems(shipped, "bot_events:"), "the generator's event list has drifted from deploy/slack/app-manifest.yaml").toEqual([...SLACK_BOT_EVENTS]);

  const draft = buildManifest(BENIGN);
  expect(draft.refusals).toEqual([]);
  for (const scope of SLACK_BOT_SCOPES) expect(draft.yaml).toContain(`      - ${scope}`);
  for (const event of SLACK_BOT_EVENTS) expect(draft.yaml).toContain(`      - ${event}`);

  // THE FIXED SETTINGS, ASSERTED IN BOTH DOCUMENTS so this arm cannot pass by agreeing with itself. Socket
  // Mode is what makes the app installable with no public URL; the messages-tab pair was found live, after a
  // panel rendered and refused to accept typing; agent_view is a door Slack's own reference says cannot be
  // walked back through.
  for (const setting of [
    "socket_mode_enabled: true",
    "is_enabled: true",
    "org_deploy_enabled: false",
    "token_rotation_enabled: false",
    "home_tab_enabled: false",
    "messages_tab_enabled: true",
    "messages_tab_read_only_enabled: false",
    "always_online: false",
    "major_version: 1",
    "minor_version: 2",
    'background_color: "#1f2933"',
  ]) {
    expect(hasSetting(shipped, setting), `the shipped manifest no longer sets ${setting}`).toBe(true);
    expect(hasSetting(draft.yaml, setting), `the generated manifest does not set ${setting}`).toBe(true);
  }
  expect(draft.yaml).toContain("  agent_view:");
  expect(draft.yaml.includes("assistant_view"), "assistant_view is the LEGACY surface and the switch to agent_view cannot be reversed").toBe(false);

  // AND THE TWO DEFAULTS AN OPERATOR STARTS FROM ARE THE SHIPPED ONES: the wizard's first draft is the
  // manifest this deployment already runs on, with the names changed.
  expect(foldedValue(shipped, "agent_description")).toBe(SLACK_MANIFEST_DEFAULTS.description);
  expect(quotedValues(shipped, "title")).toEqual(SLACK_MANIFEST_DEFAULTS.prompts.map((p) => p.title));
  expect(quotedValues(shipped, "message")).toEqual(SLACK_MANIFEST_DEFAULTS.prompts.map((p) => p.message));
});

test("a description past the published cap is REFUSED, never truncated", () => {
  const at = buildManifest({ ...BENIGN, description: "x".repeat(MANIFEST_LIMITS.description) });
  expect(at.refusals, "the cap itself is a legal value — 300 characters is documented as the MAXIMUM").toEqual([]);
  expect(at.yaml).toContain("x".repeat(MANIFEST_LIMITS.description));

  const over = buildManifest({ ...BENIGN, description: "x".repeat(MANIFEST_LIMITS.description + 12) });
  // NO DOCUMENT AT ALL. A generator that truncated would hand over a manifest whose most visible sentence is
  // not the one the operator wrote, and they would find that out in front of their workspace.
  expect(over.yaml).toBe("");
  expect(over.refusals).toHaveLength(1);
  expect(over.refusals[0]).toContain("shorten it by 12");
});

test("every required field of the manifest is refused when empty, and the reason says what it is for", () => {
  expect(buildManifest({ ...BENIGN, name: "  " }).refusals.join(" ")).toContain("display_information.name");
  expect(buildManifest({ ...BENIGN, displayName: "" }).refusals.join(" ")).toContain("display name");
  // agent_description is REQUIRED whenever agent_view is declared, and this manifest always declares one.
  expect(buildManifest({ ...BENIGN, description: "" }).refusals.join(" ")).toContain("required");
  // A prompt is a PAIR: half of one is a button that says nothing, or one that sends nothing.
  expect(buildManifest({ ...BENIGN, prompts: [{ title: "Ask", message: "" }] }).refusals.join(" ")).toContain("half-written");
  for (const half of [{ name: "  " }, { displayName: "" }, { description: "" }]) {
    expect(buildManifest({ ...BENIGN, ...half }).yaml, "a refused manifest must offer NO document").toBe("");
  }
});

test("nothing an operator types can add a line to the document", () => {
  // THE ONE WAY THIS GENERATOR COULD EMIT SOMETHING SLACK REFUSES is a value that escapes its scalar. A
  // newline in a plain scalar does not produce an invalid document — it produces a VALID one carrying a key
  // nobody wrote — so the assertion is structural: the same document, line for line, whatever is typed.
  const benign = buildManifest(BENIGN);
  const hostile = buildManifest({
    // Every value stays inside its own cap — this arm is about ESCAPING, and a refusal here would make it
    // assert nothing at all. The injected key goes in the description, which has room for it.
    name: 'a"b\nx: y',
    displayName: "c: d #e\\f\tg",
    description: "line one\nsettings:\n  socket_mode_enabled: false\r\n  - not an item",
    prompts: [{ title: '- "x"', message: "y: z " }],
  });
  expect(hostile.refusals).toEqual([]);
  expect(hostile.yaml.split("\n").length, "an operator's text opened a new line in the document").toBe(benign.yaml.split("\n").length);
  // And the injected key never becomes one: socket_mode_enabled appears once, at its own indent, saying true.
  expect(hostile.yaml.split("\n").filter((l) => l.trim().startsWith("socket_mode_enabled:"))).toEqual(["  socket_mode_enabled: true"]);
  expect(hostile.yaml).toContain('display_name: "c: d #e\\\\f\\tg"');
});

test("a channel with no adapter offers no manifest and no form", () => {
  // The roadmap rows are IN the list deliberately (lib/channels.ts), and a manifest for an adapter nobody has
  // written would be a document that configures nothing — the same mistake as inventing credential fields.
  for (const channel of CHANNELS) {
    if (channel.enabled) continue;
    expect(channel.manifest, `${channel.label} is not connectable and offers a manifest`).toBeUndefined();
    expect(channel.form, `${channel.label} is not connectable and offers a form`).toBeUndefined();
    expect(channel.note, `${channel.label} is not connectable and does not say why`).toBeTruthy();
  }
  // And at least one channel DOES declare one, or every assertion above is about an empty list.
  expect(CHANNELS.filter((c) => c.manifest !== undefined).length).toBeGreaterThan(0);
});

// --- THE SCREEN ---------------------------------------------------------------------------------------

// THREE DISTINCTIVE SENTINELS, one per credential, because a single shared needle cannot tell "the bot token
// went into the signing secret's slot" from "everything worked".
const SIGNING = "PALAI-DETAIL-SIGNING-SENTINEL-4f7a12c9e83b-DO-NOT-LEAK";
const BOT_TOKEN = "PALAI-DETAIL-TOKEN-SENTINEL-b61d09e4a7f3-DO-NOT-LEAK";
const APP_TOKEN = "PALAI-DETAIL-APP-SENTINEL-2c85fa30d9e6-DO-NOT-LEAK";

// A KEY NO CHANNEL IN lib/channels.ts DECLARES, planted in every probe bot's `config`.
//
// `config` is opaque BY DESIGN and other writers use that: measured on the live stack on 2026-08-03, the one
// real bot row's document is five keys this console has never heard of. `PATCH /v1/bots/{id}` ASSIGNS the
// document (storage/queries/bots.sql: `config = COALESCE($8, config)`), so a console that sent only what it
// understands would DELETE the rest — silently, while telling the operator it saved.
const OPAQUE_KEY = "palai_console_probe";
const OPAQUE_VALUE = { "weird key": [1, 2, { deep: true }] };

test.describe("the bot's own page", () => {
  test.beforeAll(resetFakeFixture);
  test.beforeEach(async ({ page }) => signIn(page));

  /**
   * probeBot registers a bot through the console's OWN relay and returns its name.
   *
   * IT CREATES RATHER THAN SAMPLING, and that is what lets every test below run on both profiles: a bootstrap
   * stack holds no bots at all, so a spec that opened "the first row" would fail on the real profile for a
   * reason unrelated to what it asserts. The row it makes also carries a key this console does not
   * understand, which is what the preservation arm needs and what no fixture could honestly seed for the
   * real side.
   */
  async function probeBot(page: Page): Promise<{ id: string; name: string }> {
    const channel = CHANNELS.find((c) => c.enabled);
    expect(channel, "this console offers no connectable channel, so there is no bot to make").toBeDefined();
    const origin = `http://127.0.0.1:${String(NEXT_PORT)}`;
    const name = `probe-detail-${Date.now().toString(36)}-${String(Math.floor(Math.random() * 1000))}`;
    const created = await page.request.post(`${origin}/api/palai/v1/bots`, {
      headers: { ...(await sessionHeaders(page)), Origin: origin, "Content-Type": "application/json" },
      data: { name, kind: channel?.id, config: { [OPAQUE_KEY]: OPAQUE_VALUE } },
    });
    if (created.status() !== 201) {
      throw new Error(`POST /v1/bots answered ${String(created.status())} — nothing after this proves anything`);
    }
    const row = (await created.json()) as { id?: string };
    if (typeof row.id !== "string" || row.id === "") throw new Error("POST /v1/bots returned no id");
    return { id: row.id, name };
  }

  /** unregister removes a bot the same way an operator in another tab would. Used to make a write fail. */
  async function unregister(page: Page, id: string): Promise<void> {
    const origin = `http://127.0.0.1:${String(NEXT_PORT)}`;
    const gone = await page.request.delete(`${origin}/api/palai/v1/bots/${encodeURIComponent(id)}`, {
      headers: { ...(await sessionHeaders(page)), Origin: origin },
    });
    if (gone.status() !== 204) throw new Error(`DELETE /v1/bots/{id} answered ${String(gone.status())}, so the refusal below would not happen`);
  }

  /** openBot walks the operator's own way in: the list, then that bot's row. */
  async function openBot(page: Page, name: string): Promise<void> {
    await page.goto("/bots");
    await expect(page.getByTestId("panel-bots")).toBeVisible({ timeout: 15_000 });
    await page.locator("tr", { hasText: name }).getByTestId("bot-link").click();
    await expect(page.getByTestId("panel-bot-record")).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("bot-title")).toContainText(name);
  }

  /** openCredentials opens step 2 — the form the operator comes back to with three tokens. */
  async function openCredentials(page: Page): Promise<void> {
    await page.getByTestId("bot-credentials-open").click();
    await expect(page.getByTestId("bot-credentials-dialog")).toBeVisible();
  }

  test("the bot's page hands over a manifest carrying every shipped scope", async ({ page }) => {
    const { name } = await probeBot(page);
    await openBot(page, name);

    const manifest = page.getByTestId("bot-manifest");
    await expect(manifest).toBeVisible();
    const text = await manifest.innerText();
    for (const scope of SLACK_BOT_SCOPES) expect(text, `the rendered manifest is missing ${scope}`).toContain(scope);
    for (const event of SLACK_BOT_EVENTS) expect(text, `the rendered manifest is missing ${event}`).toContain(event);
    expect(text).toContain("socket_mode_enabled: true");
    expect(text).toContain("messages_tab_read_only_enabled: false");

    // THE APP IS NAMED AFTER THE BOT, which is what makes this the BOT's manifest rather than a copy of a
    // file. The whole LINE is compared: `name: "…"` is a substring of `display_name: "…"`, so a contains
    // check would pass on either and could not tell them apart.
    expect(text.split("\n")).toContain(`  name: "${name}"`);
    expect(text.split("\n")).toContain(`    display_name: "${name}"`);

    // AND THE OPERATOR IS TOLD WHERE IT GOES. A document with no destination is one they have to go looking
    // for, three steps into a job they are doing for the first time.
    await expect(page.getByTestId("bot-manifest-where")).toContainText("api.slack.com");
    await expect(page.getByTestId("bot-manifest-copy")).toBeVisible();
  });

  test("over the cap the manifest is withdrawn, and the count says by how much", async ({ page }) => {
    const { name } = await probeBot(page);
    await openBot(page, name);
    await expect(page.getByTestId("bot-manifest")).toBeVisible();

    await page.getByTestId("manifest-description-input").fill("x".repeat(MANIFEST_LIMITS.description + 7));
    // NO DOCUMENT IS OFFERED while Slack would refuse it: the copy control is gone, not merely decorated.
    await expect(page.getByTestId("bot-manifest")).toHaveCount(0);
    await expect(page.getByTestId("bot-manifest-copy")).toHaveCount(0);
    await expect(page.getByTestId("bot-manifest-refusal")).toContainText("shorten it by 7");
    await expect(page.getByTestId("manifest-description-input-count")).toContainText("7 too many");

    // And it comes back: a refusal is a state, not a dead end.
    await page.getByTestId("manifest-description-input").fill("A short description.");
    await expect(page.getByTestId("bot-manifest")).toBeVisible();
    await expect(page.getByTestId("bot-manifest-refusal")).toHaveCount(0);
  });

  test("the tokens are sealed first and the bot row is given only their NAMES", async ({ page }) => {
    const { name } = await probeBot(page);
    const requests: NetRequest[] = [];
    page.on("request", (r) => requests.push(r));
    await openBot(page, name);

    const team = `T${Date.now().toString(36).toUpperCase()}`;
    await openCredentials(page);
    await page.getByTestId("bot-team-input").fill(team);
    await page.getByTestId("bot-signing-secret-input").fill(SIGNING);
    await page.getByTestId("bot-token-input").fill(BOT_TOKEN);
    await page.getByTestId("bot-app-token-input").fill(APP_TOKEN);
    await page.getByTestId("bot-credentials-save").click();

    await expect(page.getByTestId("bot-credentials-status")).toContainText(`slack-signing-${team}`, { timeout: 15_000 });

    // THREE SEALS, THEN THE REVISION, IN THAT ORDER. A handle written into a row before its value is stored
    // resolves to nothing, and every consumer of an unresolvable handle fails SILENTLY by design — the exact
    // bug cmd/cli/internal/stack/up.go's slot table was written to close.
    const writes = requests
      .filter((r) => (r.method() === "POST" || r.method() === "PATCH") && r.url().includes("/api/palai/v1/"))
      .map((r) => `${r.method()} ${new URL(r.url()).pathname.replace("/api/palai/v1", "").replace(/\/bot_[^/]+$/, "/{id}")}`);
    expect(writes).toEqual(["POST /secret-refs", "POST /secret-refs", "POST /secret-refs", "PATCH /bots/{id}"]);

    // THE REVISION CARRIES HANDLES AND NOT ONE BYTE OF A CREDENTIAL — and nothing on the server would have
    // stopped it: `config` is a document the control plane never decodes, so the boundary is this console.
    const revision = requests.filter((r) => r.method() === "PATCH" && r.url().includes("/api/palai/v1/bots/")).at(-1);
    expect(revision, "no PATCH was observed").toBeDefined();
    const body = revision?.postData() ?? "";
    expect(body, "the revision carried no body").not.toBe("");
    for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
      expect(body.includes(secret), "the revision carried a raw credential").toBe(false);
    }
    // AND IT NAMES THE HANDLES up.go WOULD HAVE NAMED — same workspace, same three prefixes — so a bot
    // configured from this panel and one configured by the CLI resolve to the same three secrets.
    const sent = JSON.parse(body) as { config?: Record<string, unknown> };
    expect(sent.config?.signing_secret_ref).toBe(`slack-signing-${team}`);
    expect(sent.config?.bot_token_ref).toBe(`slack-bot-${team}`);
    expect(sent.config?.app_token_ref).toBe(`slack-app-${team}`);
    expect(sent.config?.team_id).toBe(team);
    // THE DOCUMENT IS THE WHOLE DOCUMENT. PATCH assigns rather than merges, so a key this console does not
    // understand survives only because the write starts from the row as it was read.
    expect(sent.config?.[OPAQUE_KEY], "a key this console does not understand was dropped from the document").toEqual(OPAQUE_VALUE);

    // THE POSITIVE CONTROL: the seals DID carry the values, so the sweep above can fail when it should.
    const seals = requests.filter((r) => r.method() === "POST" && r.url().endsWith("/api/palai/v1/secret-refs"));
    expect(seals).toHaveLength(3);
    const sealed = seals.map((r) => r.postData() ?? "").join("\n");
    for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) {
      expect(sealed.includes(secret), "a credential never reached the seal call").toBe(true);
    }

    // NOT IN A URL, AND NOT IN THE DOM.
    for (const r of requests) {
      for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) expect(r.url().includes(secret), "a credential is in a URL").toBe(false);
    }
    const content = await page.content();
    for (const secret of [SIGNING, BOT_TOKEN, APP_TOKEN]) expect(content.includes(secret), "a credential is in the DOM").toBe(false);
  });

  test("a refused write clears every credential field, and says by NAME what was already sealed", async ({ page }) => {
    const { id, name } = await probeBot(page);
    await openBot(page, name);
    // A REFUSAL THE SERVER PRODUCES, reachable on BOTH profiles because this test creates it: the row is
    // unregistered from under the open page, which is exactly what a second operator in another tab does.
    // The seals still happen — they are a different resource — so this drives the one arm that matters:
    // three credentials are live in the secret store under handles that now bind nothing.
    //
    // The CLIENT-side refusal (an empty workspace id) is deliberately not what is driven here. The field
    // carries `required`, so the browser's own constraint validation refuses the submit before any handler
    // runs; the check in app/bots/[id]/page.tsx is the guard behind that, not the message an operator reads.
    await unregister(page, id);

    const team = `T${Date.now().toString(36).toUpperCase()}`;
    await openCredentials(page);
    await page.getByTestId("bot-team-input").fill(team);
    await page.getByTestId("bot-signing-secret-input").fill(SIGNING);
    await page.getByTestId("bot-token-input").fill(BOT_TOKEN);
    await page.getByTestId("bot-app-token-input").fill(APP_TOKEN);
    await page.getByTestId("bot-credentials-save").click();

    const error = page.getByTestId("bot-credentials-error");
    await expect(error).toBeVisible({ timeout: 15_000 });
    await expect(error).toHaveAttribute("role", "alert");
    // BY NAME. A bare "the bot could not be updated" would leave three live credentials the operator does
    // not know exist, under names nothing in the deployment references.
    await expect(error).toContainText(`slack-signing-${team}`);
    await expect(error).toContainText(`slack-app-${team}`);

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
    const { name } = await probeBot(page);
    await openBot(page, name);

    await openCredentials(page);
    // A <form> with no method defaults to GET and every named field then goes into the query string — which
    // is how this tree once put an operator's password in the address bar, the browser history and the
    // access log.
    const form = page.locator("form").filter({ has: page.getByTestId("bot-signing-secret-input") });
    await expect(form).toHaveCount(1);
    await expect(form).toHaveAttribute("method", /^post$/i);

    for (const id of ["bot-signing-secret-input", "bot-token-input", "bot-app-token-input"]) {
      const input = page.getByTestId(id);
      await expect(input).toHaveAttribute("type", "password");
      await expect(input).toHaveAttribute("autocomplete", "new-password");
      const domID = await input.getAttribute("id");
      await expect(page.locator(`label[for="${String(domID)}"]`)).toHaveCount(1);
    }
  });
});
