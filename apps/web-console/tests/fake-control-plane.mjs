// A deterministic fake control-plane for the console's browser proofs: it serves the /v1/* HTTP surface the
// console consumes (paths, methods, the Bearer gate, problem+json) and scripts a canonical event stream, so
// the proofs run against a fixed surface instead of a live provider — no credential spend, no flakiness,
// nothing to leak (a plain in-process HTTP server, no Docker, torn down with the Playwright webServer). This
// is the console's counterpart of examples/nextjs-sdk's fake-control-plane.
//
// E19 T7 — THE SURFACE IS A TABLE, AND THE TABLE IS THE DISPATCHER. `ROUTES` below is not a description of
// what this file serves; it IS what this file serves (`dispatch` walks it and nothing else answers a /v1
// request). `SCRIPTED_EVENTS` is likewise the one source of the event stream. Both are EXPORTED, and
// tests/conformance.test.mjs reads them straight out of this module to compare against the surface it
// gathers from a RUNNING REAL control plane. That is the D15 closure: a fixture cannot drift from what the
// sweep audits, because the sweep audits the very objects the server dispatches from.
//
// HONEST SEAM (named in the UI-001/UI-002 case text): this is a FAKE upstream, and the sweep has MEASURED
// how far it sits from the real one — see tests/divergences.mjs for the ledger, which is enforced, not
// prose. The automated axe/keyboard + public-API-only + exact-approval proofs run against this fixture; the
// real-profile run drives the same specs against a compose stack; a DEPLOYED console plus a manual
// VoiceOver/screen-reader pass remains the §6 operator leg above both.
//
// Every /v1/* endpoint is Bearer-gated, so the proof also shows the relay presents the credential
// SERVER-SIDE — while the browser scan shows it never reaches the client. /__introspect reports what the
// upstream actually received (only /v1/* paths, all with a Bearer, zero non-/v1 backchannel) so the
// public-API-only assertion is checkable from the upstream end too, not just the browser end.
import { createServer } from "node:http";
import { pathToFileURL } from "node:url";

const PORT = Number(process.env.FAKE_UPSTREAM_PORT ?? 3201);
const MODEL = "fake";
const FINAL_OUTPUT = [{ type: "output_text", text: "Release branch pushed after approval." }];
const USAGE = { input_tokens: 40, output_tokens: 18, total_tokens: 58, tool_calls: 1 };
const FRAME_GAP_MS = 60;

// Introspection of what the upstream actually received — the upstream half of the public-API-only proof.
//
// cookieBearingV1Requests is E25 T1's addition: the console's OPERATOR SESSION cookie must never ride an
// upstream request. The relay does not forward incoming headers at all (it calls client.request with a method,
// a path and a body), so this counter should be structurally pinned to zero — and "should be structurally"
// is exactly the kind of claim this tree has learned to count instead of assert.
const introspect = { v1Requests: 0, nonV1Requests: 0, beareredV1Requests: 0, unbeareredV1Requests: 0, cookieBearingV1Requests: 0, paths: [] };

// Per-session interactive approval state: the SSE pump pauses at approval.requested until an approve
// command lands on POST /v1/sessions/{id}/commands (a real round-trip through the relay).
const sessions = new Map(); // sid -> { approved: boolean, denied: boolean }
let seq = 0;

function newRun() {
  seq += 1;
  const n = String(seq).padStart(4, "0");
  const sid = `ses_console_${n}`;
  sessions.set(sid, { approved: false, denied: false });
  return { sid, rid: `resp_console_${n}`, runId: `run_console_${n}` };
}

function bearer(request) {
  const header = request.headers["authorization"];
  if (typeof header !== "string" || !header.startsWith("Bearer ")) return null;
  const token = header.slice("Bearer ".length).trim();
  return token === "" ? null : token;
}

function sendJSON(response, status, body) {
  response.writeHead(status, { "content-type": "application/json; charset=utf-8", "cache-control": "no-store" });
  response.end(JSON.stringify(body));
}

// `detail` is optional because most fixture refusals never had one; the real middleware.WriteProblem always
// carries the field, and the approval routes pass a DELIBERATELY BLAND one (E25 T5) so a proof can show the
// console authoring its own sentence per typed refusal rather than echoing the server's prose.
function sendProblem(response, status, code, detail) {
  response.writeHead(status, { "content-type": "application/problem+json; charset=utf-8" });
  response.end(
    JSON.stringify({
      type: `https://docs.palai.dev/problems/${code}`,
      title: code,
      status,
      code,
      ...(detail === undefined ? {} : { detail }),
      request_id: "req_fake_console",
    }),
  );
}

function listView(data) {
  return { object: "list", data };
}
function page(data) {
  return { data, has_more: false, next_cursor: null, previous_cursor: null };
}

// PAGE_LIMIT mirrors the real api/pagination.go `defaultPageLimit = 20`, and it is the whole reason the
// agents collection below holds TWENTY-ONE rows: the twenty-first row is the one the console used to drop
// silently, and a fixture that serves twenty can never demonstrate that.
const PAGE_LIMIT = 20;

