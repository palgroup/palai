// THE CHANNEL LIST — the ONE place a bot's `kind` comes from, and the only file in this console that
// knows what "Slack" is.
//
// THE RULE IT EXISTS TO KEEP (2026-08-03 plan, Global Constraints): "kind bugün slack; yarın whatsapp,
// telegram, x. Hiçbir CP yüzeyi, hiçbir tablo ve hiçbir konsol bileşeni slack'i özel-durumlamaz — kanal,
// listeden seçilen bir değerdir." The control plane keeps that rule structurally: migration 000061 stores
// `kind` as a bare string and `config` as a `json` document no statement looks inside, and
// api/bots.go carries a guard test that fails if that file so much as mentions a channel by name.
//
// The CONSOLE cannot keep it structurally — somebody has to know that a Slack app has a signing secret —
// so it keeps it by CONFINEMENT: every channel-specific fact in the console is a row in the table below,
// and app/bots/page.tsx renders whatever the chosen row declares. `kind === "slack"` appears nowhere in
// this console; adding WhatsApp is a row here, and nothing else.
//
// WHAT `config` IS. It is the bot's whole channel-specific document, stored verbatim by the control plane
// and read by the RELAY process (apps/slack-bot) at startup through the SDK's `Bots.Get`. That process
// reads four environment variables and nothing else — "its kind, the agent it speaks as, its Slack
// tokens' secret handles, its channels, comes from its own bot row" (apps/slack-bot/main.go). So the keys
// below are a contract with a reader in another program, which is why each one names its reader.
//
// EVERY CREDENTIAL IN `config` IS A HANDLE, AND HERE THAT IS A DISCIPLINE RATHER THAN A BOUNDARY — which
// is the one place this surface is WEAKER than the screen it replaces, and it is worth being exact about.
// POST /v1/slack-connections could not accept a raw token: slackRegistrationBody declared no field for
// one, so DisallowUnknownFields turned an inline credential into a 400 at the edge. POST /v1/bots has no
// such defence and cannot have one: `config` is opaque by design, so a console that put a token inside it
// would be obeyed. The seal-then-name flow in app/bots/page.tsx is therefore the ONLY thing standing
// between an operator's token and a durable row, and tests/bots.spec.ts asserts it on the wire rather
// than trusting it.

import { buildManifest, type ManifestDraft, type ManifestInput, type ManifestPrompt, MANIFEST_LIMITS, SLACK_MANIFEST_DEFAULTS } from "@/lib/botManifest";

/** A plain text field of a channel's `config` document. */
export interface ChannelField {
  /** The key this value is stored under in `config`. */
  name: string;
  label: string;
  hint: string;
  testId: string;
  required?: boolean;
  /** Comma-separated in the box, an ARRAY in `config` — the shape the reader named in `hint` expects. */
  list?: boolean;
}

/** A credential slot: sealed by POST /v1/secret-refs, and only its NAME reaches `config`. */
export interface ChannelSecret {
  /** The key the sealed secret's NAME is stored under in `config`. Never the value. */
  field: string;
  /** The handle is `${prefix}${<the value of the form's handleKey field>}`. */
  prefix: string;
  label: string;
  hint: string;
  testId: string;
  required?: boolean;
}

/** What an enabled channel asks for. A channel with no adapter written has none of this. */
export interface ChannelForm {
  /**
   * The field whose value NAMES this channel's sealed handles.
   *
   * It MUST name a `required` field of `fields`: the handle is `${prefix}${value}`, so an empty value
   * would seal every bot's credentials under the same name — one bot silently rotating another's token.
   * tests/unit.spec.ts asserts that of every row in this table rather than leaving it to this sentence.
   */
  handleKey: string;
  fields: ChannelField[];
  secrets: ChannelSecret[];
}

/**
 * One value the operator fills in before the manifest is generated — the channel's OWN vocabulary.
 *
 * A manifest is a platform concept and so is every word in it ("agent panel", "bot user"), which is why
 * these are declared here beside the credential slots rather than written into the page. The page renders
 * what the row declares and hands the values straight back to `build`.
 */
export interface ManifestField {
  /** The key `build` receives this value under. */
  name: string;
  label: string;
  hint: string;
  testId: string;
  /** A long value gets a textarea; the default is a single-line box. */
  multiline?: boolean;
  /** The platform's published cap, in characters. Shown as a live count so the refusal is never a surprise. */
  limit: number;
  /** This field starts as the BOT'S OWN NAME — the operator named the bot once and should not name it twice. */
  fromBotName?: boolean;
  /** What it starts as when it is not the bot's name. */
  initial?: string;
}

