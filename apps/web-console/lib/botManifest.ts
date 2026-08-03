// THE APP MANIFEST A BOT HANDS ITS OPERATOR (2026-08-03 plan, Task 12).
//
// WHAT THIS IS FOR. An operator creates a bot on /bots and then has to go and make it real in the channel's
// own admin UI. For Slack that means pasting an app manifest at api.slack.com — a document that decides,
// once, which events the workspace will deliver and which scopes the app will hold. It is the one part of
// this integration nobody compiles: get it wrong and the failure is silent in the worst way, because Slack
// simply never delivers an event nobody subscribed to.
//
// THE SOURCE OF TRUTH IS A SHIPPED FILE AND NOT THIS ONE. `deploy/slack/app-manifest.yaml` is what an
// operator pastes today; every scope in it is claimed by a UAT case, and `adapters/integrations/slack/
// manifest_test.go` already parses it and fails if it subscribes to an event this adapter has no verdict
// for. What is below is a COPY of that file's fixed half, because a browser bundle cannot read the
// repository — and a copy that drifts is worse than no generator at all, so tests/bot-manifest.spec.ts
// re-derives the scope set, the event set, the settings block and the two default values FROM the shipped
// file on every run and fails when this file disagrees. Adding a scope is a change in both places, and the
// test is what makes that a fact rather than a hope.
//
// WHAT VARIES PER BOT is exactly four things — the app's name, the bot user's display name, the agent
// panel's description and its suggested prompts. Everything else is fixed, and it is fixed because it was
// DECIDED: the nine scopes each name a caller, the five events each have a verdict, `agent_view` is a
// one-way door Slack's own reference says cannot be reversed, and `messages_tab_read_only_enabled: false`
// was found only by hitting it live (without it the panel renders and the human cannot type a word).
//
// IT REFUSES RATHER THAN TRUNCATES. A generator that silently cut a description at the cap would hand an
// operator a document whose most visible sentence is not the one they wrote, and they would find out in
// front of their workspace. A refusal costs them a shortened sentence before they leave this screen.

/** A static prompt Slack renders in the agent panel. Both halves are required — an object with a title and
 *  no message is not a prompt Slack will accept. */
export interface ManifestPrompt {
  title: string;
  message: string;
}

/** The four per-bot values. Everything else the document carries is fixed; see the header. */
export interface ManifestInput {
  /** display_information.name — the app's name in the workspace's app directory. */
  name: string;
  /** features.bot_user.display_name — what the bot is called when it speaks. */
  displayName: string;
  /** features.agent_view.agent_description — REQUIRED when agent_view is present. */
  description: string;
  /** features.agent_view.suggested_prompts — optional; an empty list omits the key entirely. */
  prompts: ManifestPrompt[];
}

/**
 * What buildManifest answers: a document, or the reasons there is none.
 *
 * `yaml` is non-empty EXACTLY when `refusals` is empty, which is the invariant a caller can rely on rather
 * than a convention it has to remember — a page that rendered a half-built manifest beside a refusal would
 * be offering a paste that wastes the trip this whole screen exists to make worthwhile.
 */
export interface ManifestDraft {
  yaml: string;
  refusals: string[];
}

/**
 * THE PUBLISHED CAPS, FETCHED 2026-08-03 FROM https://docs.slack.dev/reference/app-manifest/ and quoted:
 *
 *   display_information.name       — "Maximum length is 35 characters."
 *   features.bot_user.display_name — "Maximum length is 80 characters."
 *   features.agent_view.agent_description — "A string description of the agent. Maximum length is 300
 *                                    characters." (required when the agent_view subgroup is included)
 *
 * THE CHARACTER-CLASS RULE ON display_name IS NOT ENFORCED, AND THAT IS A MEASUREMENT RATHER THAN AN
 * OVERSIGHT. The same page says of display_name: "Allowed characters: `a-z`, `0-9`, `-`, `_`, and `.`" —
 * and `deploy/slack/app-manifest.yaml`, which is the manifest this deployment's own Slack app was created
 * from, sets `display_name: Palai` with a capital letter. A shipped, working document beats a documented
 * constraint: refusing what an operator can demonstrably paste would make this screen the thing that is
 * wrong. The caps are enforced because exceeding one is refused at paste time in a browser, three steps
 * into a trip the operator has already made.
 *
 * THE UNIT IS CODE POINTS, and the page does not say which unit it means. The Go guard on the shipped file
 * counts BYTES (`len()`), this counts characters as a reader would; the two agree on ASCII and can differ
 * by up to a factor of four on a description written in Turkish or Japanese. It is named here rather than
 * assumed away: a non-ASCII description within a few characters of the cap is the case where this console
 * would say yes and Slack could still say no.
 */