// pageSlice serves ONE page of rows the way the real surface does (api/pagination.go renderPage): has_more
// is true exactly when rows remain, next_cursor is minted only then, and `previous_cursor` is present-and-
// null — which is the envelope divergence DIV-SHP-004/005 records and which the console must therefore never
// depend on. A `?before=` is REFUSED with a 400, exactly as beginList does (pagination.go:179), so the
// fixture cannot teach the console a backward-pagination contract the real API rejects.
//
// The cursor is `cur_<offset>`: opaque enough that the console must carry it rather than compute it, and
// deterministic enough to read in a failure message. The real cursor is an HMAC'd keyset position; nothing
// in the console may assume either shape.
function pageSlice(rows, url, response) {
  if (url.searchParams.get("before") !== null) {
    sendProblem(response, 400, "invalid_request");
    return;
  }
  const after = url.searchParams.get("after");
  const start = after === null ? 0 : Number(after.replace("cur_", ""));
  if (!Number.isInteger(start) || start < 0) {
    sendProblem(response, 400, "invalid_cursor");
    return;
  }
  const slice = rows.slice(start, start + PAGE_LIMIT);
  const hasMore = start + PAGE_LIMIT < rows.length;
  sendJSON(response, 200, {
    data: slice,
    has_more: hasMore,
    next_cursor: hasMore ? `cur_${start + PAGE_LIMIT}` : null,
    previous_cursor: null,
  });
}

// TWENTY-ONE agents: one more than a page. See PAGE_LIMIT.
const AGENTS = Array.from({ length: 21 }, (_, i) => ({ id: `agt_${String(i + 1).padStart(2, "0")}`, object: "agent" }));

// --- THE ENVIRONMENT SURFACE (E25 T4) -----------------------------------------------------------------
//
// STATEFUL ON PURPOSE, unlike every other admin fixture above. The console's environment screen creates,
// writes, ROTATES and unbinds, and a static row cannot express "the version went from 1 to 2" — which is the
// one observable difference between a create and a rotation, and therefore the only way the console's claim
// that they are the same route is checkable at all.
//
// AND IT NEVER RETURNS A VALUE. The bytes an operator types land in `receivedValues` below, which NO handler
// reads and NO response serializes — not even /__introspect, deliberately: an introspection endpoint that
// dumped them would put the sweep's own sentinel into a response body, and the sweep would be measuring the
// fixture's indiscretion instead of the console's. What the write route answers with is exactly what
// identity/environments.go answers with: {key, object, version, updated_at}.
//
// The shapes below are the REAL projections, field for field (identity/environments.go environmentView /
// environmentKeyView), including two details that are easy to get wrong and that the conformance sweep's item
// arm would catch: `key_count` is NOT omitempty so it is present as 0 on a fresh environment, and `keys` IS
// omitempty so it is ABSENT — not `[]` — on an environment with no keys.
const environments = new Map();
// The values the fixture was given. Write-only, like the store it stands in for. See above.
const receivedValues = new Map();
let environmentSeq = 0;

/** environmentListRow is the LIST projection: metadata plus the key COUNT, never the key names. */
function environmentListRow(env) {
  return { id: env.id, object: "environment", name: env.name, description: env.description, key_count: env.keys.size, created_at: env.created_at };
}

/**
 * environmentDetail is the DETAIL projection: the list row plus key NAMES, versions and update times.
 *
 * An UNKNOWN id is synthesised rather than 404'd, which is a deliberate fixture behaviour and not an
 * oversight: the conformance sweep's first arm requires every row of ROUTES to be genuinely served, and it
 * probes each pattern with a placeholder id. GET /v1/responses/{response_id} above has synthesised for the
 * same reason since E17 T10. The real API 404s an unknown id (RLS makes absent and foreign
 * indistinguishable), and no console path depends on either behaviour — the picker only ever offers ids the
 * list returned.
 */
function environmentDetail(id) {
  const env = environments.get(id) ?? { id, name: id, description: "", created_at: "2026-07-30T00:00:00Z", keys: new Map() };
  const row = environmentListRow(env);
  if (env.keys.size === 0) return row;
  return {
    ...row,
    keys: [...env.keys.entries()]
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(([key, meta]) => ({ key, object: "environment_key", version: meta.version, updated_at: meta.updated_at })),
  };
}

/** VALID_ENV_KEY mirrors toolbroker.ValidEnvKey's name rule, so the fixture refuses what the real route refuses. */
const VALID_ENV_KEY = /^[A-Z][A-Z0-9_]*$/;

