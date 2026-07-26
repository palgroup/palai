// THE FAKE-VS-REAL DIVERGENCE LEDGER (E19 T7, plan §3.5 D15).
//
// E17 T10 built this console and every one of its proofs ran against tests/fake-control-plane.mjs. That
// epic's own exit gate then proved a fake CAN diverge from the real contract, and left the question open —
// "how is fixture drift caught?" — with no better answer than a reviewer noticing. This file plus
// tests/conformance.test.mjs is the answer. The sweep gathers the surface from a RUNNING REAL control plane
// and diffs it against the objects the fixture actually dispatches from; every difference must appear here,
// and every row here that has STOPPED being a difference fails too. The ledger can rot in neither
// direction: a new divergence is a red test, and a closed one is a red test.
//
// EVERY ROW BELOW WAS MEASURED, not reasoned about. They are the output of the first sweep run against a
// compose stack (`PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up`), and the sweep
// re-derives each one on every run. `owner` says who can close it, and the split matters: some rows are
// console fixture debt, several are real gaps in the control plane that no console change can fix.
//
// KINDS — each is re-derived by a specific arm of the sweep:
//   route       the fixture serves a (method, path-pattern) the real router does not register.
//               Evidence: OPTIONS on the real stack 404s with no Allow header (a resource-404 comes from a
//               handler and still carries Allow; only an unregistered PATTERN answers this way).
//   shape       both serve the route; the real body has a different key structure.
//   status      both serve the route; the real one answers a different status for the same request.
//   event       the fixture scripts an event type NO production code journals — the invented class.
//               Evidence: absent from a real run's stream AND zero non-test references under a repo-wide
//               `git grep` of the literal. Both halves are re-run by the sweep.
//   unexercised the fixture scripts a REAL event type that a compose-tier run cannot produce. Same absence
//               from the stream, but the grep RESOLVES — so this is a coverage gap, not an invention, and
//               conflating the two would be the dishonest move.
//   data        both journal the type; the real `data` object carries a different key set.
//   ui          a console SPEC that cannot run on the real profile because of the rows above.

/** @typedef {{id: string, kind: string, subject: string, detail: string, owner: string}} Divergence */