export const MANIFEST_LIMITS = { name: 35, displayName: 80, description: 300 } as const;

/**
 * THE NINE BOT SCOPES, copied from `deploy/slack/app-manifest.yaml` in its order.
 *
 * Each one has a caller and a UAT case behind it, written out in the shipped file rather than here — this
 * is the copy the browser can reach, and tests/bot-manifest.spec.ts pins it to that file line for line. An
 * unused scope is standing access, so a tenth entry is a decision somebody makes in the source file first.
 */
export const SLACK_BOT_SCOPES: readonly string[] = [
  "app_mentions:read",
  "assistant:write",
  "channels:history",
  "chat:write",
  "files:read",
  "files:write",
  "im:history",
  "search:read.public",
  "users:read",
];

/**
 * THE FIVE SUBSCRIBED BOT EVENTS, copied from the same file in its order.
 *
 * `adapters/integrations/slack/manifest_test.go` holds a verdict for each — whether it births a run — and
 * fails on a subscription nobody decided about, because an undecided event does not fail loudly: E20 found
 * that subscribing to app_home_opened without writing code for it made every panel open start a run with an
 * empty prompt.
 */
export const SLACK_BOT_EVENTS: readonly string[] = [
  "app_context_changed",
  "app_home_opened",
  "app_mention",
  "message.channels",
  "message.im",
];

/**
 * The two values an operator starts from, copied from the shipped file so the wizard's first draft IS the
 * manifest this deployment already runs on. Editing them is the point of the form; starting from a blank
 * description would be starting from a document Slack refuses.
 */
export const SLACK_MANIFEST_DEFAULTS: { description: string; prompts: ManifestPrompt[] } = {
  description:
    "Palai runs your agents where the work already is. Ask a question here and it starts a real run " +
    "against this workspace's configured agent; high-risk steps come back as an approval only a listed " +
    "approver can grant.",
  prompts: [
    { title: "What can you do?", message: "What can you do in this workspace, and what will you ask me to approve?" },
    { title: "Check the release", message: "What is still open on the current release?" },
  ],
};

/** The app description. FIXED: it describes Palai, not this bot, and the shipped file's wording is it. */
const APP_DESCRIPTION = "Palai agent platform — mentions open a run, buttons approve publications.";

/** count is the length in CODE POINTS — see MANIFEST_LIMITS on why the unit is stated rather than implied. */
const count = (value: string): number => [...value].length;

/**
 * quote renders an operator's text as a YAML double-quoted scalar, which is the ONE shape that can hold
 * anything a person types on ONE line.
 *
 * THIS IS THE WHOLE SAFETY ARGUMENT OF THE GENERATOR. Every fixed line below is copied from a document a Go
 * test parses on every run, so the only way this can emit something Slack refuses is through a value the
 * operator typed. A plain scalar would break on a leading `-`, a `#`, a `: `, or a newline — and a newline
 * is the dangerous one, because it does not produce an invalid document, it produces a VALID document with
 * a key nobody wrote. Double-quoted style has an escape for every one of those, so a value is a value.
 *
 * The shipped file writes agent_description as a folded `>-` block, which reads better in a file a human
 * edits. This does not fold: a fold's continuation lines are indentation-sensitive and a value with a blank
 * line or a leading space in it changes what the fold means, while a quoted scalar is exactly one line no
 * matter what is in it. tests/bot-manifest.spec.ts asserts that property directly — a hostile input and a
 * benign one produce documents with the same number of lines.
 */
function quote(value: string): string {
  let out = "";
  for (const ch of value) {
    const code = ch.codePointAt(0) ?? 0;
    if (ch === "\\") out += "\\\\";
    else if (ch === '"') out += '\\"';
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    // C0 and C1 controls, DEL, and the two Unicode line separators have no literal form inside a
    // double-quoted scalar. `\uXXXX` is YAML's own escape for them.
    else if (code < 0x20 || code === 0x7f || (code >= 0x80 && code <= 0x9f) || code === 0x2028 || code === 0x2029) {
      out += `\\u${code.toString(16).padStart(4, "0")}`;
    } else out += ch;
  }
  return `"${out}"`;
}