// --- THE TOOL-APPROVAL QUEUE (E25 T5) -------------------------------------------------------------------
//
// THE ROW SHAPE IS api.PendingApproval FIELD FOR FIELD (apps/control-plane/api/approvals.go:52-67), and its
// last four fields ARE THE APPROVAL SCREEN as the server computed it — slack.DeriveApprovalDisplay's output,
// reaching the HTTP surface at internal/store/approvals.go:60. They are written below as LITERAL strings
// rather than serialized from an object, and that is the point: the canonical form is a Go one (encoding/json
// sorts every level's keys, two-space indent, and HTML escaping OFF), so a fixture that re-serialized in
// JavaScript would be teaching the console a formatting the API does not produce. The console must render
// these bytes VERBATIM; a fixture that cannot express the exact bytes cannot catch a console that reformats
// them.
//
// ONE ROW CARRIES `<@U0THERS>` ALREADY ESCAPED, ON PURPOSE. The HTTP surface inherits
// slack.NeutralizeBroadcasts (`<!` and `<@` become `&lt;!` / `&lt;@`, slack/stream.go:163) because both
// surfaces share the ONE derivation — so the console's screen is Slack-flavoured whether it wants to be or
// not, and the honest console shows the escape rather than "repairing" it back into a mention. That is what
// "render the screen the server computed" costs, and it is asserted rather than described.
//
// ANOTHER CARRIES ACTIVE MARKUP. The arguments are the ONE piece of model-authored prose on this screen (§2):
// no MCP `description` is even on the wire here. `<img src=x onerror=…>` therefore rides the fixture, because
// React's default escaping is the whole defence and a defence nobody attacks is a claim.
//
// THREE REFUSALS ARE KEYED TO ROWS, AND THAT IS SYNTHESIS RATHER THAN FIDELITY — named, because a fixture is
// exactly as honest as its comments. The real API keys 404 to an ID (unknown and foreign are
// indistinguishable on purpose) and both 403s to the PRINCIPAL (the key's `approve` capability; the project's
// approver list), while the console's key is fixed for the life of its process — so a per-row synthesis is the
// only way a BROWSER proof reaches those branches at all. Each branch is proven against a real store at the
// component tier (apps/control-plane/internal/execution/http_tool_approval_component_test.go). What is under
// test HERE is that the console does not FLATTEN them into one sentence, which is a rendering property.
//
// THE OTHER TWO REFUSALS ARE MECHANICAL, and they are the ones that matter: a decision with NO request_hash is
// refused exactly as api/approvals.go:200-204 refuses it, and `apvl_console_drift` ROTATES its arguments and
// its hash every time it is LISTED — so whatever hash the console carried is always the previous one, and its
// 409 comes from the one-shot binding genuinely failing rather than from a status literal. Rotating on EVERY
// serve (not once) is deliberate: the console polls, and a one-shot drift would make the 409 depend on whether
// a poll landed between the render and the click.
export const APPROVAL_ARGS_JIRA = `{
  "labels": [
    "&lt;@U0THERS>"
  ],
  "project": "OPS",
  "summary": "<img src=x onerror=window.__approval_xss_executed=true>"
}`;

// The CUT block, with truncateVisibly's own sentence and its own two numbers (slack/approval_display.go:178).
// The body is short here — carrying 8,000 real bytes would prove nothing extra — but the marker is verbatim,
// because the console must show the server's admission of the cut rather than author its own.
export const APPROVAL_ARGS_CUT = `{
  "body": "the first 8000 bytes of a very long patch"
… truncated: 8000 of 9214 bytes shown; the full arguments are on the tool call this button is bound to`;

/** approvalRow is the projection, field for field. `expires_at` is omitempty on the real view. */
function approvalRow(a) {
  return {
    id: a.id,
    object: "approval",
    tool_call_id: a.tool_call_id,
    run_id: a.run_id,
    session_id: a.session_id,
    response_id: a.response_id,
    request_hash: a.request_hash,
    ...(a.expires_at === undefined ? {} : { expires_at: a.expires_at }),
    created_at: a.created_at,
    identity: a.identity,
    operator_label: a.operator_label,
    arguments: a.arguments,
    truncated: a.truncated,
  };
}

const approvals = new Map(
  [
    {
      id: "apvl_console_0001",
      tool_call_id: "tc_jira_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: "sha256:1c0ffee1",
      expires_at: "2026-07-30T12:25:00Z",
      created_at: "2026-07-30T12:00:00Z",
      identity: "mcp:jira:create_issue",
      operator_label: "Files a ticket in the team's Jira project — everyone in the company can read it.",
      arguments: APPROVAL_ARGS_JIRA,
      truncated: false,
    },
    {
      id: "apvl_console_0002",
      tool_call_id: "tc_patch_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: "sha256:2beaded2",
      // NO expires_at: the real field is a pointer and omitempty, so a gate configured with no deadline
      // renders as one. A console that printed "Invalid Date" here would be showing its own bug as a fact.
      created_at: "2026-07-30T12:01:00Z",
      identity: "shell:apply_patch",
      // slack.NoOperatorLabel, verbatim: "nobody wrote one" is a different fact from "there is nothing to
      // say", and the human deciding is entitled to know which they are reading.
      operator_label: "(no operator label)",
      arguments: APPROVAL_ARGS_CUT,
      truncated: true,
    },
    {
      // TWO ROWS EXIST ONLY TO BE ANSWERED, and their existence is a measurement of this fixture rather than a
      // convenience: the queue is STATEFUL and an answered row LEAVES it (which is the only observable
      // difference between a question that has been answered and one that has not), so a row cannot be both the
      // subject of a read assertion and the subject of a decision. The first draft used one row for both and
      // every spec after the approve leg failed on "no such row" — the fixture's own state, not the console.
      id: "apvl_console_approve",
      tool_call_id: "tc_approve_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: "sha256:a11a11a1",
      expires_at: "2026-07-30T12:45:00Z",
      created_at: "2026-07-30T12:05:00Z",
      identity: "mcp:jira:create_issue",
      operator_label: "Files a ticket in the team's Jira project — everyone in the company can read it.",
      arguments: `{\n  "project": "OPS",\n  "summary": "release notes for 0.1.1"\n}`,
      truncated: false,
    },
    {
      id: "apvl_console_deny",
      tool_call_id: "tc_deny_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: "sha256:d11d11d1",
      expires_at: "2026-07-30T12:50:00Z",
      created_at: "2026-07-30T12:06:00Z",
      identity: "mcp:jira:create_issue",
      operator_label: "Files a ticket in the team's Jira project — everyone in the company can read it.",
      arguments: `{\n  "project": "OPS",\n  "summary": "please file this in the public project"\n}`,
      truncated: false,
    },
    {
      id: "apvl_console_drift",
      tool_call_id: "tc_drift_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      // Seeded OUTSIDE the rotation series (which starts at …d0000001). The first draft seeded it AT the first
      // rotated value, so the first serve handed out a hash the ledger then re-derived identically and the
      // decision was ACCEPTED — a drift row that did not drift, and a 409 leg that would have been green by
      // never happening. Probed, found, fixed.
      request_hash: "sha256:d0000000",
      expires_at: "2026-07-30T12:30:00Z",
      created_at: "2026-07-30T12:02:00Z",
      identity: "shell:git.push",
      operator_label: "Pushes a branch to the origin remote.",
      arguments: `{\n  "branch": "release",\n  "remote": "origin"\n}`,
      truncated: false,
      drifts: true,
    },
    {
      id: "apvl_console_gone",
      tool_call_id: "tc_gone_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: "sha256:900e900e",
      expires_at: "2026-07-30T12:35:00Z",
      created_at: "2026-07-30T12:03:00Z",
      identity: "mcp:jira:delete_issue",
      operator_label: "Deletes a ticket. There is no undo.",
      arguments: `{\n  "issue": "OPS-41"\n}`,
      truncated: false,
      refusal: "gone",
    },
    {
      id: "apvl_console_locked",
      tool_call_id: "tc_locked_1",
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: "sha256:10cked10",
      expires_at: "2026-07-30T12:40:00Z",
      created_at: "2026-07-30T12:04:00Z",
      identity: "shell:terraform.apply",
      operator_label: "Applies infrastructure changes to production.",
      arguments: `{\n  "workspace": "prod"\n}`,
      truncated: false,
      refusal: "not_an_approver",
    },
  ].map((a) => [a.id, a]),
);
let driftSeq = 0;

