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
  // ROUTE SURFACE — EMPTY, AND THAT IS A MEASUREMENT (E25 T2). This section held three rows: GET
  // /v1/secret-refs (unmounted because compose configured no secret master key) and the two artifact
  // retrieval routes (unmounted because compose never passed PALAI_S3_ENDPOINT). All three reasons had
  // become FALSE — compose.yaml:116 passes PALAI_SECRET_MASTER_KEY_FILE and compose.yaml:77-78 pass
  // PALAI_S3_ENDPOINT/PALAI_S3_BUCKET — and the rows sat unrepaired for months because the only thing that
  // reads them is a sweep nothing routine runs (§6 leg 2).
  //
  // They were not deleted on the strength of reading compose.yaml. A compose stack was brought up
  // (packaged CLI, `:local` images, no build) and probed the way ARM 2 probes, and all three answered
  // 405 with an Allow header — which means the PATTERN is registered:
  //     OPTIONS /v1/secret-refs                  -> 405  Allow: GET, HEAD, POST
  //     OPTIONS /v1/artifacts/{id}/content       -> 405  Allow: GET, HEAD
  //     OPTIONS /v1/responses/{id}/artifacts     -> 405  Allow: GET, HEAD
  // The staleness arm then named exactly these three and nothing else. So the divergence is CLOSED, and
  // this ledger's own rule for a closed row is to delete it — a row nobody can re-observe is decoration,
  // and decoration is what let the false sentences survive. The MECHANISM they recorded is not lost: both
  // families are still mounted CONDITIONALLY (router.go on cfg.secrets != nil and artifacts != nil), so a
  // deployment that configures neither still serves neither, and the console still has to render an absent
  // capability honestly rather than assume a route.
  //
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
      "ENVELOPE divergence, REPAIRED TWICE AND NARROWED TO WHAT CANNOT BE CLOSED. E25 T2 (plan §3.6 D6): the measurement was right and the conclusion was too strong — renderPage GENUINELY MINTS next_cursor whenever has_more is true (api/pagination.go:226-227), so the real API does have forward cursors and this row used to read as though it had none; what is structurally absent is ONLY previous_cursor, because beginList refuses ?before= with a 400 (pagination.go:179) and renderPage never populates PreviousCursor. E25 T6 then closed the half that WAS the fixture's fault: pageSlice served BOTH cursor keys as explicit nulls, offering a previous_cursor the API will never mint, and it now mints next_cursor only when rows remain and writes no previous_cursor at all. WHAT REMAINS IS NOT CLOSABLE AND IS THE SAME FACT AS DIV-UI-005: the fixture's agents collection holds TWENTY-ONE rows so that list truncation is observable at all, while a bootstrap stack holds a handful — so `next_cursor` is present on the fixture's first page and absent from the real one. The consequence is measured rather than assumed: an envelope difference SUBSUMES the item comparison, so the sweep never compares an AGENT row, and T6 seeded an agent on both sides expecting the item floor to rise by three and watched it rise by two. THE TRAILING SENTENCE OF THIS ROW USED TO READ 'Same for GET /v1/agents/{id}/revisions': that route now serves the real envelope and DIV-SHP-005 is CLOSED and deleted.",
    owner: "console fixture, IRREDUCIBLY — a collection larger than one page is state a bootstrap stack does not have, and it is the only way truncation is provable at all",
  },
  // DIV-SHP-005 WAS HERE, AND CLOSING IT WAS THE POINT (E25 T6). It recorded the same explicit-null cursor
  // keys on GET /v1/agents/{agent_id}/revisions — and the sweep's arm 3 treats an ENVELOPE difference as
  // subsuming the ITEM comparison, so as long as this row existed the revision row's SHAPE was never once
  // compared against the real projection. It was wrong the whole time: `published: true` where the real
  // projection says `status: "published"`, and no agent_id, revision_number, mcp_connections, environment or
  // instructions. The console does not paginate a lineage, so the fixture had nothing to gain from pageSlice
  // there; it now serves {data, has_more} and the item shape is compared on every sweep. A ledger row that
  // hides a second divergence is worse than the difference it records.
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
      "The fixture announces each artifact on the stream and the console reveals the download link mid-run. The real write path inserts the artifacts row with NO journal event, so a real client can only learn about artifacts by polling GET /v1/responses/{id}/artifacts after terminal. CORRECTED (E25 T2): this row used to add 'a route that is itself unmounted on compose (DIV-RTE-002)', which was measured false — the artifact routes ARE registered on a compose stack. The gap that remains is the one this row is about: artifact creation is invisible to the stream, so the only way to learn of an artifact is to poll after terminal.",
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
      "The suite drives /v1/artifacts/art_evil/content — a fixture that deliberately replays an active content type, an `inline` disposition and a traversal filename. No real object store would serve such a response on demand, and no real stack holds an artifact called art_evil. The relay hardening it pins (coerced type, nosniff, forced attachment, sanitized filename, CSP) is real relay code; only the hostile INPUT is fixture-only. This row exists so the skip is never mistaken for the hardening being unproven — it is proven, on the profile that can produce an attacker. CORRECTED (E25 T2, plan §3.6 D6): this row used to carry a second reason — that on compose the artifact route is not even mounted — and that was MEASURED FALSE on a running stack: OPTIONS /v1/artifacts/{id}/content answers 405 with an Allow header, so the pattern is registered. The row STANDS on its first reason, which was always the load-bearing one and blocks on its own: a hostile artifact has to be synthesised, and nothing on a real stack synthesises one. A ledger is worth exactly the truth of its lines.",
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
  {
    id: "DIV-UI-005",
    kind: "ui",
    subject: "the list-truncation proof over a collection larger than one page",
    detail:
      "tests/pagination.spec.ts drives a TWENTY-ONE row collection so the twenty-first row — the one components/Panel.tsx used to drop in silence (plan §3.6 D18, api/pagination.go defaultPageLimit = 20) — is observable: twenty rows, a truncation notice in TEXT, then a continuation with the server's own ?after= cursor. A fresh compose stack holds ZERO agents: nothing creates one, and no /v1 collection on a bootstrap stack exceeds a page, so has_more is false everywhere and there is no truncation to see. Synthesising a collection larger than a page is what the fake upstream is for, exactly as with the hostile artifact (DIV-UI-003). What does NOT narrow to the fixture is the absence claim: 'no list renders a previous control, and no request carries ?before=' runs on BOTH profiles, because beginList answers ?before= with a 400 (pagination.go:179) and a backward control could therefore never work against the real API.",
    owner: "console fixture, correctly — a collection larger than one page is state a bootstrap stack does not have; the sweep re-derives that the real agents list is short",
  },
  {
    id: "DIV-UI-006",
    kind: "ui",
    subject: "the tool-approval queue over a PARKED gated tool call",
    detail:
      "GET /v1/approvals is REGISTERED AND MOUNTED UNCONDITIONALLY on a compose stack (main.go:305 api.WithApprovals(repo), router.go:277-279) and it answers 200 with an EMPTY page — so the console's approval screen RENDERS on the real profile, and its scope sentence, its principal sentence, its polling sentence and its honest empty state are all asserted there. What cannot exist there is a ROW. A tool approval is created only when a gated tool call PARKS, and DIV-UNX-001 measured why a compose run reaches no tool call at all: the fake adapter is hardcoded to fake.Script{ProviderRequestID:'fake-local', Model:'fake', Output:'ok'} with no ToolCalls and no env knob (main.go modelBrokerFromEnv:732). No /v1 route parks one either — the surface E23 T9 opened READS the queue and DECIDES, it cannot fill it. So every leg that needs a parked call runs on the fixture: the arguments rendered verbatim, the hash carried out of the row's own data, the 400 with no hash, the 409 from a rotated hash, the 404/403 refusals, and the deny reason on the wire. THE CONTROL PLANE'S HALF OF THOSE LEGS IS PROVEN AGAINST A REAL STORE at the component tier — apps/control-plane/internal/execution/http_tool_approval_component_test.go, nine tests — and that file is also the ONLY place the parked run WAKING is proven (TestHTTPToolApprovalApproveWithTheBoundHashReleasesTheParkedRun). Neither console profile can show a run waking, and the console claims it nowhere: its claim ends at the decision reaching the single throat with the row's own binding. Re-derived by the sweep from a REAL run: after a run reaches terminal, the real queue is still empty.",
    owner: "compose tier — the same scriptable-fake-adapter gap as DIV-UNX-001; NOT closable from apps/web-console",
  },
  {
    id: "DIV-UI-007",
    kind: "ui",
    subject: "a run pinned to a published agent revision reporting THAT revision's model",
    detail:
      "The revision's model DOES reach the run on a compose stack — ResolveInput layers it above the project route and the deployment default (execution/config.go:114, layerAgentRevision) and the broker is dispatched with it — but it CANNOT BE OBSERVED, because the model a terminal Response reports is the one the ADAPTER answers with and the compose adapter answers with a constant. main.go modelBrokerFromEnv:732 builds fake.Adapter{Script: fake.Script{Model: 'fake'}} and adapters/models/fake/adapter.go:150 returns Script.Model verbatim, ignoring the requested model entirely; store/postgres.go:190 then projects that onto the response. So a run pinned to a revision naming any model at all reports 'fake' — the same value an UNPINNED run reports — and the assertion 'the terminal projection carries the revision's model' is unfalsifiable there rather than false. The fixture answers with the pinned revision's own model, which is what makes the assertion mean something on the fake profile. WHAT DOES NOT NARROW, and it is the stronger half of the pin travelling: the 409 for a DRAFT revision runs on BOTH profiles, and a 409 can only come from the server having resolved the id — api/responses.go:298 answers revision_not_published from a lookup made BEFORE the idempotency reserve (coordinator/store.go's PinnedRevisionNotPublished). Re-derived by the sweep from a REAL revision-pinned run rather than from this prose: an agent, a published revision naming a distinctive model, and a run pinned to it — and the response's model is asserted to be the adapter's constant. If the compose adapter ever echoes the model it was asked for, that assertion fails, this row is stale, and the crown leg in tests/config-journey.spec.ts must stop skipping on the real profile.",
    owner: "compose tier — the hardcoded fake adapter script, the same gap as DIV-UNX-001 and DIV-UI-006; NOT closable from apps/web-console",
  },
  {
    id: "DIV-UI-008",
    kind: "ui",
    subject: "registering an MCP connection, DISCOVERING it, and approving what it found",
    detail:
      "The register → discover → approve → pin → publish chain needs an UPSTREAM MCP SERVER, and a compose stack has none — nor should it dial one: `discover` is the only call on the tools screen that leaves the process, egress is vetted at registration and again before the dial, and a hermetic test stack reaching the public internet would be a worse property than the one this row records. So every leg that needs a DISCOVERED tool revision runs on the fixture: the untrusted description on screen, the gate defaulting ON, the un-gating click, the pin, and the set's contents read back. Re-derived by the sweep from the real stack rather than from this prose: its mcp_connections collection is EMPTY and nothing on a bootstrap stack creates one (`palai up` registers none; SLACK_AGENT_MCP names a connection an operator already made). WHAT DOES NOT NARROW, and it runs on BOTH profiles: the page itself, both panels' empty states, the two pickers refusing to degrade to free text, the absence of any credential field in the registration form, and the two E25 T7 READ routes — GET /v1/tools/{tool_id}/revisions and GET /v1/tool-sets/{set}/revisions/{revision_id} — which the sweep seeds and compares on both sides through the manual registry path, no MCP server involved. The control plane's half of the discovery chain is proven against a REAL Postgres and the REAL shipped router in apps/control-plane/internal/execution/jira_runbook_component_test.go, which executes docs/operations/jira-mcp-connection.md §3 over the public API alone.",
    owner: "console fixture, correctly — an upstream MCP server is a counterparty a hermetic stack must not have; the sweep re-derives that the real connection collection is empty",
  },
  {
    id: "DIV-UI-009",
    kind: "ui",
    subject: "a fleet with a SECOND machine, a machine WAITING to be admitted, and a non-zero live count",
    detail:
      "WRITTEN WRONG FIRST AND CORRECTED BY THE SWEEP THAT EXISTS TO CATCH EXACTLY THIS. The first draft of this row said a compose stack 'enrols NO MACHINES and cannot be made to', reasoning from the runner plane being a separate mTLS listener a host agent dials. That is false on this tree: deploy/compose/compose.yaml starts a `runner` SERVICE, `palai local up` mints it a token, and a bootstrap stack has ONE machine — `label: runner-local`, `state: active`, `pool_id: pool_default`, os and arch EMPTY (measured on a running stack, 2026-07-31). GET /v1/runners is therefore comparable and the sweep's item floor DOES rise for it. A ledger is worth exactly the truth of its lines. WHAT REALLY CANNOT EXIST THERE IS NARROWER AND IS FOUR THINGS. (1) A SECOND machine: compose starts exactly one runner service, so the concurrency notice — which fires on two or more enrolled rows — has nothing to fire on. (2) A machine in state `pending`: the only pool a bootstrap stack has is `pool_default` with `strict_enrollment: false`, and the runner enrols into it before anything could take it strict; switching a pool strict afterwards does NOT re-ask about the machines already in it (`FLT-P13`'s converse), and nothing dials a second enrolment. So the waiting room's row, the admission, and the tool-queue-versus-machine-queue converse all need the fixture. (3) The two 403 admissions: the console presents ONE key which holds every capability (an empty scope set satisfies Scope.HasScope) against an unconfigured — therefore permissive — approver list, so neither refusal is reachable by being itself, and there is no pending machine to aim at. (4) A NON-ZERO live count: `active_leases` and a pool's `waiting` are both 0 on an idle stack, and making either non-zero means holding a run open across the read. A pool key with machines BEHIND it is the same shape — a key minted from the console admits nothing until a host uses it. WHAT DOES NOT NARROW, and it is most of the screen: the page, the pool list, POOL CREATION, the strict switch, the key panel, minting a key and seeing its value once, the machine table with a REAL row in it, `last_seen_at`'s meaning and the absence of any health badge, the cordon confirmation, the revoke dialog INCLUDING its single-read network assertion and its review, both renderings of each pointer (a zero, and the absent-key state reached by rewriting the response at the browser boundary), all three ceiling sentences, the scope sentence, and axe over the whole page. Re-derived by the sweep from the real stack rather than from this prose: its `runners` collection holds exactly one row, that row is `active` rather than `pending`, and its only pool is not strict.",
    owner: "compose tier — one runner service, one pool, and an idle stack; NOT closable from apps/web-console",
  },
  {
    id: "DIV-UI-010",
    kind: "ui",
    subject: "pinning a bot to an agent, which needs an agent with a PUBLISHED revision",
    detail:
      "REWRITTEN FOR THE SCREEN THAT REPLACED THE ONE IT WAS WRITTEN ABOUT (2026-08-03 plan, Task 11). It described app/integrations, which wrote `slack_connections`; app/bots writes the kind-agnostic `integration_bots` registry, and the reason for the row is unchanged because it was never about Slack. app/bots resolves the chosen agent's newest PUBLISHED revision on submit and REFUSES an agent that has none, because the registry stores a REVISION id and admission verifies published_at before pinning a run — so pinning a draft would create a bot that refuses every message it ever receives. The fixture seeds exactly one such agent (agt_01 / seeded-agent-01, revision agrev_2 published), so the ONE leg that submits an agent runs there. A bootstrap compose stack has ZERO agents, which is a genuine property of a fresh stack rather than a fixture convenience. WHAT NARROWED SINCE THIS ROW WAS FIRST WRITTEN, and it narrowed because the API did: `slack_connections` REQUIRED a run target (vetSlackDefaultPolicy refused a registration without agent_revision_id and principal_id), so the happy path and the conflict retry both needed a published revision to exist and both were skipped here. `integration_bots` requires only a name and a kind — the columns default to '' and the relay omits an empty one from its Responses.Create call (relay/inbound.go) — so the console makes the agent OPTIONAL, and creating a bot, the credential clearing on a refused submit, and the name-conflict retry all run on BOTH profiles now. Only the pin resolution needs the fixture. WHAT NEVER NARROWED: the route, the empty state, the create dialog opening, the channel list showing its roadmap rows disabled, all three credential fields being password inputs with autocomplete=new-password and programmatic labels, the form carrying method=post so an unhydrated submit cannot put a token in the URL, that no credential reaches the registration body, and axe over the dialog with every credential control RENDERED.",
    owner: "compose tier — a bootstrap stack has no agent and no published revision; NOT closable from apps/web-console",
  },
];

/** byId indexes the ledger so a skip can only cite a row that exists. */
export const DIVERGENCE_BY_ID = new Map(DIVERGENCES.map((d) => [d.id, d]));

/** find returns the single ledger row of a kind whose subject matches exactly, or undefined. */
export function find(kind, subject) {
  return DIVERGENCES.find((d) => d.kind === kind && d.subject === subject);
}
