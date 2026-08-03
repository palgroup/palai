// THE BOT REGISTRY, AS THE CONSOLE SEES IT (2026-08-03 plan, Task 4 + Task 11).
//
// GET /v1/bots' row, exactly as apps/control-plane/internal/bots renders it — measured against the live
// stack on 2026-08-03 rather than read off the struct:
//
//   {"id":"bot_7bc…","object":"bot","name":"ios-bot","kind":"slack","agent_revision_id":"",
//    "repository_binding_id":"","principal_id":"","config":{…},"disabled":false,"created_at":"…"}
//
// Every field is optional HERE and not on the wire, and the difference is deliberate: a required field in
// this type would be a claim the type checker enforces about a projection this console does not own.

/** BotRow is one registered bot. `config` is opaque to the control plane; see lib/channels.ts for what
 *  this console puts in it and which reader consumes each key. */
export interface BotRow extends Record<string, unknown> {
  id?: string;
  name?: string;
  kind?: string;
  agent_revision_id?: string;
  repository_binding_id?: string;
  principal_id?: string;
  config?: Record<string, unknown>;
  disabled?: boolean;
  created_at?: string;
}

/** BindingRow is GET /v1/repository-bindings' row, narrowed to what the repository picker needs. */
export interface BotBindingRow extends Record<string, unknown> {
  id?: string;
  repository_identity?: string;
  archived_at?: string | null;
}

/**
 * namedHandles counts how many of a channel's credential slots this row has a NAME for.
 *
 * It says "named", never "working", and the word is the whole point: `config` carries handles, and a
 * handle that resolves to nothing fails silently at every consumer — the exact failure up.go's slot table
 * was written to close. This column answers "did anyone seal these", which is the question an operator
 * asks of a bot that is not answering; whether the store can still redeem them is not visible from here
 * and this console must not imply that it is.
 */
export function namedHandles(config: Record<string, unknown> | undefined, fields: readonly string[]): number {
  if (config === undefined) return 0;
  return fields.filter((f) => typeof config[f] === "string" && config[f] !== "").length;
}