/** @type {Divergence[]} */
export const DIVERGENCES = [
  // =====================================================================================================
  // ROUTE SURFACE — three of the fifteen routes the fixture serves are not registered by the real router
  // on a compose stack. The console reads all three.
  // =====================================================================================================
  {
    id: "DIV-RTE-001",
    kind: "route",
    subject: "GET /v1/secret-refs",
    detail:
      "The console's admin page lists secret refs. router.go mounts the whole /v1/secret-refs family only when cfg.secrets != nil, which main.go passes only when a master key is configured; deploy/compose/compose.yaml configures none, so the route is absent and the panel is permanently empty against a compose stack. Not a defect — an unconfigured capability — but the fixture presented it as always-there.",
    owner: "compose tier (no master key by design); the console must render an absent capability honestly rather than assume the route",
  },
  {
    id: "DIV-RTE-002",
    kind: "route",
    subject: "GET /v1/responses/{response_id}/artifacts",
    detail:
      "Unmounted for the same reason as DIV-RTE-003 — see there; this is the list half of the same gate.",
    owner: "REAL COMPOSE WIRING GAP — see DIV-RTE-003",
  },
  {
    id: "DIV-RTE-003",
    kind: "route",
    subject: "GET /v1/artifacts/{artifact_id}/content",
    detail:
      "THE ARTIFACT RETRIEVAL SURFACE IS UNREACHABLE ON COMPOSE, AND THE REASON IS A WIRING GAP RATHER THAN A DELIBERATE TIER CHOICE. router.go gates all three artifact routes on `artifacts != nil`, which main.go derives from artifactStoreFromEnv — and that returns nil the moment PALAI_S3_ENDPOINT is empty. deploy/compose/compose.yaml starts a seaweedfs `object-store` service, publishes its port, healthchecks it, and makes the control-plane depend_on it healthy — and then never passes PALAI_S3_ENDPOINT to the control-plane. So compose runs an object store that the control plane cannot see, and the console's artifact download (which is the ONE relay path carrying untrusted bytes) has no real counterpart to be proven against.",
    owner: "REAL GAP in deploy/compose/compose.yaml — E19 T8 mount-derivation work; NOT closable from apps/web-console",
  },

  // =====================================================================================================
  // RESPONSE SHAPE — routes both serve, answered differently.
  // =====================================================================================================
  {
    id: "DIV-SHP-001",
    kind: "shape",
    subject: "GET /v1/organizations",
    detail:
      "Real: {id, object, display_name, created_at} with display_name EMPTY — ProvisionFirstOrg (identity/store.go) passes no orgName, so the bootstrap org has no display name at all. Fixture: {id, object, display_name} with 'Local Org'. The console's admin panel asserts on a display name the real bootstrap stack never sets.",
    owner: "console fixture; the empty seed name is real bootstrap behaviour, not a bug to fix here",
  },
  {
    id: "DIV-SHP-002",
    kind: "shape",
    subject: "GET /v1/projects",
    detail:
      "Real: {id, object, organization_id, display_name (empty), config_policy, created_at} and the id is `prj_local`. Fixture: {id, object, organization_id, display_name} and the id is `proj_local` — off by a letter from the id identity/store.go actually seeds, which is precisely the kind of thing a fixture gets away with forever until something compares it.",
    owner: "console fixture",
  },
  {
    id: "DIV-SHP-003",
    kind: "shape",
    subject: "GET /v1/api-keys",
    detail:
      "Real: {id, object, organization_id, project_id, principal_id, scopes, created_at} — and the bootstrap key's `scopes` is the EMPTY array (ProvisionFirstOrg seeds no scopes). Fixture: {id, object, project_id, scopes, revoked_at} with scopes ['provision','responses'] and a revoked_at the real view does not project. A console rendering a revocation column is rendering a field the API does not return.",
    owner: "console fixture",
  },
  {
    id: "DIV-SHP-004",
    kind: "shape",
    subject: "GET /v1/agents",
    detail:
      "ENVELOPE divergence, not just item fields: the real paginated envelope is {data, has_more}; the fixture adds next_cursor and previous_cursor. A console paginating on the fixture's cursors would page nothing against the real API. Same for GET /v1/agents/{id}/revisions.",
    owner: "console fixture — the E13 T10 list-envelope family; the real envelope is the contract",
  },
  {
    id: "DIV-SHP-005",
    kind: "shape",
    subject: "GET /v1/agents/{agent_id}/revisions",
    detail: "The same {data, has_more} vs {data, has_more, next_cursor, previous_cursor} envelope divergence as DIV-SHP-004.",
    owner: "console fixture",
  },
  {
    id: "DIV-SHP-006",
    kind: "shape",
    subject: "GET /v1/responses/{response_id}",
    detail:
      "Real terminal projection: {created_at, id, model, object, output, status, usage} — NO session_id and NO updated_at. Fixture adds both. The console reads the terminal Response after the stream ends; it does not read session_id from there today, which is the only reason this has never bitten.",
    owner: "console fixture",
  },
  {
    id: "DIV-SHP-007",
    kind: "shape",
    subject: "POST /v1/sessions/{session_id}/commands",
    detail:
      "Real: {created_at, id, kind, object, result, session_id, status} — and for an approve with nothing pending, status is `rejected` with result {code:'no_pending_approval'}, returned under a 202. Fixture: {id, object, kind, status:'accepted', session_id} with no `result` at all. So the fixture cannot express a DURABLY REJECTED command, which is the one outcome a real console operator most needs to see: a 202 here does not mean the command took effect.",
    owner: "console fixture — and the missing `result` rendering is a genuine console gap the real profile exposed",
  },

  // =====================================================================================================
  // STATUS.
  // =====================================================================================================
  {
    id: "DIV-STA-001",
    kind: "status",
    subject: "POST /v1/responses",
    detail:
      "With no Idempotency-Key the real router answers 400 missing_idempotency_key (middleware.RequireIdempotencyKey wraps this one route); the fixture answers 202. Any client path that creates a run without the header passes against the fixture and fails against the real API. The SDK always sends one, which is the only reason the console survives the difference — the fixture would not have caught a regression that stopped it.",
    owner: "console fixture",
  },

  // =====================================================================================================
  // EVENT VOCABULARY — THE INVENTED CLASS. This is E17 T10's invented-approval-event finding, measured and
  // found to be five times larger than the single instance that was known.
  //
  // The root cause is worth stating once: protocols/schemas/execution/event-types.json is a CATALOG of
  // names the registry ALLOWS, and the only check on it (execution/events.go) asserts emitted ⊆ registry,
  // never the reverse. A fixture author reading the registry as a description of what a run EMITS gets
  // exactly this fixture. Nothing in the tree contradicted them until this sweep.
  // =====================================================================================================
  {
    id: "DIV-EVT-001",
    kind: "event",
    subject: "response.queued.v1",
    detail:
      "The fixture opens every run with response.queued.v1. NO production code journals any response.*.v1: packages/state-machines/response.go's ResponseTable is never applied anywhere (a response is born queued; there is no transition into it). The console's progress lane leads with a frame no real client ever receives. A real run opens with run.queued.v1.",
    owner: "console fixture; the registry over-declaration is E19 T8 discovery-honesty input",
  },
  {
    id: "DIV-EVT-002",
    kind: "event",
    subject: "model_step.delta.v1",
    detail:
      "The fixture streams incremental model text as model_step.delta.v1 and the console's model_step lane renders `data.text` from it. The real orchestrator accumulates deltas IN PROCESS (model_dispatch.go onDelta/partial) and journals only model_step.created/completed — no delta ever reaches the journal, therefore none ever reaches SSE. Against the real API the console's model lane is silent until a step completes.",
    owner: "REAL GAP — token-level streaming has no journal representation, so no /v1 client can render live model text. E19 T8 public-API gap disposition",
  },
  {
    id: "DIV-EVT-003",
    kind: "event",
    subject: "tool_call.proposed.v1",
    detail:
      "The fixture announces a tool call before it runs. packages/state-machines/tool_call.go declares a Proposed state but nothing applies that transition; the first tool frame a real run journals is tool_call.executing.v1. A console listening only for `proposed` shows nothing until completion.",
    owner: "console fixture — the real pre-execution frame is tool_call.executing.v1",
  },
  {
    id: "DIV-EVT-004",
    kind: "event",
    subject: "artifact.created.v1",
    detail:
      "The fixture announces each artifact on the stream and the console reveals the download link mid-run. The real write path inserts the artifacts row with NO journal event, so a real client can only learn about artifacts by polling GET /v1/responses/{id}/artifacts after terminal — a route that is itself unmounted on compose (DIV-RTE-002).",
    owner: "REAL GAP — artifact creation is invisible to the stream. E19 T8 public-API gap disposition",
  },
  {
    id: "DIV-EVT-005",
    kind: "event",
    subject: "usage.updated.v1",
    detail:
      "The fixture emits running usage and the console renders a live usage panel. Real usage is settled into the usage ledger inside CommitModelResult (packages/coordinator/orchestration.go) with no event; a real client sees usage only on the terminal Response body. The live usage panel has no real feed.",
    owner: "REAL GAP — no incremental usage on the stream. E19 T8 public-API gap disposition",
  },

  // =====================================================================================================
  // EVENT VOCABULARY — THE UNEXERCISED CLASS. Real types with a real journaling site, which a compose-tier
  // run simply cannot reach. Kept strictly separate from the invented class above: the fixture is not
  // making these up, the tier cannot produce them, and pretending those are the same failure would hide
  // the five that ARE inventions.
  // =====================================================================================================
  {
    id: "DIV-UNX-001",
    kind: "unexercised",
    subject: "tool_call.completed.v1",
    detail:
      "Journaled by packages/state-machines/tool_call.go on Executing->Completed. A compose run reaches no tool call at all: the compose fake adapter is hardcoded to fake.Script{Output:'ok'} with no ToolCalls and no env knob (main.go modelBrokerFromEnv), and a fresh project's config_policy is NULL so the model is advertised NO tools. Real event, unreachable tier.",
    owner: "compose tier — a scriptable fake adapter (e.g. a PALAI_FAKE_SCRIPT_FILE knob) would close it",
  },
  {
    id: "DIV-UNX-002",
    kind: "unexercised",
    subject: "approval.requested.v1",
    detail: "Journaled by packages/coordinator/publication.go. Unreachable on compose for the three independent reasons enumerated in DIV-UI-001.",
    owner: "REAL GAPS — see DIV-UI-001",
  },
  {
    id: "DIV-UNX-003",
    kind: "unexercised",
    subject: "approval.approved.v1",
    detail: "Journaled by packages/coordinator/publication.go. Unreachable on compose for the reasons in DIV-UI-001 — but no longer because the HTTP approve path cannot apply a decision: that blocker is closed, and given a pending publication an approve now transitions it at the next boundary.",
    owner: "REAL GAPS — see DIV-UI-001",
  },
  {
    id: "DIV-UNX-004",
    kind: "unexercised",
    subject: "attempt.recovering.v1",
    detail: "Journaled by apps/control-plane/internal/execution/events.go. A clean compose run never fails, so the recovery ladder is never climbed. Real event, unexercised tier — tests/fault owns driving it.",
    owner: "compose tier — recovery is proven in tests/fault, not here",
  },
  {
    id: "DIV-UNX-005",
    kind: "unexercised",
    subject: "recovery.proof.v1",
    detail:
      "Journaled by apps/control-plane/internal/execution/events.go. Same reason as DIV-UNX-004 — and note the fixture's payload {checkpoint_id, detail} is not the real one either: the real payload is a whole recovery.RecoveryProof (previous/new attempt ids, level, checkpoint_id, workspace_snapshot_id, transcript_boundary_id, replayed/reused tool calls, semantic-loss assessment, duration_ms) which refuses to journal unless Complete(). The fixture's `detail` prose is invented.",
    owner: "compose tier for the absence; console fixture for the invented payload",
  },

  // =====================================================================================================
  // EVENT PAYLOAD SHAPE — types the real run DID journal, carrying keys the fixture does not (or inventing
  // keys the journal does not). The quieter half of the same class: the console reads a field that against
  // the real API is simply null forever.
  // =====================================================================================================
  {
    id: "DIV-DAT-001",
    kind: "data",
    subject: "run.running.v1",
    detail: "Fixture: {}. Real: {run_id, state} — every run-lifecycle transition carries both (packages/coordinator/store.go applyRunTransitionTx).",
    owner: "console fixture",
  },
  {
    id: "DIV-DAT-002",
    kind: "data",
    subject: "run.completed.v1",
    detail:
      "Fixture: {outcome:'completed'}. Real: {run_id, state}. `outcome` is INVENTED — the real terminal outcome is carried by `state`. The console's terminal lane happens to read the projected Response rather than this payload, which is the only reason the invention has been harmless.",
    owner: "console fixture",
  },
  {
    id: "DIV-DAT-003",
    kind: "data",
    subject: "model_step.created.v1",
    detail:
      "Fixture: {model_request_id}. Real: {model_request_id, run_id}. The fixture is a strict SUBSET here, which is the benign end of this class — it is listed because a silent subset is how the loud ones start, and because the staleness check should notice if it ever stops being a subset.",
    owner: "console fixture",
  },

  // =====================================================================================================
  // WHAT THE SPECS CANNOT DO ON THE REAL PROFILE. Each row is the reason a same-named spec is skipped on the
  // real profile — LOUDLY, citing this id — and each is a consequence of a row above. tests/profile.ts
  // refuses a skip that cites no row here, and the sweep refuses a row here it cannot re-observe.
  // =====================================================================================================
  {
    id: "DIV-UI-001",
    kind: "ui",
    subject: "the approval journey (UI-002 authoritative detail, approve, deny)",
    detail:
      "No run on a compose stack can reach approval.requested.v1, so the approval panel never renders and the approve/deny specs have nothing to act on. THREE independent blockers, each verified in the tree, each of which alone is sufficient: (1) deploy/compose/compose.yaml sets no PALAI_WORKSPACE_ROOT and mounts no shared workspace volume, so no repository is prepared and publication_registry.go's RunPublicationTarget finds nothing to publish; (2) the compose fake model adapter is hardcoded to fake.Script{Output:'ok'} with no ToolCalls and no env knob, and a fresh project's config_policy is NULL so NO tools are advertised — the model can never propose palai.publish.push; (3) publishApproved runs only inside a live attempt's boundary pump, so even an approved publication on a terminated run is never published. MEASURED, not inferred: POST /v1/sessions/{id}/commands {kind:'approve'} against the real stack returns 202 with status `rejected` and result.code `no_pending_approval`. The console is not the defect; it renders what the event carries, when the event exists. CLOSED, was blocker (3) of four: storage/queries/commands.sql PendingBoundaryCommands filtered `kind IN ('send_message','change_config')`, so command_pump.go's applyBoundaryApproval branch was unreachable from the public API and an HTTP approve sat queued until run terminal. The query now selects approve/deny and the pump applies them at a boundary (apps/control-plane/internal/execution/approval_pump_component_test.go). The remaining three still block the JOURNEY — nothing on compose produces an approval to approve — so this row and the skips citing it stand, with one reason fewer.",
    owner: "REAL GAPS in the control plane — the dead approve branch is FIXED; what remains is compose profile/config. E19 T8/T9 disposition; NOT closable from apps/web-console",
  },
  {
    id: "DIV-UI-002",
    kind: "ui",
    subject: "the lane-separated timeline, recovery lane, live usage and mid-stream artifact reveal",
    detail:
      "A compose run journals exactly six event types — run.queued/provisioning/running/completed and model_step.created/completed — and nothing else: no tool lane (DIV-UNX-001), no approval lane (DIV-UNX-002/003), no recovery lane (DIV-UNX-004/005), no usage lane (DIV-EVT-005), no artifact lane (DIV-EVT-004). The six-lane assertion is a fixture-shaped claim; against the real API the timeline is honestly two lanes.",
    owner: "mixed — DIV-EVT-004/005 are real gaps; the tool/recovery lanes need a run the compose fake adapter cannot produce",
  },
  {
    id: "DIV-UI-003",
    kind: "ui",
    subject: "the hostile-artifact download hardening suite",
    detail:
      "The suite drives /v1/artifacts/art_evil/content — a fixture that deliberately replays an active content type, an `inline` disposition and a traversal filename. On compose that route is not even mounted (DIV-RTE-003), and no real object store would serve such a response on demand anyway. The relay hardening it pins (coerced type, nosniff, forced attachment, sanitized filename, CSP) is real relay code; only the hostile INPUT is fixture-only. This row exists so the skip is never mistaken for the hardening being unproven — it is proven, on the profile that can produce an attacker.",
    owner: "console fixture, correctly — synthesising a hostile upstream is what a fixture is FOR",
  },
  {
    id: "DIV-UI-004",
    kind: "ui",
    subject: "the upstream half of the public-API-only proof (/__introspect)",
    detail:
      "The fixture exposes /__introspect so 'the relay addressed ONLY /v1/*, every one Bearered' is checkable from the UPSTREAM end. A real control plane has no such endpoint, so on the real profile only the BROWSER end runs: every captured request is same-origin under /api/palai/, none reached the upstream origin, the key appears in no request/chunk/source-map/response body, no websocket, no service worker. The browser end is the half that would actually catch a backchannel, so the loss is small — but it is a loss, and it is named rather than quietly dropped.",
    owner: "console fixture, correctly — an introspection endpoint is not something a control plane should ship",
  },
];

/** byId indexes the ledger so a skip can only cite a row that exists. */
export const DIVERGENCE_BY_ID = new Map(DIVERGENCES.map((d) => [d.id, d]));

/** find returns the single ledger row of a kind whose subject matches exactly, or undefined. */
export function find(kind, subject) {
  return DIVERGENCES.find((d) => d.kind === kind && d.subject === subject);
}