// THE TWO DECISION ROWS ARE RE-PARKED AFTER THEY ARE ANSWERED, with a NEW id each time.
//
// A fixture that its own proofs can EMPTY is a suite that passes once. Playwright reuses a running webServer
// locally (`reuseExistingServer: !CI`), and an answered row leaves this queue for good — so a second run of the
// same specs met the first run's leftovers and failed on "no such row", which is the fixture's state and not the
// console's behaviour. A run whose gated call was answered parks the next one; this is that, and nothing more.
//
// A NEW ID RATHER THAN THE SAME ONE, deliberately: "an answered row LEAVES the queue" has to stay true of the
// row that was answered, because that disappearance is the only observable difference between a question that
// has been answered and one that has not. The specs therefore select by PREFIX rather than by a fixed id.
let decisionSeq = 1;
function ensureDecisionRows() {
  for (const kind of ["approve", "deny"]) {
    const open = [...approvals.values()].some((a) => a.decided === undefined && a.id.startsWith(`apvl_console_${kind}`));
    if (open) continue;
    decisionSeq += 1;
    const id = `apvl_console_${kind}_${decisionSeq}`;
    approvals.set(id, {
      id,
      tool_call_id: `tc_${kind}_${decisionSeq}`,
      run_id: "run_console_apvl",
      session_id: "ses_console_apvl",
      response_id: "resp_console_apvl",
      request_hash: `sha256:${kind === "approve" ? "a11a11" : "d11d11"}${String(decisionSeq).padStart(2, "0")}`,
      expires_at: "2026-07-30T12:45:00Z",
      // Later than every seeded row, so the oldest-first order keeps the hand-written rows at the top of the
      // list and a re-parked one does not shuffle what the read-only specs are looking at.
      created_at: `2026-07-30T13:${String(decisionSeq).padStart(2, "0")}:00Z`,
      identity: "mcp:jira:create_issue",
      operator_label: "Files a ticket in the team's Jira project — everyone in the company can read it.",
      arguments: `{\n  "project": "OPS",\n  "summary": "release notes for 0.1.1"\n}`,
      truncated: false,
    });
  }
}

// The static admin fixtures — the §47.1 surface. Secret-ref rows are metadata only (name/version); a
// connection carries a secret REF name, never a value; an api-key row is metadata only.
const ADMIN = {
  organizations: listView([{ id: "org_local", object: "organization", display_name: "Local Org" }]),
  projects: listView([{ id: "proj_local", object: "project", organization_id: "org_local", display_name: "Default Project" }]),
  "api-keys": listView([{ id: "key_admin", object: "api_key", project_id: "proj_local", scopes: ["provision", "responses"], revoked_at: null }]),
  "model-connections": listView([{ id: "mc_1", object: "model_connection", provider: "fake", secret_ref: "provider-key" }]),
  "model-routes": listView([{ id: "mr_1", object: "model_route", name: "default" }]),
  // `updated_at` was ABSENT here until E25 T4 and nothing could catch it, for exactly the reason T2's seed
  // exists: a bootstrap stack's secret_refs collection is EMPTY, so the sweep's item arm had no real row to
  // compare against and skipped. T4's environment write is the first thing in this tree that PUTS a row
  // there (the derived name `env:<id>:<key>` IS a secret_refs row), and the very first comparison found the
  // missing field. The real projection is identity/secrets.go secretRefView: {name, object, version,
  // updated_at} with updated_at a non-nil pointer on every scanned row.
  "secret-refs": listView([{ name: "provider-key", version: 2, object: "secret_ref", updated_at: "2026-07-24T00:00:00Z" }]),
  // The knowledge-base row carries the field names the REAL projection carries — `name`, not `display_name`,
  // plus `created_at` (knowledge/views.go knowledgeBaseView; embedding_route and active_index_revision_id are
  // omitempty and absent on a freshly created base). It said `display_name` until E25 T2, which nothing had
  // ever caught because the real collection is EMPTY on a fresh stack and the conformance sweep skips an item
  // comparison when either side has no row. T2's seed gives the real side a row, so this now has to be right.
  "knowledge-bases": listView([{ id: "kb_1", object: "knowledge_base", name: "Docs KB", created_at: "2026-07-24T00:00:00Z" }]),
};
const AGENT_REVISIONS = page([
  { id: "agrev_2", object: "agent_revision", model: "fake", tools: ["add", "push"], published: true },
  { id: "agrev_1", object: "agent_revision", model: "fake", tools: ["add"], published: false },
]);