/** tooLong renders the one refusal an operator can act on: how far over, and what the field is. */
function tooLong(label: string, value: string, limit: number): string {
  return `${label} is ${String(count(value))} characters and Slack's manifest reference caps it at ${String(limit)} — shorten it by ${String(count(value) - limit)}. Slack refuses the whole manifest at paste time, in a browser, after you have already gone there.`;
}

/**
 * buildManifest renders the Slack app manifest for one bot, or refuses and says why.
 *
 * It is pure and takes no bot row: what varies is four values, and a function that took a BotRow would have
 * to decide which of its fields those four come from — a decision that belongs to the screen, where an
 * operator can see and change every one of them before it is pasted.
 */
export function buildManifest(input: ManifestInput): ManifestDraft {
  const name = input.name.trim();
  const displayName = input.displayName.trim();
  const description = input.description.trim();
  const prompts = input.prompts.map((p) => ({ title: p.title.trim(), message: p.message.trim() }));

  const refusals: string[] = [];
  if (name === "") refusals.push("The app needs a name — display_information.name is required, and it is what the workspace lists the app under.");
  else if (count(name) > MANIFEST_LIMITS.name) refusals.push(tooLong("The app name", name, MANIFEST_LIMITS.name));
  if (displayName === "") refusals.push("The bot user needs a display name — it is what the bot is called when it speaks in a channel.");
  else if (count(displayName) > MANIFEST_LIMITS.displayName) refusals.push(tooLong("The bot display name", displayName, MANIFEST_LIMITS.displayName));
  if (description === "")
    refusals.push("The agent description is required — this manifest declares an agent panel, and Slack refuses one without a description. It is the first thing a person reads when they open the panel.");
  else if (count(description) > MANIFEST_LIMITS.description) refusals.push(tooLong("The agent description", description, MANIFEST_LIMITS.description));
  // A prompt is a PAIR. Slack renders the title as the button and sends the message as the turn, so half a
  // prompt is a button that says nothing or one that sends nothing — and neither is a thing to ship into a
  // workspace. An operator who wants fewer prompts removes the row.
  for (const [i, prompt] of prompts.entries()) {
    if (prompt.title === "" || prompt.message === "") {
      refusals.push(`Suggested prompt ${String(i + 1)} is half-written: a prompt needs both a title (the button) and a message (what it sends). Fill both, or remove the row.`);
    }
  }
  if (refusals.length > 0) return { yaml: "", refusals };

  const lines = [
    "_metadata:",
    "  major_version: 1",
    "  minor_version: 2",
    "",
    "display_information:",
    `  name: ${quote(name)}`,
    `  description: ${quote(APP_DESCRIPTION)}`,
    '  background_color: "#1f2933"',
    "",
    "features:",
    "  bot_user:",
    `    display_name: ${quote(displayName)}`,
    "    always_online: false",
    // agent_view, NEVER assistant_view. Slack's manifest reference, quoted in the shipped file: "New apps
    // can only use agent_view… Switching an app from assistant_view to agent_view CANNOT BE REVERSED."
    "  agent_view:",
    `    agent_description: ${quote(description)}`,
    ...(prompts.length === 0
      ? // OMITTED RATHER THAN EMPTY. `suggested_prompts: []` declares a panel that offers nothing; leaving
        // the key out declares a panel that simply has no static prompts, which is what no prompts means.
        []
      : ["    suggested_prompts:", ...prompts.flatMap((p) => [`      - title: ${quote(p.title)}`, `        message: ${quote(p.message)}`])]),
    // THE MESSAGES TAB, AND THE SECOND LINE IS THE ONE NOBODY GUESSES. Without this block Slack leaves the
    // tab read-only: the panel renders, the prompts render, and the human cannot type. It was found live.
    "  app_home:",
    "    home_tab_enabled: false",
    "    messages_tab_enabled: true",
    "    messages_tab_read_only_enabled: false",
    "",
    "oauth_config:",
    "  scopes:",
    "    bot:",
    ...SLACK_BOT_SCOPES.map((scope) => `      - ${scope}`),
    "",
    "settings:",
    // Socket Mode carries events AND interactivity over one WebSocket, so no public HTTPS request URL, no
    // tunnel and no ngrok — which is what makes this manifest pasteable by an operator on a laptop.
    "  socket_mode_enabled: true",
    "  event_subscriptions:",
    "    bot_events:",
    ...SLACK_BOT_EVENTS.map((event) => `      - ${event}`),
    "  interactivity:",
    "    is_enabled: true",
    "  org_deploy_enabled: false",
    "  token_rotation_enabled: false",
  ];
  return { yaml: `${lines.join("\n")}\n`, refusals: [] };
}