/** A repeated pair of values the manifest carries a list of. Declared only by a channel that has one. */
export interface ManifestPromptGroup {
  label: string;
  note: string;
  addLabel: string;
  titleLabel: string;
  messageLabel: string;
  initial: readonly ManifestPrompt[];
}

/**
 * THE DOCUMENT AN OPERATOR CARRIES TO THE CHANNEL, and it is optional for a reason that is not tidiness.
 *
 * Slack asks for a manifest; another platform may ask for nothing but a token, or for a webhook URL it
 * gives you rather than one you give it. A disabled row has none of this at all — inventing a manifest for
 * an adapter nobody has written would be a form whose output is a document that configures nothing, which
 * is the same mistake as inventing credential fields for it.
 */
export interface ChannelManifest {
  /** What the platform calls it. Rendered as the section's name. */
  label: string;
  /** One sentence: what pasting it decides. */
  lead: string;
  /** The click path, in the platform's own words, so the operator does not need a second tab to find it. */
  where: string;
  /** Where they go. A constant of this file — never a value an operator or an API supplied. */
  href: string;
  fields: readonly ManifestField[];
  prompts?: ManifestPromptGroup;
  /** Renders the document, or refuses and says why. See lib/botManifest.ts for why it never truncates. */
  build: (values: Record<string, string>, prompts: readonly ManifestPrompt[]) => ManifestDraft;
}

export interface Channel {
  /** The registry's `kind`, verbatim. The control plane never interprets it. */
  id: string;
  label: string;
  /** Whether a bot of this kind can be created. A false row is a ROADMAP: shown, and not selectable. */
  enabled: boolean;
  /** Why it is not selectable yet, in words, beside the label. Required on a disabled row. */
  note?: string;
  /** Present exactly when `enabled` — an enabled channel that asks for nothing could not be configured. */
  form?: ChannelForm;
  /** Present only on an enabled channel whose platform is configured by a pasted document. */
  manifest?: ChannelManifest;
}

/**
 * THE SLACK CREDENTIAL SLOTS ARE `cmd/cli/internal/stack/up.go`'s slackSecretSlots, PREFIXES AND ALL, and
 * copying them is the point rather than a shortcut.
 *
 * up.go's own words on why that table exists: "the failure it closes is a PAIRING failure. `palai up` used
 * to register `signing_secret_ref: slack-signing-<team>` — correctly a handle, never a secret — while
 * nothing stored a value under that name anywhere the control-plane could redeem it. The handle resolved
 * to nothing, and every consumer of it fails SILENTLY by design."
 *
 * A console that invented its own naming would re-open that hole from the other side: the same workspace
 * configured from this panel and from the CLI would resolve to two different secrets. The derivation is
 * `<prefix><team>` on both sides, which is what makes them incapable of drifting.
 */