// The scripted canonical event stream — covers every §47.2 lane: model steps, tool activity, an approval
// (PAUSED until approved), recovery/attempt transitions, usage, and the terminal result. The frame after
// approval.requested is GATED on the approve command.
//
// SCRIPTED_EVENTS is the export the conformance sweep reads: [type, data, gate] rows, so the sweep gets the
// exact event VOCABULARY and the exact `data` KEY SET this fixture claims, and diffs both against what a
// real run on a real control plane actually journals. Five of these types are journaled by no production
// code path at all and three more carry invented keys — every one of them is an enforced ledger row in
// tests/divergences.mjs, not a comment.
export const SCRIPTED_EVENTS = [
  ["response.queued.v1", {}, null],
  ["run.running.v1", {}, null],
  ["model_step.created.v1", { model_request_id: "mreq_1" }, null],
  ["model_step.delta.v1", { model_request_id: "mreq_1", text: "Preparing the release push. " }, null],
  ["tool_call.proposed.v1", { tool_call_id: "tc_push", name: "git.push", arguments: { remote: "origin", branch: "release" } }, null],
  [
    "approval.requested.v1",
    {
      // The REAL journaled shape (packages/coordinator/publication.go): the AUTHORITATIVE, model-independent
      // detail is operation/branch/request_hash — the remote/branch come from the resolved binding, not model
      // output, and request_hash is the one-shot binding token.
      publication_id: "pub_1",
      operation: "push_branch",
      branch: "release",
      request_hash: "sha256:9f2b1c",
      // The proposal-supplied display string — NON-authoritative (the summary-equivalent). A soothing
      // "everything looks fine" here must NOT stand in for the authoritative operation/branch/request_hash.
      display: "Routine release push — everything looks fine, safe to approve.",
    },
    null,
  ],
  // GATED: this frame is withheld until the approve command arrives. Real shape: {publication_id, command_id}.
  ["approval.approved.v1", { publication_id: "pub_1", command_id: "cmd_approve_pub_1" }, "approved"],
  ["tool_call.completed.v1", { tool_call_id: "tc_push", name: "git.push", result: { pushed: true, head: "a1b2c3d" } }, null],
  // Recovery / attempt lane: a host loss and a checkpoint-proven resume.
  ["attempt.recovering.v1", { attempt: 2, detail: "runner host lost; recovering from the last checkpoint" }, null],
  ["recovery.proof.v1", { checkpoint_id: "ckpt_1", detail: "resumed from checkpoint 1 with no lost work" }, null],
  ["model_step.delta.v1", { model_request_id: "mreq_2", text: "Done." }, null],
  ["artifact.created.v1", { artifact_id: "art_1" }, null],
  ["usage.updated.v1", { ...USAGE }, null],
  ["run.completed.v1", { outcome: "completed" }, null],
];

function scriptedEvents(sid, rid, runId) {
  const base = { source: "palai://fake-control-plane", specversion: "1.0", session_id: sid, run_id: runId, datacontenttype: "application/json" };
  return SCRIPTED_EVENTS.map(([type, data, gate], i) => ({
    ...base,
    id: `evt_${String(i + 1).padStart(4, "0")}`,
    type,
    sequence: i + 1,
    time: new Date(Date.UTC(2026, 6, 24, 0, 0, i + 1)).toISOString(),
    data,
    gate,
  }));
}

function streamEvents(sid, rid, runId, request, response) {
  response.writeHead(200, {
    "content-type": "text/event-stream; charset=utf-8",
    "cache-control": "no-cache, no-transform",
    connection: "keep-alive",
    "x-accel-buffering": "no",
  });
  const events = scriptedEvents(sid, rid, runId);
  let index = 0;
  let timer = null;
  const stop = () => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  };
  response.once("close", stop);

  const write = (type, data) => {
    const ev = { ...events[0], id: `evt_end_${type}`, type, data };
    response.write(`id: ${ev.id}\nevent: ${type}\ndata: ${JSON.stringify(ev)}\n\n`);
  };

  const pump = () => {
    if (response.writableEnded || response.destroyed) {
      stop();
      return;
    }
    if (index >= events.length) {
      stop();
      response.end();
      return;
    }
    const event = events[index];
    const state = sessions.get(sid) ?? { approved: false, denied: false };
    // The gated (post-approval) frame is the interactive fork: approve resumes the normal tail (push
    // completes, recovery, terminal), deny BLOCKS the push and terminates the run canceled, and neither
    // yet is a genuine pause (the run stays waiting_for_approval).
    if (event.gate === "approved") {
      if (state.denied) {
        write("approval.denied.v1", { publication_id: "pub_1", command_id: "cmd_deny_pub_1" });
        write("run.canceled.v1", { outcome: "canceled", reason: "approval_denied" });
        stop();
        response.end();
        return;
      }
      if (!state.approved) {
        timer = setTimeout(pump, 40);
        return;
      }
    }
    index += 1;
    const frame = `id: ${event.id}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`;
    response.write(frame);
    timer = setTimeout(pump, FRAME_GAP_MS);
  };
  timer = setTimeout(pump, FRAME_GAP_MS);
}

