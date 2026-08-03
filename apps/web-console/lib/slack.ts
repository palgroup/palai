// THE SLACK WORKSPACE REGISTRATION, AS THE CONSOLE SEES IT.
//
// Everything here exists because of one measurement (2026-08-03):
//
//   grep -rn 'slack-connections' apps/web-console/app apps/web-console/lib  ->  0 hits
//
// The console could not register a Slack workspace at all. `palai up` was the only way, and it works by
// reading four values out of .env.local — which is precisely the arrangement the owner has now refused three
// times: runtime configuration that lives in a file on a machine.

/** SlackConnectionRow is GET /v1/slack-connections' row as api/slack_connections.go's listConnections
 *  projects it: {id, object, team_id, enterprise_id, bot_user_id, disabled}. It carries NO *_ref handles —
 *  a browse surface has no use for them, and a field that is never rendered is one that cannot be logged by
 *  accident. Every field is optional here because the projection writes some of them empty rather than
 *  omitting them, and a required field would be a lie the type checker enforced. */
export interface SlackConnectionRow extends Record<string, unknown> {
  id?: string;
  team_id?: string;
  enterprise_id?: string;
  bot_user_id?: string;
  disabled?: boolean;
  created_at?: string;
}

/** ApiKeyRow is GET /v1/api-keys' row, narrowed to what the principal picker needs. `principal_id` IS on the
 *  list projection — measured against the live stack rather than assumed, because a picker built on a field
 *  the list omits would have rendered an empty control on every real deployment. */
export interface ApiKeyRow extends Record<string, unknown> {
  id?: string;
  principal_id?: string;
  scopes?: string[];
  /** `string | null | undefined` because the two upstreams disagree and BOTH absent forms mean "live": the
   *  live control plane omits the field on an un-revoked key, the deterministic fixture writes `null`. A type
   *  that admitted only one of them would make the other a silent filter miss. */
  revoked_at?: string | null;
}

/**
 * SLACK_SECRET_SLOTS is the ONE table behind every Slack credential this console seals, and it is a
 * DELIBERATE MIRROR of cmd/cli/internal/stack/up.go's `slackSecretSlots`.
 *
 * WHY IT IS ONE TABLE, in up.go's own words, because the reason transfers exactly: "the failure it closes is
 * a PAIRING failure. `palai up` used to register `signing_secret_ref: slack-signing-<team>` — correctly a
 * handle, never a secret — while nothing stored a value under that name anywhere the control-plane could
 * redeem it. The handle resolved to nothing, and every consumer of it fails SILENTLY by design: an
 * unresolvable signing secret is a verification refusal (no config oracle), an unresolvable app token is a
 * dial that never happens."
 *
 * WHY THE NAMES ARE COPIED RATHER THAN CHOSEN. A console that invented its own naming would re-open that
 * hole from the other side: a workspace registered here and the same workspace registered by `palai up`
 * would resolve to different secret names, so re-running the bring-up after using the panel would seal a
 * second copy of every credential under names the registered connection does not point at. The handles are
 * derived from `<prefix><team>` on both sides, which is what makes them incapable of drifting.
 *
 * ponytail: there is no /v1 route serving this table, so it is duplicated rather than fetched — the same
 * trade /registry's FAMILIES records. A fifth credential means editing two places, and the pairing is
 * asserted by tests/slack-workspace.spec.ts reading the registration body for all three prefixes.
 */
export const SLACK_SECRET_SLOTS = [
  {
    field: "signing_secret_ref",
    prefix: "slack-signing-",
    label: "Signing secret",
    testId: "slack-signing-secret-input",
    required: true,
    hint: "App → Basic Information → App Credentials → Signing Secret. It verifies the v0 signature on every Events and interactivity callback: without it this deployment cannot tell a real Slack request from a forged one, which is why it is the one handle the API itself requires.",
  },
  {
    field: "bot_token_ref",
    prefix: "slack-bot-",
    label: "Bot token",
    testId: "slack-bot-token-input",
    required: false,
    hint: "App → OAuth & Permissions → Bot User OAuth Token (xoxb-…). It is what posts and updates the bot's own messages in a thread. Leave it empty and the workspace is registered but the bot can say nothing.",
  },
  {
    field: "app_token_ref",
    prefix: "slack-app-",
    label: "App-level token",
    testId: "slack-app-token-input",
    required: false,
    hint: "App → Basic Information → App-Level Tokens (xapp-…), with connections:write. It is Socket Mode's ONLY authentication. Leave it empty on a deployment Slack can reach over HTTP; without it the connect loop stays dormant and an @mention never arrives.",
  },
] as const;

/** slackHandle is the secret NAME a slot's value is sealed under for a workspace — `<prefix><team>`, the
 *  same derivation up.go uses, so the two cannot drift. */
export function slackHandle(prefix: string, teamID: string): string {
  return `${prefix}${teamID}`;
}

/** csvList splits a comma-separated field into a trimmed list, dropping empties. The API takes arrays. */
export function csvList(raw: string): string[] {
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}