const SLACK: Channel = {
  id: "slack",
  label: "Slack",
  enabled: true,
  form: {
    handleKey: "team_id",
    fields: [
      {
        name: "team_id",
        label: "Workspace ID",
        required: true,
        testId: "bot-team-input",
        hint: "The `T…` id from App → Basic Information. Every credential below is sealed under a name derived from it, so it must be the real workspace id rather than a label.",
      },
      {
        name: "bot_user_id",
        label: "Bot user ID",
        testId: "bot-user-input",
        hint: "Optional, `U…`. The relay drops any inbound event whose author is this id — it is the self-loop guard (relay.InboundDeps.BotUserID), so without it the bot can answer its own messages.",
      },
      {
        name: "allowed_approvers",
        label: "Approvers",
        list: true,
        testId: "bot-approvers-input",
        hint: "Comma-separated Slack user ids (U…). This is the ONLY per-human authorization the approval bridge has (relay.ApprovalDeps.AllowedApprovers): the control plane sees one deciding principal — this bot's API key — for every click, so it cannot tell one clicker from another. An empty list denies everyone.",
      },
    ],
    secrets: [
      {
        field: "signing_secret_ref",
        prefix: "slack-signing-",
        label: "Signing secret",
        testId: "bot-signing-secret-input",
        hint: "App → Basic Information → App Credentials → Signing Secret. It verifies the v0 signature on every Events and interactivity callback: without it the relay cannot tell a real Slack request from a forged one.",
      },
      {
        field: "bot_token_ref",
        prefix: "slack-bot-",
        label: "Bot token",
        testId: "bot-token-input",
        hint: "App → OAuth & Permissions → Bot User OAuth Token (xoxb-…). It is what posts and updates this bot's own messages in a thread. Leave it empty and the bot is registered but can say nothing.",
      },
      {
        field: "app_token_ref",
        prefix: "slack-app-",
        label: "App-level token",
        testId: "bot-app-token-input",
        hint: "App → Basic Information → App-Level Tokens (xapp-…), with connections:write. It is Socket Mode's ONLY authentication; without it the relay's connect loop stays dormant and an @mention never arrives.",
      },
    ],
  },
  // THE MANIFEST STEP. It comes BEFORE the credentials above and not after, because the tokens do not exist
  // until the app does: an operator generates this, creates the app from it, and only then has a signing
  // secret to bring back. lib/botManifest.ts holds the document and the argument for every fixed line in it.
  manifest: {
    label: "App manifest",
    lead: "Slack builds the app from this document, and it is what decides which events the workspace will deliver and which scopes the app will hold.",
    where: "Create New App → From a manifest → pick the workspace → YAML",
    href: "https://api.slack.com/apps",
    fields: [
      {
        name: "name",
        label: "App name",
        testId: "manifest-name-input",
        limit: MANIFEST_LIMITS.name,
        fromBotName: true,
        hint: "What the workspace lists the app under. It starts as this bot's name; nothing joins the two afterwards, so renaming the bot here does not rename the app in Slack.",
      },
      {
        name: "displayName",
        label: "Bot display name",
        testId: "manifest-display-name-input",
        limit: MANIFEST_LIMITS.displayName,
        fromBotName: true,
        hint: "What the bot is called when it speaks in a channel. Slack's reference names an allowed character set (a-z, 0-9, - _ .) that the manifest this deployment already runs on does not obey, so it is not enforced here — only the length is.",
      },
      {
        name: "description",
        label: "Agent panel description",
        testId: "manifest-description-input",
        limit: MANIFEST_LIMITS.description,
        multiline: true,
        initial: SLACK_MANIFEST_DEFAULTS.description,
        hint: "The first thing a person reads when they open the agent panel. Slack REQUIRES it whenever a panel is declared and refuses the whole manifest without it.",
      },
    ],
    prompts: {
      label: "Suggested prompts",
      note: "Buttons Slack renders in the empty panel. They are static — part of this document, no API call and no runtime — and they are optional.",
      addLabel: "Add a prompt",
      titleLabel: "Button",
      messageLabel: "Sends",
      initial: SLACK_MANIFEST_DEFAULTS.prompts,
    },
    build: (values, prompts) =>
      buildManifest({
        name: values.name ?? "",
        displayName: values.displayName ?? "",
        description: values.description ?? "",
        prompts: prompts.map((p) => ({ title: p.title, message: p.message })),
      } satisfies ManifestInput),
  },
};

/**
 * CHANNELS is the list an operator chooses from, and the roadmap rows are IN it deliberately.
 *
 * A disabled row is not decoration and it is not a promise either: it says this console knows the channel
 * is coming and cannot connect one today. Hiding them would leave an operator to ask; inventing fields for
 * them would be worse — a form for an adapter nobody has written is a form whose every value goes nowhere.
 * So a roadmap row carries a label, a reason, and no form at all.
 */
export const CHANNELS: readonly Channel[] = [
  SLACK,
  { id: "whatsapp", label: "WhatsApp", enabled: false, note: "no adapter yet" },
  { id: "telegram", label: "Telegram", enabled: false, note: "no adapter yet" },
];

/** channelOf resolves a registry row's `kind`, or undefined for a kind this console does not offer. */
export function channelOf(kind: string): Channel | undefined {
  return CHANNELS.find((c) => c.id === kind);
}

/**
 * channelLabel is a `kind` as a cell: this console's word for it, or the RAW value when it has none.
 *
 * The raw fallback is the load-bearing half. POST /v1/bots accepts any `kind` — measured against the live
 * stack on 2026-08-03: `telegram`, `whatsapp` and a made-up kind all answered 201 — and that is the
 * property the control plane exists to keep. A list that rendered an unknown kind as blank, or hid the
 * row, would make this console the thing that breaks it.
 */
export function channelLabel(kind: string): string {
  return channelOf(kind)?.label ?? kind;
}

/** channelHandle is the secret NAME a slot's value is sealed under — `<prefix><scope>`, up.go's derivation. */
export function channelHandle(prefix: string, scope: string): string {
  return `${prefix}${scope}`;
}

/** csvList splits a comma-separated field into a trimmed list, dropping empties. `list` fields take arrays. */
export function csvList(raw: string): string[] {
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}