// --- THE ROUTE TABLE --------------------------------------------------------------------------------
//
// One row per (method, path-pattern) this fixture serves. `pattern` is written in Go `net/http` ServeMux
// syntax ({name} wildcards) because that is the syntax the REAL router registers in
// apps/control-plane/api/router.go — so the sweep compares like with like instead of translating between
// two notations. `dispatch` compiles each pattern once and walks the table; NOTHING else answers a /v1
// request, which is what makes this table an honest inventory rather than a hopeful one.
//
// The list surfaces are enumerated one row per collection (not a single `/v1/{collection}` catch-all)
// precisely because the real router registers them one at a time: a catch-all would let the fixture serve
// a collection the real API has never heard of and still look conformant.
const adminList = (name) => (_req, res) => sendJSON(res, 200, ADMIN[name]);

/** requestURL re-parses a handler's request URL so a paginated route can read ?after= / ?before=. */
const requestURL = (request) => new URL(request.url ?? "/", `http://127.0.0.1:${PORT}`);

// decideApproval is BOTH decision routes — /approve and /deny are different URLs for the same reason
// POST /v1/schedules/{id}/pause and /resume are, and the body they take is identical (api/approvals.go:82-84).
//
// The refusal ORDER matters and it is the real handler's: the hash is checked at the EDGE before anything is
// resolved (api/approvals.go:200-204, so "no such approval" can never be the answer to a malformed decision),
// then resolution (404, unknown and foreign indistinguishable), then the approver policy (403), then the
// one-shot binding and the deadline (409). A fixture that checked them in a different order would let the
// console pass while mapping the wrong sentence onto the wrong cause.
const decideApproval = (approve) => (request, response, { approval_id: id }) =>
  drainBody(request, (raw) => {
    let body = {};
    try {
      body = JSON.parse(raw || "{}");
    } catch {
      /* tolerate — the real route answers 400 for a bad body, which arm 1 probes with "{}" */
    }
    const hash = typeof body.request_hash === "string" ? body.request_hash : "";
    if (hash === "") {
      return sendProblem(response, 400, "invalid_request", "request_hash is required: an approval id alone authorizes nothing");
    }
    const row = approvals.get(id);
    if (row === undefined || row.decided !== undefined || row.refusal === "gone") {
      return sendProblem(response, 404, "not_found", "no such approval");
    }
    if (row.refusal === "not_an_approver") {
      return sendProblem(response, 403, "not_an_approver", "this principal is not in the project's approver list");
    }
    if (hash !== row.request_hash) {
      return sendProblem(response, 409, "approval_not_decidable", "the approval was no longer decidable");
    }
    // ANSWERED. The row leaves the queue, which is the only observable difference between a question that has
    // been answered and one that has not — there is no `status` field on this projection to read instead.
    row.decided = approve ? "approved" : "denied";
    sendJSON(response, 200, { id, object: "approval.decision", decision: row.decided });
  });

