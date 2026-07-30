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
      "REPAIRED (E25 T2, plan §3.6 D6) — THE REASON THIS ROW GAVE IS NOW FALSE, AND IT WAS FALSE FOR MONTHS. It read 'deploy/compose/compose.yaml configures none', and compose.yaml:116 passes PALAI_SECRET_MASTER_KEY_FILE: /run/secrets/master_key — written LITERALLY, and `palai up`'s ensureSecretSlots guarantees the file it names holds a parseable key on every bring-up. What is still true is the MECHANISM: router.go mounts the whole /v1/secret-refs family only when cfg.secrets != nil, which main.go passes only when a master key is configured, so the console must still render an absent capability honestly rather than assume the route. What is no longer true is that a compose stack is such a stack. DISPOSITION, STATED RATHER THAN GUESSED: on a stack brought up by `palai local up` this row is expected to have CLOSED, and the sweep's staleness arm will say so by failing here — which is the correct signal and the whole reason the ledger fails in both directions. T2 could not run that sweep (it needs RUN_CONSOLE_REAL=1 and a compose stack, §6 leg 2, which is also why this row rotted), so the row is REPAIRED rather than deleted: deleting a measurement on the strength of a source read is how the false sentence got here in the first place. AND THIS ROW IS R1's PRECONDITION: T4's secret-ref list is not empty against a real stack, so a console secret form has something to write to.",
    owner: "compose tier — the master key IS configured now (compose.yaml:116); the next RUN_CONSOLE_REAL=1 sweep decides whether this row closes",
  },
  {
    id: "DIV-RTE-002",
    kind: "route",
    subject: "GET /v1/responses/{response_id}/artifacts",
    detail:
      "REPAIRED (E25 T2) — unmounted for the same reason as DIV-RTE-003, and repaired for the same reason; this is the list half of the same gate. See there.",
    owner: "REAL COMPOSE WIRING GAP, NOW WIRED — see DIV-RTE-003",
  },
  {
    id: "DIV-RTE-003",
    kind: "route",
    subject: "GET /v1/artifacts/{artifact_id}/content",
    detail:
      "REPAIRED (E25 T2, plan §3.6 D6) — THE WIRING GAP THIS ROW MEASURED HAS BEEN CLOSED AND THE ROW DID NOT NOTICE. It read that compose 'never passes PALAI_S3_ENDPOINT to the control-plane'; compose.yaml:77-78 passes PALAI_S3_ENDPOINT: http://object-store:8333 and PALAI_S3_BUCKET: palai-artifacts, deliberately NOT interpolated and with no empty default, and the comment above them names the five surfaces that were silently off while it was missing. The MECHANISM stands: router.go gates all three artifact routes on `artifacts != nil`, which main.go derives from artifactStoreFromEnv, which returns nil the moment PALAI_S3_ENDPOINT is empty (main.go:1417-1421) — so an artifact-less deployment still gets no artifact routes, and the console must still render that honestly. DISPOSITION: as with DIV-RTE-001, on a compose stack this row is expected to have CLOSED and the staleness arm is expected to fail here; that failure is the measurement T2 could not make (§6 leg 2). NOT REPAIRED BY DELETION, because the row also records WHY the console's one untrusted-bytes relay path had no real counterpart, and whether it now has one is a claim only a running stack can make.",
    owner: "deploy/compose/compose.yaml — WIRED (77-78); the next RUN_CONSOLE_REAL=1 sweep decides whether this row closes",
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
      "ENVELOPE divergence, and REPAIRED (E25 T2, plan §3.6 D6): the measurement was right and the conclusion was too strong. Over a page that ENDS the collection the real envelope is {data, has_more} — next_cursor and previous_cursor are omitempty pointers (contracts.Page) and neither is set. But renderPage GENUINELY MINTS next_cursor whenever has_more is true (api/pagination.go:226-227), so the real API does have forward cursors and this row used to read as though it had none. What is structurally absent is ONLY previous_cursor: beginList refuses ?before= with a 400 (pagination.go:179) and renderPage never populates PreviousCursor, so backward pagination does not exist on this surface at all. The divergence that remains is the fixture serving BOTH cursor keys as explicit nulls on an exhausted page where the real envelope omits them, and offering a previous_cursor the API will never mint. A console that paged on previous_cursor would page nothing — which is why components/Panel.tsx reads has_more + next_cursor and renders no backward control. Same for GET /v1/agents/{id}/revisions.",
    owner: "console fixture — the E13 T10 list-envelope family; the real envelope is the contract, and its forward half WORKS",
  },
  {
    id: "DIV-SHP-005",
    kind: "shape",
    subject: "GET /v1/agents/{agent_id}/revisions",
    detail:
      "The same envelope divergence as DIV-SHP-004, and repaired the same way: the real envelope is {data, has_more} plus a MINTED next_cursor when has_more is true (pagination.go:226-227); only previous_cursor is structurally absent. The fixture serves both cursor keys as explicit nulls.",
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
      "No run on a compose stack can reach approval.requested.v1, so the approval panel never renders and the approve/deny specs have nothing to act on. THREE independent blockers, each verified in the tree, each of which alone is sufficient: (1) deploy/compose/compose.yaml sets no PALAI_WORKSPACE_ROOT and mounts no shared workspace volume, so no repository is prepared and publication_registry.go's RunPublicationTarget finds nothing to publish; (2) the compose fake model adapter is hardcoded to fake.Script{Output:'ok'} with no ToolCalls and no env knob, so the model can never propose palai.publish.push; (3) publishApproved runs only inside a live attempt's boundary pump, so even an approved publication on a terminated run is never published. MEASURED, not inferred: POST /v1/sessions/{id}/commands {kind:'approve'} against the real stack returns 202 with status `rejected` and result.code `no_pending_approval`. The console is not the defect; it renders what the event carries, when the event exists. CLOSED, was blocker (3) of four: storage/queries/commands.sql PendingBoundaryCommands filtered `kind IN ('send_message','change_config')`, so command_pump.go's applyBoundaryApproval branch was unreachable from the public API and an HTTP approve sat queued until run terminal. The query now selects approve/deny and the pump applies them at a boundary (apps/control-plane/internal/execution/approval_pump_component_test.go). The remaining three still block the JOURNEY — nothing on compose produces an approval to approve — so this row and the skips citing it stand, with one reason fewer. CORRECTED (E21 T4, plan §3.6 D2): blocker (2) used to carry a second clause — 'and a fresh project's config_policy is NULL so NO tools are advertised' — which is FALSE and provably so. apps/control-plane/internal/execution/config.go UNIONs tool-set grants onto the baseline and unionTools does not short-circuit on a nil baseline, so a granted set is advertised on a project with no config_policy row at all; TestResolveGrantsToolSetsOnANullProjectBaseline pins it, and mcp_jira_component_test.go asserts the NULL-policy shape explicitly while the tool IS advertised. The first clause is true and blocks on its own, so the blocker stands and only its reasoning is repaired. A ledger is worth exactly the truth of its lines.",
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
      "The suite drives /v1/artifacts/art_evil/content — a fixture that deliberately replays an active content type, an `inline` disposition and a traversal filename. No real object store would serve such a response on demand, and no real stack holds an artifact called art_evil. The relay hardening it pins (coerced type, nosniff, forced attachment, sanitized filename, CSP) is real relay code; only the hostile INPUT is fixture-only. This row exists so the skip is never mistaken for the hardening being unproven — it is proven, on the profile that can produce an attacker. CORRECTED (E25 T2, plan §3.6 D6): this row used to carry a second reason — 'on compose that route is not even mounted (DIV-RTE-003)' — which is stale, because compose.yaml:77-78 passes PALAI_S3_ENDPOINT and the artifact routes mount. The row STANDS on its first reason, which was always the load-bearing one and blocks on its own: a hostile artifact has to be synthesised, and nothing on a real stack synthesises one. A ledger is worth exactly the truth of its lines.",
    owner: "console fixture, correctly — synthesising a hostile upstream is what a fixture is FOR",
  },
  {
    id: "DIV-UI-005",
    kind: "ui",
    subject: "the list-truncation proof over a collection larger than one page",
    detail:
      "tests/pagination.spec.ts drives a TWENTY-ONE row collection so the twenty-first row — the one components/Panel.tsx used to drop in silence (plan §3.6 D18, api/pagination.go defaultPageLimit = 20) — is observable: twenty rows, a truncation notice in TEXT, then a continuation with the server's own ?after= cursor. A fresh compose stack holds ZERO agents: nothing creates one, and no /v1 collection on a bootstrap stack exceeds a page, so has_more is false everywhere and there is no truncation to see. Synthesising a collection larger than a page is what the fake upstream is for, exactly as with the hostile artifact (DIV-UI-003). What does NOT narrow to the fixture is the absence claim: 'no list renders a previous control, and no request carries ?before=' runs on BOTH profiles, because beginList answers ?before= with a 400 (pagination.go:179) and a backward control could therefore never work against the real API.",
    owner: "console fixture, correctly — a collection larger than one page is state a bootstrap stack does not have; the sweep re-derives that the real agents list is short",
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