export const ROUTES = [
  { method: "GET", pattern: "/v1/organizations", handle: adminList("organizations") },
  { method: "GET", pattern: "/v1/projects", handle: adminList("projects") },
  { method: "GET", pattern: "/v1/api-keys", handle: adminList("api-keys") },
  { method: "GET", pattern: "/v1/model-connections", handle: adminList("model-connections") },
  { method: "GET", pattern: "/v1/model-routes", handle: adminList("model-routes") },
  { method: "GET", pattern: "/v1/secret-refs", handle: adminList("secret-refs") },
  { method: "GET", pattern: "/v1/knowledge-bases", handle: adminList("knowledge-bases") },
  { method: "GET", pattern: "/v1/agents", handle: (request, response) => pageSlice(AGENTS, requestURL(request), response) },
  { method: "GET", pattern: "/v1/agents/{agent_id}/revisions", handle: (_req, res) => sendJSON(res, 200, AGENT_REVISIONS) },

  // --- ENVIRONMENTS (E25 T4). Five routes, matching api/router.go's registration order. ---
  {
    method: "POST",
    pattern: "/v1/environments",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        let body = {};
        try {
          body = JSON.parse(raw || "{}");
        } catch {
          /* tolerate — the real route answers 400 for a bad body, which arm 1 probes with "{}" */
        }
        const name = typeof body.name === "string" ? body.name.trim() : "";
        if (name === "") return sendProblem(response, 400, "invalid_request");
        // A duplicate NAME is a conflict in the real store (UNIQUE(organization_id, name)), reported rather
        // than silently returning someone else's environment.
        for (const env of environments.values()) {
          if (env.name === name) return sendProblem(response, 409, "conflict");
        }
        environmentSeq += 1;
        const env = {
          id: `env_console_${String(environmentSeq).padStart(4, "0")}`,
          name,
          description: typeof body.description === "string" ? body.description : "",
          created_at: new Date(Date.UTC(2026, 6, 30, 0, 0, environmentSeq)).toISOString(),
          keys: new Map(),
        };
        environments.set(env.id, env);
        sendJSON(response, 201, environmentListRow(env));
      }),
  },
  { method: "GET", pattern: "/v1/environments", handle: (_req, res) => sendJSON(res, 200, listView([...environments.values()].map(environmentListRow))) },
  { method: "GET", pattern: "/v1/environments/{environment_id}", handle: (_req, res, { environment_id: id }) => sendJSON(res, 200, environmentDetail(id)) },
  {
    // CREATE-OR-ROTATE, one route, because secret_refs is append-only. The version increments; the value is
    // stored where nothing serves it.
    method: "POST",
    pattern: "/v1/environments/{environment_id}/values",
    handle: (request, response, { environment_id: id }) =>
      drainBody(request, (raw) => {
        let body = {};
        try {
          body = JSON.parse(raw || "{}");
        } catch {
          /* tolerate */
        }
        const key = typeof body.key === "string" ? body.key : "";
        const value = typeof body.value === "string" ? body.value : "";
        if (key === "" || value === "" || !VALID_ENV_KEY.test(key) || key.startsWith("PALAI_")) {
          return sendProblem(response, 400, "invalid_request");
        }
        const env = environments.get(id);
        if (env === undefined) return sendProblem(response, 404, "not_found");
        const version = (env.keys.get(key)?.version ?? 0) + 1;
        const updated_at = new Date(Date.UTC(2026, 6, 30, 1, 0, version)).toISOString();
        env.keys.set(key, { version, updated_at });
        receivedValues.set(`env:${id}:${key}`, value); // write-only, served by nothing
        sendJSON(response, 201, { key, object: "environment_key", version, updated_at });
      }),
  },
  {
    // The BINDING goes; the bytes do not. The `note` is the real store's own wording, because a console that
    // paraphrases this would be paraphrasing a claim about whether a credential still exists.
    method: "DELETE",
    pattern: "/v1/environments/{environment_id}/values/{environment_key}",
    handle: (_req, response, { environment_id: id, environment_key: key }) => {
      // Synthesised for an unknown id/key, exactly as the detail route is and for the same reason — the
      // sweep's arm 1 probes this pattern with placeholder segments and a 404 there would read as "the table
      // declares a route the fixture does not serve". The real route 404s an unbound key; nothing in the
      // console depends on either answer, because the unbind control only exists on a key the list returned.
      environments.get(id)?.keys.delete(key);
      sendJSON(response, 200, {
        key,
        object: "environment_key",
        removed: true,
        note:
          "the binding was removed; the stored value's versions are retained and unreachable " +
          "(secret_refs is append-only for audit). Nothing names them and no run receives this key.",
      });
    },
  },

  // --- THE TOOL-APPROVAL QUEUE (E25 T5). Three routes, in api/router.go:277-279's registration order. ---
  {
    method: "GET",
    pattern: "/v1/approvals",
    handle: (request, response) => {
      const url = requestURL(request);
      // ?before= is a 400 here exactly as beginList answers it (api/pagination.go:179), so the fixture cannot
      // teach the console a backward-pagination contract the real API rejects.
      if (url.searchParams.get("before") !== null) return sendProblem(response, 400, "invalid_request");
      ensureDecisionRows();
      // OLDEST FIRST — these are questions, and the oldest is closest to its deadline (store/approvals.go:34).
      const open = [...approvals.values()].filter((a) => a.decided === undefined).sort((a, b) => (a.created_at < b.created_at ? -1 : 1));
      const rows = open.map(approvalRow);
      // THE DRIFT HAPPENS ON THE WAY OUT. The row the browser is about to hold is the PREVIOUS binding; the
      // ledger moves on. See the block comment above for why it is every serve rather than one.
      for (const a of open) {
        if (a.drifts !== true) continue;
        driftSeq += 1;
        a.arguments = `{\n  "branch": "release-${driftSeq}",\n  "remote": "origin"\n}`;
        a.request_hash = `sha256:d000000${driftSeq}`;
      }
      // The envelope is renderPage's over an EXHAUSTED page: {data, has_more} and NOTHING else. next_cursor and
      // previous_cursor are omitempty pointers on contracts.Page and neither is set when nothing remains, which
      // is why this route does not go through pageSlice — that helper serves both cursor keys as explicit nulls
      // (DIV-SHP-004) and the conformance sweep compares this envelope against the real one key for key.
      sendJSON(response, 200, { data: rows, has_more: false });
    },
  },
  { method: "POST", pattern: "/v1/approvals/{approval_id}/approve", handle: decideApproval(true) },
  { method: "POST", pattern: "/v1/approvals/{approval_id}/deny", handle: decideApproval(false) },

  {
    method: "POST",
    pattern: "/v1/responses",
    handle: (request, response) => {
      const { sid, rid, runId } = newRun();
      drain(request, () =>
        sendJSON(response, 202, { id: rid, object: "response", status: "queued", model: MODEL, session_id: sid, run_id: runId, created_at: "2026-07-24T00:00:00Z", output: [], usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 } }),
      );
    },
  },
  {
    method: "GET",
    pattern: "/v1/responses/{response_id}",
    handle: (_request, response, { response_id: rid }) => {
      const sid = rid.replace("resp_", "ses_");
      // The terminal projection reflects the ACTUAL outcome: a denied run is canceled with no output (the
      // side effect was blocked), an approved run completed with its output. This is the canonical terminal
      // Response the SDK retrieves after the stream's terminal event.
      const denied = sessions.get(sid)?.denied === true;
      sendJSON(response, 200, {
        id: rid,
        object: "response",
        status: denied ? "canceled" : "completed",
        model: MODEL,
        session_id: sid,
        created_at: "2026-07-24T00:00:00Z",
        updated_at: "2026-07-24T00:00:03Z",
        output: denied ? [] : FINAL_OUTPUT,
        usage: denied ? { input_tokens: 40, output_tokens: 0, total_tokens: 40, tool_calls: 0 } : USAGE,
      });
    },
  },
  {
    method: "GET",
    pattern: "/v1/responses/{response_id}/artifacts",
    handle: (_request, response) => sendJSON(response, 200, listView([{ id: "art_1", object: "artifact", filename: "release-notes.txt", byte_size: 24 }])),
  },
  {
    method: "GET",
    pattern: "/v1/sessions/{session_id}/events",
    handle: (request, response, { session_id: sid }) => streamEvents(sid, sid.replace("ses_", "resp_"), sid.replace("ses_", "run_"), request, response),
  },
  {
    method: "POST",
    pattern: "/v1/sessions/{session_id}/commands",
    handle: (request, response, { session_id: sid }) =>
      drainBody(request, (raw) => {
        let body = {};
        try {
          body = JSON.parse(raw || "{}");
        } catch {
          /* tolerate */
        }
        const state = sessions.get(sid);
        if (state) {
          if (body.kind === "approve") state.approved = true;
          if (body.kind === "deny") state.denied = true;
        }
        sendJSON(response, 202, { id: body.command_id ?? "cmd_1", object: "command", kind: body.kind ?? "", status: "accepted", session_id: sid });
      }),
  },
  {
    method: "GET",
    pattern: "/v1/artifacts/{artifact_id}/content",
    handle: (_request, response, { artifact_id: id }) => {
      // --- THE HOSTILE ARTIFACT ---
      // Artifact bytes are UNTRUSTED (a run's own output, an ingested source, an A2A-pushed file, a worker
      // result), and so are the headers the object store replays for them. `art_evil` is the attacker's
      // artifact: active content type, `inline` disposition, and a filename carrying traversal. If the relay
      // passed these through, the artifact would RENDER as script on the console's own origin — stored XSS
      // that could drive the relay (and therefore the whole API) as the signed-in operator. A literal CR/LF
      // is NOT used in the filename because Node's own header validation rejects it at write time (so does
      // any compliant upstream) — the relay's sanitizer still refuses CR/LF, which the spec asserts, but the
      // reachable attack is the traversal + the inline rendering.
      const evil = id === "art_evil";
      const bytes = Buffer.from(evil ? "<script>window.__xss_executed = true</script>" : "release notes: v0.1.1\n");
      const headers = {
        "content-type": evil ? "text/html; charset=utf-8" : "text/plain; charset=utf-8",
        "content-length": String(bytes.length),
        "content-disposition": evil ? 'inline; filename="../../etc/passwd.html"' : 'attachment; filename="release-notes.txt"',
        "cache-control": "no-store",
      };
      if (!evil) headers["content-digest"] = "sha-256=:fakedigestbase64==:";
      response.writeHead(200, headers);
      response.end(bytes);
    },
  },
];

// compile turns a ServeMux-style pattern into an anchored regex with named groups, so a route's wildcards
// reach its handler and the pattern stays the single spelling of the path.
function compile(pattern) {
  const source = pattern.replace(/[.+*?^$()[\]|]/g, "\\$&").replace(/\\?\{([a-z_]+)\\?\}/g, (_m, name) => `(?<${name}>[^/]+)`);
  return new RegExp(`^${source}$`);
}
const compiled = ROUTES.map((route) => ({ ...route, re: compile(route.pattern) }));

// dispatch is the ONLY thing that answers a /v1 request. A path that matches no row is a 404 — which is
// exactly what the sweep relies on when it asserts this table is the fixture's whole surface.
function dispatch(method, pathname, request, response) {
  for (const route of compiled) {
    if (route.method !== method) continue;
    const m = route.re.exec(pathname);
    if (m !== null) {
      route.handle(request, response, m.groups ?? {});
      return true;
    }
  }
  return false;
}

const server = createServer((request, response) => {
  const url = new URL(request.url ?? "/", `http://127.0.0.1:${PORT}`);
  const { pathname } = url;
  const method = request.method ?? "GET";

  if (method === "GET" && pathname === "/healthz") return sendJSON(response, 200, { status: "ok" });
  if (method === "GET" && pathname === "/__introspect") return sendJSON(response, 200, introspect);
  // The route table as this server dispatches from it — the runtime half of the conformance sweep's
  // "the table IS the surface" claim (the sweep also imports ROUTES directly, and asserts the two agree).
  if (method === "GET" && pathname === "/__routes") {
    return sendJSON(response, 200, ROUTES.map((r) => ({ method: r.method, pattern: r.pattern })));
  }

  // Record what the upstream received (the public-API-only proof, upstream end).
  if (pathname.startsWith("/v1/")) {
    introspect.v1Requests += 1;
    if (!introspect.paths.includes(pathname)) introspect.paths.push(pathname);
    if (typeof request.headers["cookie"] === "string") introspect.cookieBearingV1Requests += 1;
    if (bearer(request) === null) {
      introspect.unbeareredV1Requests += 1;
      return sendProblem(response, 401, "authentication_required");
    }
    introspect.beareredV1Requests += 1;
  } else {
    introspect.nonV1Requests += 1;
    return sendProblem(response, 404, "not_found");
  }

  if (dispatch(method, pathname, request, response)) return;
  return sendProblem(response, 404, "not_found");
});

function drain(request, done) {
  request.resume();
  request.once("end", done);
}
function drainBody(request, done) {
  let raw = "";
  request.on("data", (c) => (raw += c));
  request.once("end", () => done(raw));
}

// Only listen when run as a program. Imported (by the conformance sweep, for ROUTES/SCRIPTED_EVENTS) this
// module binds nothing.
if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  server.listen(PORT, "127.0.0.1", () => {
    process.stdout.write(`fake-control-plane (console) listening on http://127.0.0.1:${PORT}\n`);
  });
}
