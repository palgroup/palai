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
//
// `nonV1Paths` arrived in E28 T2 for a reason worth keeping: `nonV1Requests` went to 1 during a full-suite run
// and the counter could say only THAT, not WHICH — so the finding could not be triaged without editing the
// fixture. A counter whose failure cannot be diagnosed sends the reader to a bisect. The list is bounded by
// de-duplication, exactly like `paths`.
const introspect = { v1Requests: 0, nonV1Requests: 0, beareredV1Requests: 0, unbeareredV1Requests: 0, cookieBearingV1Requests: 0, paths: [], nonV1Paths: [] };

// Per-session interactive approval state: the SSE pump pauses at approval.requested until an approve
// command lands on POST /v1/sessions/{id}/commands (a real round-trip through the relay).
const sessions = new Map(); // sid -> { approved: boolean, denied: boolean }
let seq = 0;

// `model` is carried on the session so GET /v1/responses/{id} can project the model this run actually ran
// with (E25 T6). A run pinned to a published agent revision runs with the REVISION's model — ResolveInput
// layers it above the project route and the deployment default (execution/config.go layerAgentRevision) — and
// the terminal projection is where a console can see which configuration was used. On a compose stack it
// CANNOT be seen, because the fake adapter answers with a constant model whatever it is asked for; that is
// DIV-UI-007 and the sweep re-derives it.
function newRun(model = MODEL) {
  seq += 1;
  const n = String(seq).padStart(4, "0");
  const sid = `ses_console_${n}`;
  sessions.set(sid, { approved: false, denied: false, model });
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
// `page` was the {data, has_more, next_cursor: null, previous_cursor: null} envelope, and it is GONE (E25 T6)
// rather than left unused: renderPage never writes either cursor key as an explicit null — both are omitempty
// pointers on contracts.Page — so every use of it taught the console an envelope the real API does not
// produce, and offered a previous_cursor beginList refuses to honour. Its two callers now serve the real
// shape: pageSlice below mints next_cursor only when rows remain, and the revisions route serves {data,
// has_more}. What survives of DIV-SHP-004 is the difference that CANNOT be closed — see pageSlice.

// PAGE_LIMIT mirrors the real api/pagination.go `defaultPageLimit = 20`, and it is the whole reason the
// agents collection below holds TWENTY-ONE rows: the twenty-first row is the one the console used to drop
// silently, and a fixture that serves twenty can never demonstrate that.
const PAGE_LIMIT = 20;

// pageSlice serves ONE page of rows the way the real surface does (api/pagination.go renderPage): has_more is
// true exactly when rows remain, next_cursor is MINTED only then, and previous_cursor is never written at all
// — both are omitempty pointers on contracts.Page. It used to serve both keys as explicit nulls, which is the
// half of DIV-SHP-004 that E25 T6 CLOSED: an explicit `previous_cursor: null` is a key the real API never
// sends and a control the API would refuse to honour, so a fixture offering it is a fixture teaching a
// contract. A `?before=` is REFUSED with a 400, exactly as beginList does (pagination.go:179).
//
// WHAT REMAINS OF DIV-SHP-004 CANNOT BE CLOSED, and it is the same fact DIV-UI-005 records: this collection
// holds twenty-one rows so that truncation is observable at all, while a bootstrap stack holds at most a
// handful — so `next_cursor` is present on the fixture's first page and absent from the real one, and the
// sweep's item arm therefore never compares an AGENT row. That is measured rather than assumed: T6 seeded an
// agent on both sides expecting the item floor to rise by three and it rose by two.
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
    // Minted only when rows remain, and never a previous_cursor — renderPage's own behaviour.
    ...(hasMore ? { next_cursor: `cur_${start + PAGE_LIMIT}` } : {}),
  });
}

// TWENTY-ONE agents: one more than a page. See PAGE_LIMIT.
//
// STATEFUL SINCE E25 T6, and for the same reason the environment surface is: the console CREATES an agent and
// a revision, and a static row cannot express "this lineage now has a draft" or "that draft is now published"
// — which is the one observable difference between a revision a run can be pinned to and one it cannot. The
// twenty-one seeded rows stay, because they are what makes list truncation observable (DIV-UI-005), and a
// created row is appended so the collection only ever grows past a page.
//
// `name` IS ON THE ROW. The real projection is {id, object, name} (store/agents.go ListAgentProfiles), and the
// fixture said {id, object} until now — invisible to the conformance sweep's item arm because a bootstrap
// stack has ZERO agents, so the real side never had a row to compare against. T6's seed gives it one.
const AGENTS = Array.from({ length: 21 }, (_, i) => ({
  id: `agt_${String(i + 1).padStart(2, "0")}`,
  object: "agent",
  name: `seeded-agent-${String(i + 1).padStart(2, "0")}`,
}));
let agentSeq = 0;
// The E29 connection sequence, so a console-minted id is distinguishable from the seed's.
let connectionSeq = 0;

// --- THE AGENT LINEAGE (E25 T6) --------------------------------------------------------------------------
//
// revisions maps an agent id to its revision rows, NEWEST FIRST (which is the order ListAgentRevisions
// returns and the order components/AgentDiff.tsx diffs in: [0] against [1]).
//
// THE ROW SHAPE IS store/agents.go's ListAgentRevisions PROJECTION, field for field: {id, object, agent_id,
// revision_number, model, tools, mcp_connections, environment, instructions, status}. It used to be
// {id, object, model, tools, published} — a `published` BOOLEAN the real API has never sent, where the real
// field is a `status` STRING of "draft"/"published". Nothing caught it because the sweep's item arm probes
// /v1/agents/{agent_id}/revisions with a placeholder id, which on a real stack is an unknown profile and
// answers an EMPTY page. T6's seed substitutes a REAL agent id there, so this shape is now compared.
const revisions = new Map([
  [
    "agt_01",
    [
      { id: "agrev_2", revision_number: 2, model: "fake", tools: ["add", "push"], instructions: "", environment: "", status: "published" },
      { id: "agrev_1", revision_number: 1, model: "fake", tools: ["add"], instructions: "", environment: "", status: "draft" },
    ],
  ],
]);
let revisionSeq = 100;

/**
 * revisionRow is the projection, field for field.
 *
 * `tools`, `tool_sets` and `mcp_connections` are NULL when the revision named none, not `[]`, and that is
 * measured rather than stylistic: store/agents.go marshals `it.Tools` / `it.ToolSets` / `it.MCPConnections`,
 * which are Go `[]string` — a revision created without them holds nil and nil marshals to `null`. The
 * fixture served `[]` and the conformance sweep's item arm named the difference the first time it could see
 * this route at all.
 *
 * `tool_sets` JOINED THE REAL PROJECTION IN E25 T7 and joins this one with it. It is the GRANT (the
 * published set revisions a run pinned to this revision may reach); `mcp_connections` beside it is the
 * CEILING. Only the ceiling read back before, so a console could show the half that advertises nothing.
 */
function revisionRow(agentID, rev) {
  return {
    id: rev.id,
    object: "agent_revision",
    agent_id: agentID,
    revision_number: rev.revision_number,
    model: rev.model,
    tools: rev.tools ?? null,
    tool_sets: rev.tool_sets ?? null,
    mcp_connections: rev.mcp_connections ?? null,
    environment: rev.environment,
    instructions: rev.instructions,
    status: rev.status,
  };
}

/** findRevision locates a revision across every lineage — the pin on POST /v1/responses names no agent. */
function findRevision(id) {
  for (const [agentID, rows] of revisions) {
    const found = rows.find((r) => r.id === id);
    if (found !== undefined) return { agentID, revision: found };
  }
  return null;
}

// --- THE MCP CONNECTION + TOOL REGISTRY (E25 T7) ---------------------------------------------------------
//
// STATEFUL, and for the reason T4's environments are: the console's claim is "register a connection,
// discover it, approve what it found, pin it and publish the set", and a static row proves none of that. All
// four collections therefore start EMPTY — which is also what a bootstrap stack holds, so the fake profile
// meets the same first-day console a compose stack does.
//
// THE DISCOVERED DESCRIPTION IS HOSTILE ON PURPOSE. `mcpDiscoverable` below carries a <script> tag and an
// <a href>, because an MCP server's description is an ATTACKER'S TEXT and "we render it as text" is a claim
// that has to be attacked rather than asserted. tests/mcp-tools.spec.ts drives it onto the screen and then
// counts elements and navigations. It is the exact discipline the approval queue's own XSS leg uses on the
// model's arguments (approval-queue.spec.ts), applied to the other untrusted author.
const mcpConnections = new Map();
const toolLineages = new Map();
const toolRevisionsByTool = new Map();
const toolSetRevisions = [];
let mcpSeq = 0;
let toolSeq = 0;
let toolRevSeq = 0;
let toolSetSeq = 0;

// What the fixture "server" offers. Two tools, mirroring the Atlassian Rovo shapes the runbook names: a READ
// and a WRITE, so the screen's gate control has both cases to be exercised on.
const mcpDiscoverable = [
  {
    remote: "getJiraIssue",
    description: 'Get the details of a Jira issue by its key. <script>window.__mcp_xss_executed = true;</script> <a href="https://evil.example/pwned">docs</a>',
    input_schema: { type: "object", properties: { issueKey: { type: "string" } }, required: ["issueKey"] },
  },
  {
    remote: "transitionJiraIssue",
    description: "Move a Jira issue to another status.",
    input_schema: { type: "object", properties: { issueKey: { type: "string" }, transition: { type: "string" } } },
  },
];

/** mcpConnectionRow is store/mcp_connections.go's mcpConnectionProjection, field for field. */
function mcpConnectionRow(conn) {
  return { id: conn.id, object: "mcp_connection", name: conn.name, transport: conn.transport, trust_level: conn.trust_level, disabled: conn.disabled };
}

/** toolRow is store/tools.go's toolLineageProjection, field for field. */
function toolRow(tool) {
  return { id: tool.id, object: "tool", canonical_name: tool.canonical_name, model_visible_name: tool.model_visible_name };
}

/**
 * toolRevisionRow is store/tools.go's ListToolRevisions projection, field for field — INCLUDING description
 * and input_schema, which are the two fields the route exists for (they are what an admin approves).
 */
function toolRevisionRow(rev) {
  return {
    id: rev.id,
    object: "tool_revision",
    tool_id: rev.tool_id,
    revision_number: rev.revision_number,
    executor: rev.executor,
    description: rev.description,
    input_schema: rev.input_schema,
    digest: rev.digest,
    status: rev.status,
    approval_required: rev.approval_required,
    approval_label: rev.approval_label,
    created_at: rev.created_at,
  };
}

/** toolSetListRow is the LIST projection: identity + digest, and deliberately NOT the pins. */
function toolSetListRow(rev) {
  return { id: rev.id, object: "tool_set_revision", set: rev.set, revision_number: rev.revision_number, digest: rev.digest, status: rev.status };
}

/** toolSetDetailRow is the single-resource projection: the list row plus `tools` (the pins) and created_at. */
function toolSetDetailRow(rev) {
  return { ...toolSetListRow(rev), tools: rev.tools, created_at: rev.created_at };
}

/** fixtureTime keeps created_at deterministic and ordered without a clock. */
const fixtureTime = (seq) => new Date(Date.UTC(2026, 6, 30, 1, 0, seq)).toISOString();

// --- REPOSITORY BINDINGS (E25 T6) ------------------------------------------------------------------------
//
// STATEFUL, because the console's claim is "register one and it reads back on the list" and a static row
// cannot express that. The projection is what store/repository_bindings.go returns.
//
// AND IT REFUSES A NON-http(s) CLONE URL exactly as api/repository_bindings.go does (allowedCloneScheme): a
// fixture that accepted a `file:` URL would teach the console that the field takes one, and the real refusal
// is a §24 trust boundary — on a collapsed single-host deployment a local path would let one tenant point a
// clone at another tenant's allocation.
const bindings = [];
let bindingSeq = 0;

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

// --- RUN HISTORY, ARTIFACT METADATA AND METERING (E25 T8) ------------------------------------------------

// responseListRows renders every run this fixture has created as GET /v1/responses renders one, NEWEST
// FIRST — the order storage/queries/responses.sql gives (`ORDER BY created_at DESC, id DESC`), which is what
// lets the console's history screen open "the run I just made" without naming an id.
//
// The KEY SET is store/postgres.go ListResponses': a zero Usage marshals to {input_tokens, output_tokens}
// (total_tokens and tool_calls are omitempty and zero), Model is "" and present, Output is [], and RunID and
// UpdatedAt are omitempty and absent. A list row is deliberately poorer than the detail — that is the
// contract, not a shortcut.
function responseListRows() {
  const rows = [];
  for (const [sid, state] of sessions) {
    rows.push({
      created_at: fixtureTime(rows.length),
      id: sid.replace("ses_", "resp_"),
      model: "",
      object: "response",
      organization_id: "org_local",
      output: [],
      project_id: "proj_local",
      session_id: sid,
      status: state.denied === true ? "canceled" : "completed",
      usage: { input_tokens: 0, output_tokens: 0 },
    });
  }
  return rows.reverse();
}

// artifactProjection is the artifact metadata shape BOTH artifact reads render — the list and the per-id
// GET share artifacts/reader.go metadataRow.projection(), so they are one function here too. There is no
// `filename` and no `byte_size`: the projection carries neither, and a download's filename comes from the
// object store's own disposition (sanitized by the relay), never from this metadata.
function artifactProjection(id) {
  return {
    id,
    object: "artifact",
    run_id: "run_console_0001",
    size_bytes: 22,
    checksum: "sha256:7f0d1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7",
    media_type: "text/plain",
    logical_type: "release_notes",
    malware_scan_status: "not_scanned",
    created_at: "2026-07-24T00:00:03Z",
  };
}

// The metering fixtures. Meters and the ledger carry rows so the console's columns are exercised; budgets
// and quotas are empty on both profiles and the ROUTES table says why.
//
// THE METER NAMES ARE THE REAL ONES, and getting that wrong is the exact defect this task caught one file
// over. coordinator/usage.go names them: `run.admitted` (unit "run", settled inside the ADMISSION
// transaction, so a real stack has this row the moment a run is created) and `model.input_tokens` /
// `model.output_tokens` (unit "token"). A compose run settles ONLY the first — settleUsage skips a
// zero-quantity entry and the compose fake adapter reports no tokens — so the two model meters here are
// UNEXERCISED real names rather than invented ones, which is a distinction this tree enforces elsewhere and
// should not blur in a fixture.
const USAGE_METERS = [
  { meter: "run.admitted", unit: "run", quantity: 1, entries: 1 },
  { meter: "model.input_tokens", unit: "token", quantity: 40, entries: 1 },
  { meter: "model.output_tokens", unit: "token", quantity: 18, entries: 1 },
];

// The ledger row's key set is metering/store.go ledgerEntryView: session_id and run_id are omitempty and are
// present here because a settled model step always carries both.
const USAGE_LEDGER = [
  {
    id: "use_fixture000000000001",
    object: "usage_ledger_entry",
    schema_version: 1,
    project_id: "proj_local",
    session_id: "ses_console_0001",
    run_id: "run_console_0001",
    meter: "tokens.input",
    quantity: 40,
    unit: "token",
    occurred_at: "2026-07-24T00:00:02Z",
  },
  {
    id: "use_fixture000000000002",
    object: "usage_ledger_entry",
    schema_version: 1,
    project_id: "proj_local",
    session_id: "ses_console_0001",
    run_id: "run_console_0001",
    meter: "tokens.output",
    quantity: 18,
    unit: "token",
    occurred_at: "2026-07-24T00:00:02Z",
  },
];

// The static admin fixtures — the §47.1 surface. Secret-ref rows are metadata only (name/version); a
// connection carries a secret REF name, never a value; an api-key row is metadata only.
const ADMIN = {
  organizations: listView([{ id: "org_local", object: "organization", display_name: "Local Org" }]),
  projects: listView([{ id: "proj_local", object: "project", organization_id: "org_local", display_name: "Default Project" }]),
  "api-keys": listView([{ id: "key_admin", object: "api_key", project_id: "proj_local", scopes: ["provision", "responses"], revoked_at: null }]),
  // `provider: "provider-one"`, NOT "fake". The seed used to say "fake", which is a family an operator can
  // never select — it is the deterministic in-process adapter, deliberately absent from
  // modelbroker.Families(). A fixture row nothing could have created is a fixture that teaches the console
  // to render a shape the API cannot produce, which is the fake-is-more-generous-than-production trap.
  // `base_url` is absent here for the same reason: provider-one has an endpoint of its own and the store
  // REFUSES one on it (000049 / vetConnectionEndpoint).
  "model-connections": listView([{ id: "mc_1", object: "model_connection", provider: "provider-one", secret_ref: "provider-key" }]),
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

// --- THE POLICY DOCUMENT, AND IT IS AN ASSIGNMENT (E28 T2, plan §3.6 D9) -------------------------------
//
// THE FIDELITY THAT MATTERS HERE IS THE DESTRUCTIVE ONE. identity/store.go UpdateProjectPolicy does
// `json.Marshal(in.ConfigPolicy)` into UpdateProjectConfigPolicy — no merge, no patch — and configPolicyInput
// carries FOUR nil-able slices plus one `omitempty` string. So a request that names only `pool` stores
// `{allowed_models:null, allowed_tools:null, default_tools:null, approvers:null, pool:"…"}` and the other four
// fields are GONE. That is not a detail of the fixture: `HIL-P11` measured that an EMPTY approver list is
// PERMISSIVE, so the wire behaviour a naive form triggers silently opens the approval gate, and a fixture
// that merged would make tests/policy.spec.ts pass over the exact accident it exists to catch.
//
// The policy lives beside ADMIN.projects rather than inside it, deliberately: the LIST row stays byte-for-byte
// what it was, because DIV-SHP-002 records that row as missing `config_policy` and `created_at` against the
// real projection, and that ledger row can only be re-derived — or retired — by a sweep against a running
// stack. Closing half of a divergence blind is how a ledger stops meaning anything.
const projectPolicies = new Map([
  [
    "proj_local",
    // A project that ALREADY carries an approver list and a tool allow-list, which is the precondition the
    // crown test needs: without it "the naive form dropped `approvers`" has nothing to drop.
    { allowed_models: ["fake"], allowed_tools: ["git.push"], default_tools: ["git.push"], approvers: ["prin_release_captain"] },
  ],
]);

/**
 * projectDetail is GET /v1/projects/{project_id} as identity/store.go projectView renders it: the list row's
 * identity fields plus `config_policy` (a RawMessage with no omitempty, so it is ALWAYS a key — `null` when
 * the project has no policy row) and `created_at`.
 *
 * An unknown id is SYNTHESISED rather than 404'd, for the reason the environment detail and the agent publish
 * route already are: the sweep's arm 1 probes every pattern with a placeholder segment and reads a 404 as
 * "the table declares a route the fixture does not serve". The real route 404s an unknown or foreign id (RLS
 * makes absent and foreign indistinguishable) and no console path depends on either answer — the policy page
 * only ever puts an id in this path that GET /v1/projects just handed it.
 */
function projectDetail(id) {
  const row = ADMIN.projects.data.find((p) => p.id === id) ?? { id, object: "project", organization_id: "org_local", display_name: "Default Project" };
  return { ...row, config_policy: projectPolicies.get(id) ?? null, created_at: "2026-07-24T00:00:00Z" };
}

/**
 * assignPolicy is UpdateProjectPolicy's marshal step, verbatim in its consequences. A field absent from the
 * request is stored as `null` — that is what a nil Go slice marshals to — and `pool` is the one `omitempty`
 * field, so an empty pool is stored as an ABSENT key rather than an empty string.
 */
function assignPolicy(id, policy) {
  const slice = (v) => (Array.isArray(v) ? v.map(String) : null);
  const stored = {
    allowed_models: slice(policy.allowed_models),
    allowed_tools: slice(policy.allowed_tools),
    default_tools: slice(policy.default_tools),
    approvers: slice(policy.approvers),
  };
  if (typeof policy.pool === "string" && policy.pool !== "") stored.pool = policy.pool;
  projectPolicies.set(id, stored);
}

// THE POOLS THE POLICY CAN NAME (E24 T2's read route, E28 T1's write half). `config_policy.pool` is a pool
// ID and not a name — fleet/placement.go ResolvePool feeds it straight through as PolicyPoolID — so the
// picker's option VALUE is the id and its label is what an operator recognises. Two rows, because one is how
// a picker looks correct while offering no choice at all, and `waiting` is E28 T1's `*int64`.
//
// EVERY ROW CARRIES `waiting` OR NONE DOES, AND THAT IS A MEASUREMENT RATHER THAN A CONVENIENCE (E28 T3).
// internal/fleet/api.go's ListRunnerPools sets the pointer in a loop guarded by `a.live == nil` — the live
// gateway is a DEPLOYMENT-level thing, not a per-pool one — so the absent-key state is what a stack with no
// runner listener bound answers for its WHOLE list, and a fixture serving one pool with the key and one
// without would be teaching a wire that cannot happen. tests/fleet.spec.ts reaches that state by rewriting
// the response at the browser boundary instead, which is the same bytes and no fiction here.
//
// STATEFUL SINCE E28 T3, for the reason the environments and agents collections are: the console CREATES a
// pool (E28 T1's POST) and switches an existing one strict (its PATCH), and a static row cannot express
// either. The two seeded rows stay — one sandboxed and quiet, one unsandboxed with a queue behind it.
const RUNNER_POOLS = [
  { id: "pool_default", object: "runner_pool", name: "default", posture: "sandboxed-linux", os: "", arch: "", strict_enrollment: false, created_at: "2026-07-24T00:00:00Z", waiting: 0 },
  { id: "pool_mac", object: "runner_pool", name: "mac-pool", posture: "unsandboxed-host", os: "darwin", arch: "arm64", strict_enrollment: true, created_at: "2026-07-25T00:00:00Z", waiting: 2 },
];
let poolSeq = 0;

/** poolView is the create/patch response: the stored row WITHOUT the live queue depth — see createRunnerPool. */
const poolView = ({ waiting: _live, ...row }) => row;

// --- THE POOL KEYS (E24 T3's surface, driven from a screen by E28 T3) -----------------------------------
//
// poolKeyView's shape, verbatim: {id, object, pool_id, key_prefix, created_at} always, `key` on the CREATE
// alone, and expires_at / revoked_at / last_used_at only when set. The store keeps no value, so the mint
// response is the only thing in the world that has one — the fixture keeps none either, which is what makes
// the browser proof's "no later response carries it" a real search rather than a lucky one.
const POOL_KEYS = [
  { id: "rpk_seeded_01", object: "runner_pool_key", pool_id: "pool_mac", key_prefix: "palai-rk", created_at: "2026-07-25T00:00:00Z" },
];
let poolKeySeq = 0;
let seededKeySeq = 1;
const FIXTURE_POOL_KEY_PREFIX = "palai-rk-console-minted-";

// WHICH MACHINES A KEY ALREADY ADMITTED. The real store answers this from a join on `enrolled_via_key_id`
// (ListRunnersEnrolledViaKey); here it is a map, because what matters is the SENTENCE it makes possible
// rather than the join — revoking a key stops nothing that is already in, and a console showing a bare
// "revoked" teaches the opposite.
const KEY_ENROLMENTS = new Map([["rpk_seeded_01", ["run_active_02", "run_pending_01"]]]);

// THE REVOCABLE KEY IS RE-SEEDED AFTER IT IS REVOKED, WITH A NEW ID — the approval queue's rule applied to
// a second consumable row, and for the identical reason. A fixture its own proofs can EMPTY is a suite that
// passes once: Playwright reuses a running webServer locally (`reuseExistingServer: !CI`) AND runs every
// spec twice, once per colour-scheme project, so the second pass met the first pass's revoked key and failed
// on the fixture's state rather than on the console's behaviour.
//
// A NEW ID RATHER THAN UN-REVOKING THE OLD ONE, deliberately: "a revoked key stays revoked" has to remain
// true of the key that was revoked, because that is the property being proven. The spec selects by PREFIX.
function ensureRevocableKey() {
  if (POOL_KEYS.some((k) => k.id.startsWith("rpk_seeded_") && k.revoked_at === undefined)) return;
  seededKeySeq += 1;
  const id = `rpk_seeded_${String(seededKeySeq).padStart(2, "0")}`;
  POOL_KEYS.unshift({ id, object: "runner_pool_key", pool_id: "pool_mac", key_prefix: "palai-rk", created_at: new Date().toISOString() });
  KEY_ENROLMENTS.set(id, ["run_active_02", "run_pending_01"]);
}

// --- THE MACHINES (E24 T1/T5/T6, and the state nothing could show until E28 T3) --------------------------
//
// A COMPOSE STACK HAS NONE OF THIS AND CANNOT BE MADE TO (DIV-UI-009): a row in `runners` is written by
// fleet.Store.Register over the RUNNER PLANE — a separate mTLS listener a host agent dials with a pool key
// and a CSR — and no /v1 route puts one there. So the machine half of the fleet screen is proven here, and
// the ledger row says so rather than the skip being silent.
//
// `active_leases` IS DELIBERATELY NOT ON THESE ROWS. api/runners.go:49-59: it is on the SINGLE read because
// it is a live value, and "a page of them would be a page of separate instants presented as one". That
// absence is what forces the revoke dialog to make a second request, which tests/fleet.spec.ts asserts as a
// NETWORK call — a fixture generous enough to put the count on the listing would make that assertion pass
// over a dialog that never read anything.
//
// TWO of them are `active`, which is the condition cmd/cli/internal/stack/up.go's dispatchWorkerFleetWarning
// fires on (it counts ROWS from /v1/runners?limit=10, whatever their state, and pairs that with
// PALAI_DISPATCH_WORKERS=1).
//
// NEWEST FIRST, because ListRunners orders `created_at DESC, id DESC` (storage/queries/runners.sql:167).
// That is the same fidelity fix E25 T6 made to the agents collection after a created row landed behind a
// page boundary and the console's own picker could not offer what the operator had just made.
const RUNNERS = [
  {
    id: "run_pending_gone", object: "runner", pool_id: "pool_mac", label: "mac-mini-05",
    runner_dns: "run-pending-gone.runners.palai.local", public_key_sha256: "e6438d1b2a90c75f", state: "pending",
    os: "darwin", arch: "arm64", posture: "unsandboxed-host", capacity: 2,
    created_at: "2026-07-31T06:30:00Z", enrolled_at: "2026-07-31T06:30:00Z",
    cert_not_after: "2026-10-31T06:30:00Z", last_seen_at: "2026-07-31T06:30:00Z",
  },
  {
    id: "run_pending_noscope", object: "runner", pool_id: "pool_mac", label: "mac-mini-04",
    runner_dns: "run-pending-noscope.runners.palai.local", public_key_sha256: "a7c2019ef4b5d386", state: "pending",
    os: "darwin", arch: "arm64", posture: "unsandboxed-host", capacity: 2,
    created_at: "2026-07-31T06:20:00Z", enrolled_at: "2026-07-31T06:20:00Z",
    cert_not_after: "2026-10-31T06:20:00Z", last_seen_at: "2026-07-31T06:20:00Z",
  },
  {
    id: "run_pending_locked", object: "runner", pool_id: "pool_mac", label: "mac-mini-03",
    runner_dns: "run-pending-locked.runners.palai.local", public_key_sha256: "5d90ba3c6e18f472", state: "pending",
    os: "darwin", arch: "arm64", posture: "unsandboxed-host", capacity: 2,
    created_at: "2026-07-31T06:10:00Z", enrolled_at: "2026-07-31T06:10:00Z",
    cert_not_after: "2026-10-31T06:10:00Z", last_seen_at: "2026-07-31T06:10:00Z",
  },
  {
    id: "run_pending_01", object: "runner", pool_id: "pool_mac", label: "mac-mini-02",
    runner_dns: "run-pending-01.runners.palai.local", public_key_sha256: "c41e77b0a9d2f358", state: "pending",
    os: "darwin", arch: "arm64", posture: "unsandboxed-host", capacity: 2,
    created_at: "2026-07-31T06:00:00Z", enrolled_at: "2026-07-31T06:00:00Z",
    cert_not_after: "2026-10-31T06:00:00Z", last_seen_at: "2026-07-31T06:00:00Z",
  },
  {
    id: "run_active_02", object: "runner", pool_id: "pool_mac", label: "mac-mini-01",
    runner_dns: "run-active-02.runners.palai.local", public_key_sha256: "8b02d5fa71cc39e4", state: "active",
    os: "darwin", arch: "arm64", posture: "unsandboxed-host", capacity: 2,
    created_at: "2026-07-25T10:00:00Z", enrolled_at: "2026-07-25T10:00:00Z",
    cert_not_after: "2026-10-25T10:00:00Z", last_seen_at: "2026-07-31T07:55:00Z",
  },
  {
    id: "run_active_01", object: "runner", pool_id: "pool_default", label: "ci-linux-01",
    runner_dns: "run-active-01.runners.palai.local", public_key_sha256: "3f1a9c7e2b4d5061", state: "active",
    os: "linux", arch: "amd64", posture: "sandboxed-linux", capacity: 1,
    created_at: "2026-07-24T09:00:00Z", enrolled_at: "2026-07-24T09:00:00Z",
    cert_not_after: "2026-10-24T09:00:00Z",
    // DELIBERATELY STALE. The screen must say this means "has not authenticated since" and not "is down".
    last_seen_at: "2026-07-20T04:12:00Z",
  },
];

// THE ADMITTABLE MACHINE IS RE-SEEDED AFTER IT IS ADMITTED, with a NEW id — ensureRevocableKey's argument
// again, and the approval queue's before that. An admitted machine leaves the waiting room for good, so a
// second colour-scheme pass (or a locally reused server) met an empty room and failed on the fixture's own
// state. `run_pending_locked` / `_noscope` / `_gone` are NOT re-seeded and never leave: their admissions are
// always refused, which is what makes them stable subjects for the typed-refusal legs.
let waitingSeq = 0;
function ensureWaitingMachine() {
  if (RUNNERS.some((r) => r.state === "pending" && r.id.startsWith("run_waiting_"))) return;
  waitingSeq += 1;
  const n = String(waitingSeq).padStart(2, "0");
  RUNNERS.unshift({
    id: `run_waiting_${n}`, object: "runner", pool_id: "pool_mac", label: `mac-mini-w${n}`,
    runner_dns: `run-waiting-${n}.runners.palai.local`, public_key_sha256: `9c${n}4e0b7d1a635f`, state: "pending",
    os: "darwin", arch: "arm64", posture: "unsandboxed-host", capacity: 2,
    created_at: new Date().toISOString(), enrolled_at: new Date().toISOString(),
    cert_not_after: "2026-10-31T12:00:00Z", last_seen_at: new Date().toISOString(),
  });
}

// THE LIVE LEASE COUNT, KEPT OFF THE ROWS ABOVE so it can only be served by the reads that actually have it.
//
// A DEFAULT OF ZERO RATHER THAN AN ABSENCE, and that is fidelity rather than laziness: fleet/api.go's
// `decorate` sets the pointer for every machine whenever `a.live != nil` and RunnerGateway.RunnerActiveLeases
// answers 0 for a machine it holds no sessions for. So "the gateway could not be asked" is a DEPLOYMENT-wide
// state (no runner listener bound), never a per-machine one, and a fixture that omitted the key for one
// machine would be teaching a wire that cannot happen. tests/fleet.spec.ts reaches the absent-key state by
// rewriting the response at the browser boundary, which is the same bytes and no fiction here.
const ACTIVE_LEASES = new Map([["run_active_02", 2]]);
const leasesFor = (id) => ACTIVE_LEASES.get(id) ?? 0;

// THE THREE TYPED REFUSALS OF AN ADMISSION, AND THEY ARE SYNTHESIS — named as such, exactly like the
// per-row canned refusals on the approval queue (E25 T5). The real route keys them to the CALLER: 403
// `insufficient_scope` when the key lacks `approve` (api/runners.go:540-543), 403 `approver_not_authorized`
// when the project's approver list excludes the principal the key resolves to (:549-555), and 404 for
// unknown / foreign / not-admissible, which are deliberately indistinguishable (:556-560). This console
// presents ONE key, so it cannot reach the first two by being itself; keying them to the machine is how the
// three sentences get rendered at all. The refusal ORDER is the real handler's: capability, then policy,
// then resolution.
const ADMISSION_REFUSALS = new Map([
  ["run_pending_noscope", [403, "insufficient_scope", "this API key lacks the approve capability"]],
  ["run_pending_locked", [403, "approver_not_authorized", "this project's approver list does not include the principal this key resolves to"]],
  ["run_pending_gone", [404, "not_found", "no such runner awaiting approval in this project"]],
]);

/** poolKeyView is api/runners.go's, including the field NAMED for what it means on a revoke. */
function poolKeyView(row, { key, enrolled } = {}) {
  return {
    id: row.id, object: "runner_pool_key", pool_id: row.pool_id, key_prefix: row.key_prefix,
    created_at: row.created_at,
    ...(key === undefined ? {} : { key }),
    ...(row.expires_at === undefined ? {} : { expires_at: row.expires_at }),
    ...(row.revoked_at === undefined ? {} : { revoked_at: row.revoked_at }),
    ...(row.last_used_at === undefined ? {} : { last_used_at: row.last_used_at }),
    ...(enrolled === undefined ? {} : { enrolled_runners_still_running: enrolled }),
  };
}

/**
 * runnerView adds `active_leases`, which the LISTING never carries and these reads always do — fleet/api.go
 * shadows GetRunner, SetRunnerState and ApproveRunner with `decorate` and leaves ListRunners alone.
 */
function runnerView(row) {
  return { ...row, active_leases: leasesFor(row.id) };
}

/** findRunner is the lookup every machine route shares; `undefined` for an id the fixture does not hold. */
const findRunner = (id) => RUNNERS.find((r) => r.id === id);

// --- API KEYS, MINTED ONCE (E28 T2) ---------------------------------------------------------------------
//
// identity/store.go apiKeyView carries `key` with `omitempty` and sets it ONLY on the create response; every
// read leaves it empty, so a listing can never disclose a secret. The fixture mirrors that exactly, because
// the browser proof is "the value is in no later response body" and a generous fixture would make that
// assertion pass for the wrong reason — or fail honestly and be blamed on the console.
//
// The minted row is appended to ADMIN["api-keys"].data — the array `adminList` already serves — and it
// carries the SAME key set the seeded row carries, so the collection keeps one list shape and DIV-SHP-003
// keeps its exact meaning. The CREATE response is a different shape on the real side too: `key` is set there
// and nowhere else.
let apiKeySeq = 0;
let projectSeq = 0;
const FIXTURE_KEY_PREFIX = "palai-sk-console-minted-";
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

// --- THE SESSION COLLECTION (E29) -------------------------------------------------------------------
//
// GET /v1/sessions and GET /v1/sessions/{id} are registered by the real router (api/router.go:321-322) and
// this fixture served NEITHER, which is why the Sessions screen could not be built against it. The shape
// below is migration 000048's projection key for key — id, object, status, created_at, organization_id,
// project_id, name, name_source, agents, input_tokens, output_tokens, first_activity_at, last_activity_at,
// duration_ms — because the conformance sweep diffs this against the running real one and a generous fixture
// is how a console ends up rendering a field the API does not send.
//
// TWENTY-FOUR ROWS, for PAGE_LIMIT's reason: the twenty-first row is the one truncation is only observable
// past, and a Sessions list is the collection an operator meets with hundreds in it.
//
// THE TIMESTAMPS ARE RELATIVE TO PROCESS START, and that is the one deliberate departure from the fixture's
// usual fixed dates. The screen's Created column renders "10 hours ago", so a row stamped 2026-07-24 would
// read as "8 days ago" on the day it was written and "three months ago" a quarter later — a fixture whose
// rendered value rots. Anchoring to boot makes the relative stamps stable for as long as the assertion is
// about their SHAPE, which is what it is about.
const SESSION_EPOCH = Date.now();
const ses = (n) => `ses_${String(n).padStart(2, "0").repeat(16).slice(0, 32)}`;

// ONE SESSION WHOSE RUN WAS REFUSED, and it exists so the transcript's ERROR PILL is shipped code rather
// than dead code. streamEvents forks on the approval command: `denied` writes approval.denied.v1 and
// run.canceled.v1 and ends the stream, which is a journal with two FAILING frames in it. Every other
// fixture session replays a completed run, so without this row the pill would render on nothing and no test
// could tell the difference between "correct" and "never reached".
//
// It is found by its NAME rather than by its index, because the create route unshifts rows and an index is a
// handle that moves. A compose stack cannot produce this journal at all (DIV-UI-002: a real run journals six
// types and canceled is not one), which is why the spec that drives it cites that row and skips there.
const REFUSED_INDEX = 1;
const REFUSED_NAME = "Refused release push";

// The fixture's session bodies. `name_source` covers all three of the enum's values on purpose: `derived` is
// the common case (a cut of the first prompt, which nobody chose), `operator` is a label a human typed, and
// `none` is a session with neither — the row whose name cell must say so rather than render blank.
const SESSIONS = Array.from({ length: 24 }, (_, i) => {
  const createdAt = SESSION_EPOCH - (i + 1) * 3_600_000;
  const ran = i % 7 !== 6; // one session in seven never ran: null span, null duration, zero tokens
  const durationMs = ran ? 1_600 + i * 940 : null;
  const named = i % 5 === 0 || i === REFUSED_INDEX;
  const unnamed = i === 3;
  return {
    id: ses(i + 1),
    object: "session",
    status: i % 6 === 5 ? "closed" : i % 6 === 4 ? "paused" : "active",
    created_at: new Date(createdAt).toISOString(),
    organization_id: "org_local",
    project_id: "prj_local",
    name: i === REFUSED_INDEX ? REFUSED_NAME : named ? `Release rehearsal ${String(i + 1)}` : unnamed ? "" : "Push the release branch.",
    name_source: named ? "operator" : unnamed ? "none" : "derived",
    agents: i % 4 === 0 ? ["release-bot"] : i % 9 === 3 ? ["release-bot", "reviewer"] : [],
    input_tokens: ran ? (i === 1 ? 22_500 : 44 + i * 13) : 0,
    output_tokens: ran ? 111 + i * 7 : 0,
    // THE SPAN IS OMITTED, NOT NULLED, ON A SESSION THAT NEVER RAN — and that was a fixture bug found by
    // hand-diffing this collection against the running stack on 2026-07-31, because the conformance sweep
    // could not run (its `before` hook drives a real run to terminal and this stack leaves runs queued).
    // MEASURED: `GET /v1/sessions/{id}` for a session created through the API answers eleven keys —
    // agents, created_at, id, input_tokens, name, name_source, object, organization_id, output_tokens,
    // project_id, status — and first_activity_at / last_activity_at / duration_ms are ABSENT, because the
    // Go projection carries them omitempty. A fixture serving `null` teaches a shape the API never sends,
    // which is the DIV-SHP-004 defect exactly: the console would be written against a key that is not there.
    ...(ran
      ? {
          first_activity_at: new Date(createdAt + 1).toISOString(),
          last_activity_at: new Date(createdAt + durationMs).toISOString(),
          duration_ms: durationMs,
        }
      : {}),
  };
});

// EVERY FIXTURE SESSION IS PRE-APPROVED IN THE STREAM'S OWN STATE MAP, and without this line the transcript
// screen would hang. streamEvents gates the frame after approval.requested.v1 on an approve command; a
// session id it has never seen resolves to {approved:false} and the pump waits forever. These sessions are
// HISTORY — their run already happened — so the journal replays to its terminal frame the way a finished
// run's does on the real API.
for (const row of SESSIONS) sessions.set(row.id, { approved: true, denied: false, model: MODEL });
sessions.set(SESSIONS[REFUSED_INDEX].id, { approved: false, denied: true, model: MODEL });

// THE PRISTINE COPY, AND WHY IT EXISTS: this fixture is ONE process shared by BOTH colour-scheme projects,
// and PATCH /v1/sessions/{id} renames a row in place. So the light project renamed sessions and the dark
// project inherited the renames — 16 of sessions.spec.ts's assertions failed in the second project and
// passed (22/22) when that project ran alone. Measured 2026-08-01.
//
// That is not a flake: it is one project's writes leaking into another project's reads through a fixture
// with no boundary between them. The same shape already cost mcp-tools.spec.ts three dark-only failures
// when the light project published trev_console_0001.
const SESSIONS_PRISTINE = structuredClone(SESSIONS);

// AND THE SAME FOR ADMIN, because sessions were not the only collection a spec writes. /policy mints API
// keys and revokes them (`ADMIN["api-keys"].data.push`, `row.revoked_at = …`), so by the tenth test in
// policy.spec.ts that list is longer and differently shaped than the fixture authored — which is why the
// revoke dialog's focus trap failed there and passed when the test ran alone. Measured 2026-08-01: a probe
// inside a solo run showed the trap working exactly as designed (two controls, focus contained), so the
// defect was never in the dialog.
const ADMIN_PRISTINE = structuredClone(ADMIN);

/** findSession is the item read AND the write path's lookup — one place decides what "no such session" means. */
const findSession = (id) => SESSIONS.find((s) => s.id === id);

/**
 * filterSessions applies the two filters beginList actually parses for this kind — ?status= and the
 * created_at bounds (pagination.go statusFilterKinds includes "sessions"; created_after/created_before are
 * RFC3339). It is a real narrowing rather than a courtesy: the console's Status and Created controls send
 * these parameters, so a fixture that ignored them would let a dropdown that changes nothing look like one
 * that works.
 */
function filterSessions(url) {
  const status = url.searchParams.get("status");
  const after = url.searchParams.get("created_after");
  const before = url.searchParams.get("created_before");
  return SESSIONS.filter((row) => {
    if (status !== null && status !== "" && row.status !== status) return false;
    if (after !== null && after !== "" && Date.parse(row.created_at) < Date.parse(after)) return false;
    if (before !== null && before !== "" && Date.parse(row.created_at) > Date.parse(before)) return false;
    return true;
  });
}

let sessionSeq = 0;

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

/**
 * parseBody decodes a request body, tolerating garbage — the real routes answer 400 for a bad body and the
 * sweep's arm 1 probes every POST with "{}", so a throw here would fail that arm instead of the route.
 */
function parseBody(raw) {
  try {
    const value = JSON.parse(raw || "{}");
    return value === null || typeof value !== "object" ? {} : value;
  } catch {
    return {};
  }
}

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

  // --- THE POLICY DOCUMENT AND THE KEYS (E28 T2), in api/router.go:165-172's order. ---
  {
    method: "POST",
    pattern: "/v1/projects",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        // `display_name` is the ONLY field CreateProject accepts (strictDecode), and it is not required —
        // identity/store.go inserts whatever it was given, empty included.
        if (Object.keys(body).some((k) => k !== "display_name")) {
          return sendProblem(response, 400, "invalid_request", "the request carries an unsupported field");
        }
        projectSeq += 1;
        const id = `prj_console_${String(projectSeq).padStart(4, "0")}`;
        const row = { id, object: "project", organization_id: "org_local", display_name: typeof body.display_name === "string" ? body.display_name : "" };
        // UNSHIFT, for the reason the agents route does it: a spec that creates a project and then picks it
        // must find it, and a fixture that appends teaches an ordering the real list does not have.
        ADMIN.projects.data.unshift(row);
        // The CREATE response carries `config_policy: null` explicitly — identity/store.go builds the view
        // with `json.RawMessage("null")` rather than leaving the field absent.
        sendJSON(response, 201, { ...row, config_policy: null });
      }),
  },
  {
    method: "GET",
    pattern: "/v1/projects/{project_id}",
    handle: (_req, res, { project_id: id }) => sendJSON(res, 200, projectDetail(id)),
  },
  {
    method: "PATCH",
    pattern: "/v1/projects/{project_id}",
    handle: (request, response, { project_id: id }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        // strictDecode's two refusals, in identity/store.go's own order: an unknown field anywhere is a 400
        // (DisallowUnknownFields), and an absent config_policy is the missing-field 400. Both matter to the
        // console: the field set is the whole contract this page writes against, and a form that invented a
        // sixth knob would be refused by the real API and must be refused here.
        const known = new Set(["allowed_models", "allowed_tools", "default_tools", "approvers", "pool"]);
        const policy = body.config_policy;
        if (Object.keys(body).some((k) => k !== "config_policy")) {
          return sendProblem(response, 400, "invalid_request", "the request carries an unsupported field");
        }
        if (policy === undefined || policy === null || typeof policy !== "object") {
          return sendProblem(response, 400, "invalid_request", "config_policy is required");
        }
        if (Object.keys(policy).some((k) => !known.has(k))) {
          return sendProblem(response, 400, "invalid_request", "the request carries an unsupported field");
        }
        assignPolicy(id, policy);
        // The real route re-READS the project after the write (readProject), so the response IS the stored
        // document rather than an echo of the request. The console's read-back leg depends on that being
        // true here too, and an echo would make it vacuous.
        sendJSON(response, 200, projectDetail(id));
      }),
  },
  {
    method: "POST",
    pattern: "/v1/api-keys",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const projectID = typeof body.project_id === "string" ? body.project_id : "";
        // `project_id is required` is the real refusal (ProvisionResult.MissingField -> 400).
        if (projectID === "") return sendProblem(response, 400, "invalid_request", "project_id is required");
        apiKeySeq += 1;
        const id = `key_console_${String(apiKeySeq).padStart(4, "0")}`;
        const scopes = Array.isArray(body.scopes) ? body.scopes.map(String) : [];
        // A DISTINCTIVE, SINGLE-USE VALUE. The browser proof scans megabytes of DOM, storage and minified
        // JavaScript for exactly these bytes, so a value that could match by accident would make the sweep
        // meaningless. It authenticates nothing: this fixture accepts any non-empty bearer.
        const key = `${FIXTURE_KEY_PREFIX}${id}-9e4c17b0f3a2`;
        ADMIN["api-keys"].data.push({ id, object: "api_key", project_id: projectID, scopes, revoked_at: null });
        sendJSON(response, 201, {
          id,
          object: "api_key",
          organization_id: "org_local",
          project_id: projectID,
          principal_id: `prin_console_${String(apiKeySeq).padStart(4, "0")}`,
          scopes,
          key,
        });
      }),
  },
  {
    // The single-key read (api/router.go:171). It is what lets a spec assert "Escape revoked nothing" over
    // the API rather than over a rendered row — a row that simply failed to refresh looks identical.
    method: "GET",
    pattern: "/v1/api-keys/{key_id}",
    handle: (_req, response, { key_id: id }) => {
      const row = ADMIN["api-keys"].data.find((k) => k.id === id);
      // Synthesised on an unknown id, like the sibling routes and for the same arm-1 reason.
      sendJSON(response, 200, row ?? { id, object: "api_key", project_id: "proj_local", scopes: [], revoked_at: null });
    },
  },
  {
    method: "POST",
    pattern: "/v1/api-keys/{key_id}/revoke",
    handle: (_req, response, { key_id: id }) => {
      const row = ADMIN["api-keys"].data.find((k) => k.id === id);
      // SYNTHESISED on an unknown id, like the environment detail route and for the same reason (arm 1's
      // placeholder probe). The real route 404s; no console path depends on either, because the revoke
      // button only exists on a row the list returned.
      if (row !== undefined) row.revoked_at = "2026-07-31T00:00:00Z";
      // NO `key` ON THIS RESPONSE, and that is the point of the leg that reads it: apiKeyView carries the
      // plaintext with omitempty and sets it on the create alone.
      sendJSON(response, 200, { id, object: "api_key", project_id: "proj_local", scopes: row?.scopes ?? [], revoked_at: "2026-07-31T00:00:00Z" });
    },
  },
  // --- THE FLEET (E24 T1/T2/T3/T5/T6's routes + E28 T1's write half), in api/router.go's order ----------
  //
  // The pools a policy's `pool` can name (E24 T2). renderPage's envelope, so the console's Panel/Picker meet
  // the same {data, has_more} shape here as on a real stack.
  { method: "GET", pattern: "/v1/runner-pools", handle: (request, response) => pageSlice(RUNNER_POOLS, requestURL(request), response) },
  {
    // E28 T1's birth path. The refusals are the route's own, in its order: a blank name is a 400, a posture
    // outside 000045's CHECK is a 400 *from the route* (the constraint is the last defence, not the first),
    // and a duplicate name is a 409 rather than the 500 an unhandled unique violation would be.
    method: "POST",
    pattern: "/v1/runner-pools",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const name = typeof body.name === "string" ? body.name.trim() : "";
        if (name === "") return sendProblem(response, 400, "invalid_request", "name is required and must not be blank");
        if (body.posture !== "sandboxed-linux" && body.posture !== "unsandboxed-host") {
          return sendProblem(response, 400, "invalid_request", "posture must be one of sandboxed-linux, unsandboxed-host");
        }
        if (RUNNER_POOLS.some((p) => p.name === name)) {
          return sendProblem(response, 409, "already_exists", "this project already has a runner pool with that name");
        }
        poolSeq += 1;
        const pool = {
          id: `pool_console_${String(poolSeq).padStart(4, "0")}`, object: "runner_pool", name,
          posture: body.posture, os: typeof body.os === "string" ? body.os : "",
          arch: typeof body.arch === "string" ? body.arch : "",
          strict_enrollment: body.strict_enrollment === true,
          created_at: new Date().toISOString(),
          // The live gateway answers for the WHOLE list or for none of it, so a new pool joins the list
          // carrying the same key its siblings carry — see RUNNER_POOLS.
          waiting: 0,
        };
        // UNSHIFT, NOT PUSH: ListRunnerPools orders `created_at DESC, id DESC` (storage/queries/runners.sql),
        // so a freshly created pool is always on page ONE of a real stack. Appending would put it behind the
        // page limit on any deployment with twenty pools, and the picker below would not offer the pool the
        // operator had just made — the exact defect E25 T6 found on the agents collection.
        RUNNER_POOLS.unshift(pool);
        // THE SAME RENDERER AS THE LISTING — AND ONE KEY FEWER, WHICH WAS MEASURED RATHER THAN ASSUMED.
        // `runnerPoolView` is shared, but only ListRunnerPools decorates its items with the live gateway's
        // queue depth (internal/fleet/api.go), so a CREATE response carries no `waiting` at all. Verified on a
        // running compose stack (2026-07-31): POST answers 201 with {arch, created_at, id, name, object, os,
        // posture, strict_enrollment} and the listing's row adds `waiting`. A fixture that echoed the stored
        // row here would teach a create response the real one does not send.
        sendJSON(response, 201, poolView(pool));
      }),
  },
  {
    // E28 T1's second route: ONE field. `posture` is not patchable because a machine INHERITS its pool's
    // posture at enrolment, so moving a populated pool would retroactively change what the machines in it
    // ARE — the fixture refuses an unknown field for the same reason the route's DisallowUnknownFields does.
    method: "PATCH",
    pattern: "/v1/runner-pools/{pool_id}",
    handle: (request, response, { pool_id: id }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        if (typeof body.strict_enrollment !== "boolean") {
          return sendProblem(response, 400, "invalid_request",
            "strict_enrollment is required; a pool's posture is fixed at creation because its machines inherit it");
        }
        const pool = RUNNER_POOLS.find((p) => p.id === id);
        // A REAL 404 HERE, unlike the id-bearing routes below, and it is safe for the sweep's arm 1: that arm
        // probes a PATCH with NO body, which this route refuses at the 400 above before any lookup happens.
        if (pool === undefined) return sendProblem(response, 404, "not_found", "no such runner pool in this project");
        pool.strict_enrollment = body.strict_enrollment;
        // No `waiting` here either, and for the same reason: SetRunnerPoolStrictEnrollment is not decorated.
        sendJSON(response, 200, poolView(pool));
      }),
  },
  {
    // Metadata only — never a value, never a digest. An unknown pool yields an EMPTY list rather than a 404,
    // which is the real route's shape too: ListRunnerPoolKeys reports no found flag.
    method: "GET",
    pattern: "/v1/runner-pools/{pool_id}/keys",
    handle: (_req, response, { pool_id: id }) => {
      ensureRevocableKey();
      sendJSON(response, 200, { object: "list", data: POOL_KEYS.filter((k) => k.pool_id === id).map((k) => poolKeyView(k)) });
    },
  },
  {
    // THE ONE TIME A VALUE EXISTS. poolKeyView(item, true) is used here and on no other call site.
    //
    // SYNTHESISED ON AN UNKNOWN POOL, like the environment detail and agent publish routes and for the same
    // arm-1 reason: the sweep probes every pattern with a placeholder segment and reads a 404 as "the table
    // declares a route the fixture does not serve". The real route 404s an unknown or foreign pool and no
    // console path depends on either answer — the mint button only ever names a pool the list returned.
    method: "POST",
    pattern: "/v1/runner-pools/{pool_id}/keys",
    handle: (request, response, { pool_id: id }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        poolKeySeq += 1;
        const keyID = `rpk_console_${String(poolKeySeq).padStart(4, "0")}`;
        // A DISTINCTIVE, SINGLE-USE VALUE, for the reason the minted API key's is: the browser proof scans
        // the reloaded document and every listing for exactly these bytes, so a value that could match by
        // accident would make the search meaningless. It authenticates nothing.
        const value = `${FIXTURE_POOL_KEY_PREFIX}${keyID}-6d1f84ba0c37`;
        const row = {
          id: keyID, object: "runner_pool_key", pool_id: id, key_prefix: value.slice(0, 8),
          created_at: new Date().toISOString(),
          ...(typeof body.expires_at === "string" && body.expires_at !== "" ? { expires_at: body.expires_at } : {}),
        };
        // Newest first, like the pools and the machines — ListRunnerPoolKeys is `created_at DESC, id DESC`.
        POOL_KEYS.unshift(row);
        sendJSON(response, 201, poolKeyView(row, { key: value }));
      }),
  },
  {
    // A REVOKE ANSWERS WITH THE MACHINES IT DOES NOT STOP, and the field is named for what it means. An
    // operator not shown them reads "revoked" as "removed" and believes one call decommissioned a fleet.
    // Synthesised on an unknown key, for the arm-1 reason above.
    method: "POST",
    pattern: "/v1/runner-pool-keys/{key_id}/revoke",
    handle: (_req, response, { key_id: id }) => {
      const row = POOL_KEYS.find((k) => k.id === id) ?? { id, object: "runner_pool_key", pool_id: "pool_mac", key_prefix: "palai-rk", created_at: "2026-07-25T00:00:00Z" };
      row.revoked_at = "2026-07-31T00:00:00Z";
      const enrolled = (KEY_ENROLMENTS.get(id) ?? [])
        .map(findRunner)
        .filter((r) => r !== undefined)
        .map((r) => ({ id: r.id, label: r.label, runner_dns: r.runner_dns, state: r.state, pool_id: r.pool_id, enrolled_at: r.enrolled_at }));
      sendJSON(response, 200, poolKeyView(row, { enrolled }));
    },
  },
  {
    method: "GET",
    pattern: "/v1/runners",
    handle: (request, response) => {
      ensureWaitingMachine();
      pageSlice(RUNNERS, requestURL(request), response);
    },
  },
  {
    // THE SINGLE READ, and the ONLY place `active_leases` exists. Synthesised on an unknown id (arm 1).
    method: "GET",
    pattern: "/v1/runners/{runner_id}",
    handle: (_req, response, { runner_id: id }) => {
      const row = findRunner(id) ?? {
        id, object: "runner", pool_id: "pool_default", label: "", runner_dns: "", public_key_sha256: "",
        state: "active", os: "linux", arch: "amd64", posture: "sandboxed-linux", capacity: 1,
        created_at: "2026-07-24T00:00:00Z",
      };
      sendJSON(response, 200, runnerView(row));
    },
  },
  ...["cordon", "resume", "revoke"].map((action) => ({
    // THE ACTION IS BOUND AT REGISTRATION, exactly as api/runners.go binds it: three patterns, one closure
    // each, so there is no caller-supplied string to validate. A revoke is ONE-WAY here too — the state it
    // writes is terminal and neither of the other two brings it back.
    method: "POST",
    pattern: `/v1/runners/{runner_id}/${action}`,
    handle: (_req, response, { runner_id: id }) => {
      const row = findRunner(id);
      if (row === undefined) {
        return sendJSON(response, 200, runnerView({ id, object: "runner", pool_id: "pool_default", state: action === "resume" ? "active" : `${action}ed`, created_at: "2026-07-24T00:00:00Z" }));
      }
      // A REVOKED machine cannot move, and a PENDING one cannot be cordoned or resumed — SetState's own
      // shape: a cordon would erase the fact that nobody had admitted it, and the resume after it would
      // then look legitimate. Only `revoke` reaches a machine in the waiting room, as its refusal.
      if (row.state === "revoked" || (row.state === "pending" && action !== "revoke")) {
        return sendProblem(response, 404, "not_found", "no such runner in this project");
      }
      row.state = action === "resume" ? "active" : action === "cordon" ? "cordoned" : "revoked";
      sendJSON(response, 200, runnerView(row));
    },
  })),
  {
    // THE WAITING ROOM'S DOOR (E24 T6), gated on `approve` and not on `provision`. The three refusals are
    // SYNTHESIS keyed to the machine — see ADMISSION_REFUSALS for why they cannot be keyed to the caller.
    method: "POST",
    pattern: "/v1/runners/{runner_id}/approve",
    handle: (_req, response, { runner_id: id }) => {
      const refusal = ADMISSION_REFUSALS.get(id);
      if (refusal !== undefined) return sendProblem(response, refusal[0], refusal[1], refusal[2]);
      const row = findRunner(id);
      // Synthesised on an unknown id (arm 1). The real route answers 404 and the console only ever names a
      // machine the waiting room just listed.
      if (row === undefined) return sendJSON(response, 200, runnerView({ id, object: "runner", pool_id: "pool_mac", state: "active", created_at: "2026-07-31T00:00:00Z" }));
      if (row.state !== "pending") return sendProblem(response, 404, "not_found", "no such runner awaiting approval in this project");
      row.state = "active";
      sendJSON(response, 200, runnerView(row));
    },
  },

  { method: "GET", pattern: "/v1/model-connections", handle: adminList("model-connections") },

  // --- THE PROVIDER WIRING WRITE PATH (E29). ---
  //
  // It mirrors internal/store/model_routes.go's refusals rather than accepting whatever arrives, because a
  // fixture more generous than production makes the console code against a shape that does not exist — this
  // tree's own rule, and it has been paid for. The three refusals below are the three the real store makes.
  {
    method: "POST",
    pattern: "/v1/model-connections",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const provider = typeof body.provider === "string" ? body.provider : "";
        const secretRef = typeof body.secret_ref === "string" ? body.secret_ref : "";
        const baseURL = typeof body.base_url === "string" ? body.base_url : "";
        // A family the broker cannot dial (store: modelbroker.LookupFamily).
        if (!["provider-one", "provider-two", "openai-compatible"].includes(provider)) {
          return sendProblem(response, 400, "invalid_request", "provider must be one of provider-one, provider-two, openai-compatible");
        }
        if (secretRef === "") return sendProblem(response, 400, "invalid_request", "secret_ref is required");
        // The endpoint rule: required by the custom family, refused on the others.
        if (provider === "openai-compatible" && baseURL === "") {
          return sendProblem(response, 400, "invalid_request", "base_url is required for the custom family");
        }
        if (provider !== "openai-compatible" && baseURL !== "") {
          return sendProblem(response, 400, "invalid_request", "base_url is refused on a family with its own endpoint");
        }
        connectionSeq += 1;
        const row = { id: `mconn_console_${String(connectionSeq).padStart(4, "0")}`, object: "model_connection", provider, secret_ref: secretRef };
        if (baseURL !== "") row.base_url = baseURL;
        // UNSHIFT: ListModelConnections orders by id, and a console-minted id sorts after the seed's `mc_1`
        // on a real stack too — but the row an operator just made must be findable without scrolling, and
        // the panel renders in received order.
        ADMIN["model-connections"].data.unshift(row);
        sendJSON(response, 201, row);
      }),
  },
  {
    // The credential probe. The fake CANNOT make a real network call, so it answers the one outcome that is
    // honest for a fixture: `not_probed`, with the sentence saying nothing was checked. That is deliberate
    // and it is the stronger fixture — a spec that passes on a scripted "credential_accepted" would be
    // asserting the console can render a green, which is not the property worth pinning. The REAL
    // classification is measured in Go against an httptest endpoint
    // (internal/store TestVerifyModelConnectionMakesARealRequest).
    // THE PATTERN IS A STRING WITH A `{name}` WILDCARD, NOT A RegExp. compile() below calls
    // `pattern.replace`, so a RegExp here throws at module load and the fixture never binds its port —
    // which is not a failing spec but every console spec failing to start, reported as a webServer error.
    // The wildcard's name is what reaches the handler as `groups`.
    method: "POST",
    pattern: "/v1/model-connections/{connection_id}/verify",
    handle: (request, response, groups) => {
      const id = groups.connection_id ?? "";
      sendJSON(response, 200, {
        object: "model_connection_verification",
        connection_id: id,
        provider: "provider-one",
        outcome: "not_probed",
        endpoint: "https://api.openai.com/v1/models",
        detail: "this fixture makes no network call, so NOTHING was checked",
        checked: "that the endpoint answered and did not reject this credential — NOT that the route's model id exists, nor that a quota remains",
      });
    },
  },

  { method: "GET", pattern: "/v1/model-routes", handle: adminList("model-routes") },
  { method: "GET", pattern: "/v1/secret-refs", handle: adminList("secret-refs") },
  {
    // WRITING THE CREDENTIAL — the first half of the console's two-call connection flow (E29). The
    // connection surface stores a REF, so a screen that takes a key must seal it here first and then name
    // it; without this route the console could only bind refs somebody had already created by CLI, which is
    // the gap the whole screen exists to close.
    //
    // The refusals and the projection are identity/secrets.go's: `{name, value}` in, and metadata out —
    // `{name, object, version, updated_at}` per secretRefView, with NO value on the response. That absence
    // is the fixture's most important property: secret-never-returns.spec.ts and provider-wiring.spec.ts
    // both sweep every response body for the sentinel they typed, and a fixture that echoed `value` would
    // turn a real leak assertion into a fixture artefact.
    //
    // A REPEAT NAME VERSIONS RATHER THAN REPLACING, because secret_refs is append-only and the real store
    // computes the next version in putVersion(). A fixture that answered `version: 1` forever would let a
    // rotation display bug ship.
    method: "POST",
    pattern: "/v1/secret-refs",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const name = typeof body.name === "string" ? body.name : "";
        const value = typeof body.value === "string" ? body.value : "";
        if (name === "") return sendProblem(response, 400, "invalid_request", "name is required");
        if (value === "") return sendProblem(response, 400, "invalid_request", "value is required");
        const rows = ADMIN["secret-refs"].data;
        const existing = rows.find((r) => r.name === name);
        const updatedAt = new Date().toISOString();
        if (existing !== undefined) {
          existing.version += 1;
          existing.updated_at = updatedAt;
          return sendJSON(response, 201, { name, object: "secret_ref", version: existing.version, updated_at: updatedAt });
        }
        const row = { name, object: "secret_ref", version: 1, updated_at: updatedAt };
        rows.unshift(row);
        sendJSON(response, 201, row);
      }),
  },
  { method: "GET", pattern: "/v1/knowledge-bases", handle: adminList("knowledge-bases") },
  { method: "GET", pattern: "/v1/agents", handle: (request, response) => pageSlice(AGENTS, requestURL(request), response) },

  // --- THE AGENT LINEAGE (E25 T6). Create → revise → publish, in api/router.go:52-58's order. ---
  {
    method: "POST",
    pattern: "/v1/agents",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const name = typeof body.name === "string" ? body.name.trim() : "";
        // `name is required` is the real refusal (store/agents.go MissingName → 400).
        if (name === "") return sendProblem(response, 400, "invalid_request", "name is required");
        agentSeq += 1;
        const agent = { id: `agt_console_${String(agentSeq).padStart(4, "0")}`, object: "agent", name };
        // UNSHIFT, NOT PUSH, and it is a fidelity fix rather than a convenience. ListAgentProfiles orders
        // `created_at DESC, id DESC` (storage/queries/agents.sql:33) — NEWEST FIRST — so on a real stack a
        // freshly created agent is always on page ONE. Appending put it at position 22 of a 21-row fixture,
        // behind defaultPageLimit, and the console's own picker then could not offer the agent the operator
        // had just made. Found by the console's create-then-select leg; the sweep could never have found it,
        // because ORDER is not a shape and its item arm compares keys.
        AGENTS.unshift(agent);
        revisions.set(agent.id, []);
        sendJSON(response, 201, agent);
      }),
  },
  {
    // GET /v1/agents/{agent_id} — mounted by api/router.go:56 since E13 T4 and never served here, because
    // nothing in the console addressed one agent until app/agents/[id] existed. The projection is
    // store/agents.go GetAgentProfile's, field for field: {id, object, name} and nothing else. There is no
    // created_at on this wire — the column storage/queries/agents.sql selects rides api.ListRow to mint a
    // cursor and renderPage never serialises it — which is why the console's agent screens carry no
    // timestamp anywhere (lib/agents.ts holds the measurement).
    //
    // SYNTHESISED ON AN UNKNOWN ID, like the environment-detail, binding and publish routes above and for
    // the same reason: the conformance sweep's arm 1 probes every declared pattern with a placeholder
    // segment, and a 404 there reads as "the table declares a route the fixture does not serve". The real
    // route answers 404 for an unknown or foreign profile (store/agents.go NotFound), and no console path
    // depends on either answer — every id this console puts in this path came out of a list it just read.
    method: "GET",
    pattern: "/v1/agents/{agent_id}",
    handle: (_req, res, { agent_id: id }) => {
      const row = AGENTS.find((a) => a.id === id);
      sendJSON(res, 200, row ?? { id, object: "agent", name: `synthesised-${id}` });
    },
  },
  {
    method: "GET",
    pattern: "/v1/agents/{agent_id}/revisions",
    // {data, has_more} AND NOTHING ELSE — renderPage's envelope over a page that ends the collection, which
    // is what the real route answers for any lineage shorter than defaultPageLimit (every lineage there is).
    // THIS CLOSED DIV-SHP-005: the fixture used to serve both cursor keys as explicit nulls here, and that
    // envelope difference made the conformance sweep skip the ITEM comparison for this route ("an envelope
    // difference subsumes the item comparison") — which is how the row shape stayed wrong for months. It said
    // `published: true` where the real projection says `status: "published"`, and it was missing agent_id,
    // revision_number, mcp_connections, environment and instructions. GET /v1/agents keeps pageSlice, because
    // its twenty-one rows are what makes truncation observable (DIV-UI-005) and DIV-SHP-004 records that.
    handle: (_req, res, { agent_id: id }) => sendJSON(res, 200, { data: (revisions.get(id) ?? []).map((rev) => revisionRow(id, rev)), has_more: false }),
  },
  {
    method: "POST",
    pattern: "/v1/agents/{agent_id}/revisions",
    handle: (request, response, { agent_id: id }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        // An environment naming no row is a 400 rather than a 404, exactly as store/agents.go answers it: the
        // PROFILE in the path exists, so a 404 would point the operator at the wrong thing.
        const environment = typeof body.environment === "string" ? body.environment : "";
        if (environment !== "" && !environments.has(environment)) {
          return sendProblem(response, 400, "invalid_request", "the revision carries an unsupported field (accepted: model, tools, instructions, tool_sets, mcp_connections, skills, hooks)");
        }
        const lineage = revisions.get(id) ?? [];
        revisionSeq += 1;
        const rev = {
          id: `agrev_console_${String(revisionSeq)}`,
          revision_number: lineage.length + 1,
          model: typeof body.model === "string" ? body.model : "",
          // Nil, not empty — see revisionRow. A revision that named no tools has `null` on the real wire.
          tools: Array.isArray(body.tools) ? body.tools : null,
          tool_sets: Array.isArray(body.tool_sets) ? body.tool_sets : null,
          mcp_connections: Array.isArray(body.mcp_connections) ? body.mcp_connections : null,
          instructions: typeof body.instructions === "string" ? body.instructions : "",
          environment,
          status: "draft",
        };
        // NEWEST FIRST, which is the order ListAgentRevisions returns and the order AgentDiff diffs in.
        revisions.set(id, [rev, ...lineage]);
        sendJSON(response, 201, revisionRow(id, rev));
      }),
  },
  {
    method: "POST",
    pattern: "/v1/agents/{agent_id}/revisions/{revision_id}/publish",
    handle: (_req, response, { agent_id: id, revision_id: revID }) => {
      const rev = (revisions.get(id) ?? []).find((r) => r.id === revID);
      if (rev === undefined) {
        // SYNTHESISED for an unknown lineage/revision, like the environment detail and binding routes and for
        // the same reason: the sweep's arm 1 probes every pattern with placeholder segments, and a 404 there
        // reads as "the table declares a route the fixture does not serve" — which is what it reported the
        // first time this route existed. The real route answers 404 (store/agents.go NotFound) and that
        // refusal is proven against a real router in
        // apps/control-plane/internal/execution/console_environment_run_component_test.go; no console path
        // depends on either answer, because the publish button only exists on a row the list returned.
        return sendJSON(response, 200, revisionRow(id, { id: revID, revision_number: 1, model: "", tools: null, tool_sets: null, instructions: "", environment: "", status: "published" }));
      }
      // A RE-PUBLISH IS AN IDEMPOTENT SUCCESS on the real surface (store/agents.go publishResult), not a
      // conflict — publishing is irreversible, so asking twice is asking for the state it is already in.
      rev.status = "published";
      sendJSON(response, 200, revisionRow(id, rev));
    },
  },

  // --- MCP CONNECTIONS + THE TOOL REGISTRY (E25 T7), in api/router.go's registration order. ---
  //
  // SYNTHESISED-ON-UNKNOWN, on the three routes whose path carries an id: the sweep's arm 1 probes every
  // pattern with a placeholder segment and reads a 404 as "the table declares a route the fixture does not
  // serve". The real routes answer 404 for an unknown or foreign id — GET /v1/tools/{id}/revisions is a 404
  // rather than an empty page ON PURPOSE (store/tools.go checks the lineage first), and that refusal is
  // proven against a REAL router and a REAL foreign tenant in
  // apps/control-plane/internal/execution/jira_runbook_component_test.go. No console path depends on either
  // answer: every id this console puts in a path came out of a list it just read.
  {
    method: "POST",
    pattern: "/v1/mcp-connections",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const name = typeof body.name === "string" ? body.name.trim() : "";
        const config = typeof body.config === "object" && body.config !== null ? body.config : {};
        if (name === "") return sendProblem(response, 400, "invalid_request", "the request carries an invalid transport, config, or name, or an inline credential");
        // The real refusal for an http connection with no url (extensions/mcp.go validateConnectionConfig).
        if (typeof config.url !== "string" || config.url === "") {
          return sendProblem(response, 400, "invalid_request", "the request carries an invalid transport, config, or name, or an inline credential");
        }
        mcpSeq += 1;
        const conn = { id: `mcpc_console_${String(mcpSeq).padStart(4, "0")}`, name, transport: "http", trust_level: "untrusted", disabled: false };
        mcpConnections.set(conn.id, conn);
        sendJSON(response, 201, mcpConnectionRow(conn));
      }),
  },
  { method: "GET", pattern: "/v1/mcp-connections", handle: (_req, res) => sendJSON(res, 200, { data: [...mcpConnections.values()].map(mcpConnectionRow), has_more: false }) },
  {
    method: "GET",
    pattern: "/v1/mcp-connections/{id}",
    handle: (_req, res, { id }) =>
      sendJSON(res, 200, mcpConnectionRow(mcpConnections.get(id) ?? { id, name: "synthesised", transport: "http", trust_level: "untrusted", disabled: false })),
  },
  {
    method: "POST",
    pattern: "/v1/mcp-connections/{id}/discover",
    // THE ONLY ROUTE HERE THAT WOULD TOUCH A REAL SERVER. On this profile it is a fixture and that is §6
    // leg 3 — what is proven below the browser is that a DRAFT is what discovery leaves behind, never a
    // published revision, which is the property EXT-006 names and the reason this screen exists at all.
    handle: (request, response, { id }) =>
      drainBody(request, () => {
        const conn = mcpConnections.get(id);
        if (conn === undefined) {
          return sendJSON(response, 200, { object: "mcp_discovery", connection_id: id, new_revisions: [], unchanged: [], rejected: [] });
        }
        const fresh = [];
        const unchanged = [];
        for (const offered of mcpDiscoverable) {
          const canonical = `mcp.${conn.name}.${offered.remote}`;
          let tool = [...toolLineages.values()].find((t) => t.canonical_name === canonical);
          if (tool === undefined) {
            toolSeq += 1;
            tool = {
              id: `tool_console_${String(toolSeq).padStart(4, "0")}`,
              canonical_name: canonical,
              model_visible_name: `${conn.name}__${offered.remote}`,
            };
            toolLineages.set(tool.id, tool);
            toolRevisionsByTool.set(tool.id, []);
          }
          const existing = toolRevisionsByTool.get(tool.id) ?? [];
          // The no-churn rule: an identical description + schema writes NOTHING on re-discovery.
          if (existing.some((r) => r.description === offered.description)) {
            unchanged.push(offered.remote);
            continue;
          }
          toolRevSeq += 1;
          const rev = {
            id: `trev_console_${String(toolRevSeq).padStart(4, "0")}`,
            tool_id: tool.id,
            revision_number: existing.length + 1,
            executor: "mcp",
            description: offered.description,
            input_schema: offered.input_schema,
            digest: `sha256:fixture${String(toolRevSeq).padStart(2, "0")}`,
            status: "draft",
            approval_required: false,
            approval_label: "",
            created_at: fixtureTime(toolRevSeq),
          };
          // NEWEST FIRST, the order ListToolRevisions returns.
          toolRevisionsByTool.set(tool.id, [rev, ...existing]);
          fresh.push(offered.remote);
        }
        sendJSON(response, 200, { object: "mcp_discovery", connection_id: id, new_revisions: fresh, unchanged, rejected: [] });
      }),
  },
  // THE MANUAL REGISTRY WRITE PATH, AND ITS ABSENCE MADE `pnpm sweep` RED FROM THE MOMENT E25 T7 LANDED.
  // T7 added a seed to tests/conformance.test.mjs that creates a tool and a revision on BOTH sides through
  // these two routes — and the fixture served neither, so seedBothStacks asserted on `POST /v1/tools
  // returned 404` before a single arm ran. Nothing else could have caught it: the sweep's arm 2 checks only
  // that every route the FIXTURE serves is registered by the real router, never the other direction, so a
  // route the real router mounts and the fixture lacks is invisible to it by design. The seed was the one
  // thing that would notice, and the seed is what never ran.
  //
  // The console itself does not use either route — it fills the registry by DISCOVERY (the block above), and
  // that is why the gap survived T7's own screen work. They are here for the sweep's benefit, so a manually
  // registered lineage and a control_plane revision exist on both sides and their projections get compared.
  //
  // BOTH SHAPES WERE MEASURED AGAINST THE RUNNING REAL ROUTER RATHER THAN READ OFF A STRUCT, and the second
  // one is not what a reader would guess: creating a revision answers a NARROWER projection than listing
  // one. `description`, `input_schema`, `approval_required`, `approval_label` and `created_at` are all
  // absent from the create answer and present in ListToolRevisions, so a fixture that echoed toolRevisionRow
  // here would have invented five fields — the exact defect class this commit's own artifact-row finding is.
  {
    method: "POST",
    pattern: "/v1/tools",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const canonical = typeof body.canonical_name === "string" ? body.canonical_name.trim() : "";
        if (canonical === "") return sendProblem(response, 400, "invalid_request", "the request carries an unsupported field, a malformed canonical name, or a widening override");
        toolSeq += 1;
        const tool = {
          id: `tool_console_${String(toolSeq).padStart(4, "0")}`,
          canonical_name: canonical,
          // The real derivation, measured: `sweep.shape.probe1` answered model_visible_name `probe1`, so it
          // is the last dot-segment rather than the whole name or a slug of it.
          model_visible_name: canonical.slice(canonical.lastIndexOf(".") + 1),
        };
        toolLineages.set(tool.id, tool);
        toolRevisionsByTool.set(tool.id, []);
        sendJSON(response, 201, toolRow(tool));
      }),
  },
  { method: "GET", pattern: "/v1/tools", handle: (_req, res) => sendJSON(res, 200, { data: [...toolLineages.values()].map(toolRow), has_more: false }) },
  {
    method: "POST",
    pattern: "/v1/tools/{tool_id}/revisions",
    handle: (request, response, { tool_id: id }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const executor = typeof body.executor === "string" ? body.executor : "";
        if (executor === "") return sendProblem(response, 400, "invalid_request", "the request carries an unsupported field, a malformed canonical name, or a widening override");
        const existing = toolRevisionsByTool.get(id) ?? [];
        toolRevSeq += 1;
        const rev = {
          id: `trev_console_${String(toolRevSeq).padStart(4, "0")}`,
          tool_id: id,
          revision_number: existing.length + 1,
          executor,
          description: typeof body.description === "string" ? body.description : "",
          input_schema: body.input_schema ?? { type: "object" },
          digest: `sha256:fixture${String(toolRevSeq).padStart(2, "0")}`,
          status: "draft",
          approval_required: false,
          approval_label: "",
          created_at: fixtureTime(toolRevSeq),
        };
        // NEWEST FIRST, the order ListToolRevisions returns — the same rule the discovery path follows.
        toolRevisionsByTool.set(id, [rev, ...existing]);
        // THE NARROW CREATE PROJECTION, not toolRevisionRow. See the note above this block.
        sendJSON(response, 201, {
          id: rev.id,
          object: "tool_revision",
          tool_id: rev.tool_id,
          revision_number: rev.revision_number,
          executor: rev.executor,
          digest: rev.digest,
          status: rev.status,
        });
      }),
  },
  {
    method: "GET",
    pattern: "/v1/tools/{tool_id}/revisions",
    handle: (_req, res, { tool_id: id }) => sendJSON(res, 200, { data: (toolRevisionsByTool.get(id) ?? []).map(toolRevisionRow), has_more: false }),
  },
  // The single-resource read POST /v1/tools/{tool_id}/revisions names in its 201 Location. It is served
  // here for the reason the sweep's route arm cannot catch on its own: that arm checks only that every
  // route the FIXTURE serves is registered by the real router, never the other direction, so a route
  // mounted on the real side and missing here is INVISIBLE to it. Its projection is toolRevisionRow —
  // the list's, field for field — because the real route shares one projection helper with the list.
  {
    method: "GET",
    pattern: "/v1/tools/{tool_id}/revisions/{revision_id}",
    handle: (_req, res, { tool_id: id, revision_id: revID }) => {
      const rev = (toolRevisionsByTool.get(id) ?? []).find((r) => r.id === revID);
      return sendJSON(
        res,
        200,
        toolRevisionRow(
          rev ?? {
            id: revID,
            tool_id: id,
            revision_number: 1,
            executor: "control_plane",
            description: "",
            input_schema: { type: "object" },
            digest: "sha256:fixturesynthesised",
            status: "draft",
            approval_required: false,
            approval_label: "",
            created_at: fixtureTime(0),
          },
        ),
      );
    },
  },
  {
    method: "POST",
    pattern: "/v1/tools/{tool_id}/revisions/{revision_id}/publish",
    handle: (request, response, { tool_id: toolID, revision_id: revID }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        // THE LABEL CEILING IS REAL AND IT IS 300 (extensions.MaxApprovalLabelLen): a longer one is
        // ErrApprovalLabelTooLong → BadField → 400, NOT a silently truncated label on the approval screen.
        // Serving it here is what makes the console's publish-refusal region reachable at all.
        if (typeof body.approval_label === "string" && body.approval_label.length > 300) {
          return sendProblem(response, 400, "invalid_request", "the request carries an unsupported field, a malformed canonical name, or a widening override");
        }
        const rev = (toolRevisionsByTool.get(toolID) ?? []).find((r) => r.id === revID);
        if (rev === undefined) return sendJSON(response, 200, { id: revID, status: "published" });
        // A RE-PUBLISH CHANGES NOTHING, which is the guard's whole point: a gate cannot be quietly REMOVED
        // from an already-published revision by calling this again without the flag (storage/queries/
        // tools.sql PublishToolRevision — the UPDATE is conditional on published_at IS NULL).
        if (rev.status !== "published") {
          rev.status = "published";
          rev.approval_required = body.approval_required === true;
          rev.approval_label = typeof body.approval_label === "string" ? body.approval_label : "";
        }
        sendJSON(response, 200, { id: revID, status: "published" });
      }),
  },
  { method: "GET", pattern: "/v1/tool-sets", handle: (_req, res) => sendJSON(res, 200, { data: toolSetRevisions.map(toolSetListRow), has_more: false }) },
  {
    method: "POST",
    pattern: "/v1/tool-sets/{set}/revisions",
    handle: (request, response, { set }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const pins = Array.isArray(body.tools) ? body.tools : [];
        // ONLY PUBLISHED REVISIONS MAY BE PINNED — a draft pin is a 409 on the real surface
        // (extensions.ErrRevisionNotPublished → Conflict), and it is the refusal an operator meets if they
        // pin before approving. Serving it here is what lets the console's error region be exercised.
        for (const pin of pins) {
          const revID = typeof pin?.tool_revision_id === "string" ? pin.tool_revision_id : "";
          const found = [...toolRevisionsByTool.values()].flat().find((r) => r.id === revID);
          if (found !== undefined && found.status !== "published") {
            return sendProblem(response, 409, "conflict", "the tool name is already taken, shadows a built-in, or a pinned revision is not published");
          }
        }
        toolSetSeq += 1;
        const rev = {
          id: `tsrev_console_${String(toolSetSeq).padStart(4, "0")}`,
          set,
          revision_number: toolSetRevisions.filter((r) => r.set === set).length + 1,
          digest: `sha256:fixtureset${String(toolSetSeq).padStart(2, "0")}`,
          status: "draft",
          tools: pins,
          created_at: fixtureTime(100 + toolSetSeq),
        };
        toolSetRevisions.unshift(rev);
        sendJSON(response, 201, { id: rev.id, object: "tool_set_revision", set, revision_number: rev.revision_number, digest: rev.digest, status: "draft" });
      }),
  },
  {
    method: "GET",
    pattern: "/v1/tool-sets/{set}/revisions/{revision_id}",
    handle: (_req, res, { set, revision_id: revID }) => {
      const rev = toolSetRevisions.find((r) => r.id === revID && r.set === set);
      return sendJSON(
        res,
        200,
        toolSetDetailRow(rev ?? { id: revID, set, revision_number: 1, digest: "sha256:fixturesynthesised", status: "draft", tools: [], created_at: fixtureTime(0) }),
      );
    },
  },
  {
    method: "POST",
    pattern: "/v1/tool-sets/{set}/revisions/{revision_id}/publish",
    handle: (_req, response, { revision_id: revID }) => {
      const rev = toolSetRevisions.find((r) => r.id === revID);
      if (rev !== undefined) rev.status = "published";
      sendJSON(response, 200, { id: revID, status: "published" });
    },
  },

  // --- REPOSITORY BINDINGS (E25 T6). Three routes, in api/router.go:44-46's registration order. ---
  {
    method: "POST",
    pattern: "/v1/repository-bindings",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        const provider = typeof body.provider === "string" ? body.provider : "";
        const identity = typeof body.repository_identity === "string" ? body.repository_identity : "";
        const cloneURL = typeof body.clone_url === "string" ? body.clone_url : "";
        // The scheme gate FIRST, exactly as api/repository_bindings.go orders it — before the store is asked
        // anything, so a `file:` URL can never be the answer to a missing-field question.
        if (!/^https?:\/\//i.test(cloneURL)) {
          return sendProblem(response, 400, "invalid_request", "clone_url must be an http(s) URL");
        }
        if (provider === "" || identity === "") {
          return sendProblem(response, 400, "invalid_request", "provider, repository_identity, and clone_url are required");
        }
        bindingSeq += 1;
        // omitempty, field for field, like contracts.RepositoryBinding: an absent connection_ref/policy/
        // classification/region is ABSENT rather than "".
        const binding = {
          id: `rbind_console_${String(bindingSeq).padStart(4, "0")}`,
          object: "repository_binding",
          provider,
          repository_identity: identity,
          clone_url: cloneURL,
          default_branch: typeof body.default_branch === "string" && body.default_branch !== "" ? body.default_branch : "main",
          organization_id: "org_local",
          project_id: "proj_local",
          created_at: new Date(Date.UTC(2026, 6, 30, 2, 0, bindingSeq)).toISOString(),
          ...(typeof body.connection_ref === "string" && body.connection_ref !== "" ? { connection_ref: body.connection_ref } : {}),
          ...(Array.isArray(body.allowed_operations) && body.allowed_operations.length > 0 ? { allowed_operations: body.allowed_operations } : {}),
          ...(body.policy !== undefined && body.policy !== null ? { policy: body.policy } : {}),
          ...(typeof body.data_classification === "string" && body.data_classification !== "" ? { data_classification: body.data_classification } : {}),
          ...(typeof body.region_constraint === "string" && body.region_constraint !== "" ? { region_constraint: body.region_constraint } : {}),
        };
        // Newest first, like ListRepositoryBindings' `created_at DESC, id DESC`.
        bindings.unshift(binding);
        sendJSON(response, 201, binding);
      }),
  },
  {
    method: "GET",
    pattern: "/v1/repository-bindings",
    // {data, has_more} AND NOTHING ELSE, which is renderPage's envelope over a page that ENDS the collection
    // (next_cursor and previous_cursor are omitempty pointers on contracts.Page and neither is set). This is
    // the same reason GET /v1/approvals does not go through pageSlice: that helper serves both cursor keys as
    // explicit nulls, which is the recorded DIV-SHP-004/005 divergence, and a third route reproducing it
    // would need a third ledger row for a difference that need not exist.
    handle: (_req, res) => sendJSON(res, 200, { data: bindings, has_more: false }),
  },
  {
    method: "GET",
    pattern: "/v1/repository-bindings/{binding_id}",
    handle: (_req, response, { binding_id: id }) => {
      const found = bindings.find((b) => b.id === id);
      // Synthesised for an unknown id, like the environment detail route and for the same reason: the sweep's
      // arm 1 probes every pattern with a placeholder and a 404 there would read as "the table declares a
      // route the fixture does not serve". The real route 404s; no console path depends on either answer.
      sendJSON(response, 200, found ?? { id, object: "repository_binding", provider: "github", repository_identity: id, clone_url: `https://example.invalid/${id}.git`, default_branch: "main" });
    },
  },

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
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        // THE OPTIONAL PIN (E25 T6), and its two refusals in the REAL ORDER. api/responses.go answers a pin
        // naming an unknown revision with a tenant-scoped 404 and a pin onto a DRAFT with 409
        // `revision_not_published`; both are decided by admission BEFORE the idempotency reserve
        // (coordinator/store.go PinnedRevisionNotFound / PinnedRevisionNotPublished), so neither leaves a run,
        // a session or an idempotency record behind. The fixture leaves nothing behind either — newRun() is
        // called only after both gates pass — which is what makes the console's "this run did not start"
        // sentence checkable rather than decorative.
        const pin = typeof body.agent_revision_id === "string" ? body.agent_revision_id : "";
        let model = MODEL;
        if (pin !== "") {
          const found = findRevision(pin);
          if (found === null) {
            return sendProblem(response, 404, "not_found", "no such agent revision or run template in this project");
          }
          if (found.revision.status !== "published") {
            return sendProblem(response, 409, "revision_not_published", "the pinned revision is a draft; publish it before running it");
          }
          // The revision's model wins over the deployment default, which is what execution/config.go's
          // layerAgentRevision does. A revision that pins no model inherits, so an empty one is not a pin.
          if (found.revision.model !== "") model = found.revision.model;
        }
        const { sid, rid, runId } = newRun(model);
        sendJSON(response, 202, { id: rid, object: "response", status: "queued", model, session_id: sid, run_id: runId, created_at: "2026-07-24T00:00:00Z", output: [], usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 } });
      }),
  },
  {
    method: "GET",
    pattern: "/v1/responses/{response_id}",
    handle: (_request, response, { response_id: rid }) => {
      const sid = rid.replace("resp_", "ses_");
      // The terminal projection reflects the ACTUAL outcome: a denied run is canceled with no output (the
      // side effect was blocked), an approved run completed with its output. This is the canonical terminal
      // Response the SDK retrieves after the stream's terminal event.
      const state = sessions.get(sid);
      const denied = state?.denied === true;
      sendJSON(response, 200, {
        id: rid,
        object: "response",
        status: denied ? "canceled" : "completed",
        // The model this run actually ran with — the pinned revision's when it had one (E25 T6). An unknown
        // response id (the sweep's probe) has no session and falls back to the deployment default.
        model: state?.model ?? MODEL,
        session_id: sid,
        created_at: "2026-07-24T00:00:00Z",
        updated_at: "2026-07-24T00:00:03Z",
        output: denied ? [] : FINAL_OUTPUT,
        usage: denied ? { input_tokens: 40, output_tokens: 0, total_tokens: 40, tool_calls: 0 } : USAGE,
      });
    },
  },
  // GET /v1/responses — RUN HISTORY (E25 T8, feature list O1). Paged like every other list, and derived from
  // the runs this fixture has actually created rather than from a static row: the console's history screen
  // has to show a run the suite just drove, and a frozen fixture row would make that assertion a property of
  // which spec file ran first (the trap pagination.spec.ts already paid for once).
  //
  // THE ROW SHAPE IS THE LIST ROW'S, NOT THE DETAIL'S, and the difference is the point: store/postgres.go
  // ListResponses marshals contracts.Response with Model "", Output [] and a ZERO Usage — model, usage and
  // output come from the per-id GET, so a page never decodes N output blobs. Serving the richer detail shape
  // here would have taught the console that a list row carries a model, and the console would then have
  // rendered an empty column against the real API.
  { method: "GET", pattern: "/v1/responses", handle: (request, response) => pageSlice(responseListRows(), requestURL(request), response) },
  {
    method: "GET",
    pattern: "/v1/responses/{response_id}/artifacts",
    // THE PROJECTION IS THE REAL ONE NOW, AND IT WAS INVENTED BEFORE (E25 T8). This route used to serve
    // `{id, object, filename, byte_size}` — and the artifact projection has NEITHER of those two fields:
    // artifacts/reader.go metadataRow.projection writes id, object, run_id, size_bytes, checksum, media_type,
    // logical_type, malware_scan_status and created_at, and no name at all. Nothing caught it because the
    // only consumer was /runs, which reads `a.id` and nothing else, and because a compose run leaves no
    // artifact for the sweep's item arm to compare against. E25 T8's artifact browser is the first screen
    // that would have rendered those columns — blank, on the real API, with a filename column that can never
    // be filled from this route.
    handle: (_request, response) => sendJSON(response, 200, listView([artifactProjection("art_1")])),
  },
  {
    method: "GET",
    pattern: "/v1/artifacts/{artifact_id}",
    // The per-id metadata read. It answers with the SAME projection the list above serves, because that is
    // what the real pair does — ListRunArtifacts and GetArtifact both render metadataRow.projection(), so
    // this route adds no field the list did not already carry. It is served here because the sweep's arm 1
    // requires every table row to be reachable and because a console that lists ids must be able to resolve
    // one; it is NOT a richer view, and the artifact browser does not pretend it is.
    handle: (_request, response, { artifact_id: id }) => sendJSON(response, 200, artifactProjection(id)),
  },

  // --- DISCOVERY AND METERING (E25 T8, feature list O9 / O4 / O5) -----------------------------------------
  {
    method: "GET",
    pattern: "/v1/capabilities",
    // The UNCONDITIONAL half of api/capabilities.go's matrix and nothing else. `a2a`, `slack`, `queues`,
    // `knowledge` and `capability-workers` are advertised ONLY where the binary MOUNTED them, so a fixture
    // that listed them would be claiming mounts on behalf of a deployment it is not — the §2 discovery lie
    // in fixture form. `workspaces` is "unavailable" because this fixture configures no workspace root,
    // which is the same derivation workspacesCapability() makes.
    handle: (_request, response) =>
      sendJSON(response, 200, {
        object: "capabilities",
        maturity: "preview",
        isolation: "development",
        retention: { store_false_ttl_seconds: 0 },
        capabilities: {
          responses: "preview",
          sessions: "unavailable",
          workspaces: "unavailable",
          "knowledge-vector": "disabled",
          "apple-build": "disabled",
          console: "preview",
        },
      }),
  },
  {
    method: "GET",
    pattern: "/v1/deployment",
    // THE MACHINE'S EFFECTIVE CONFIGURATION (machine-config). The keys are api/deployment.go's
    // deploymentSetting/deploymentWarning verbatim; the ROWS are a subset, and the subset is chosen so this
    // fixture answers what the REAL PROFILE's stack answers rather than a shape invented to be convenient.
    //
    // tests/divergences.mjs records the compose stack this suite's real profile runs against:
    // `PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up`. So on both profiles the
    // dispatcher is ON (no blocking warning) and the model provider IS the fake adapter (one advisory
    // warning). A fixture that scripted the blocking warning would make the /runs banner assertable here
    // and NOT on the real profile — the fixture proving a console behaviour the real stack contradicts,
    // which is the exact class E17 T10's own gate found. The blocking arm is asserted by intercepting the
    // relay in tests/deployment.spec.ts, which is a statement about this console's rendering and is
    // therefore true on both profiles.
    handle: (_request, response) =>
      sendJSON(response, 200, {
        object: "deployment",
        settings: [
          {
            name: "PALAI_DISPATCH_WORKERS",
            group: "execution",
            value: "1",
            set: true,
            default: "1",
            kind: "value",
            effect:
              "How many durable dispatch workers run. ZERO IS NOT 'SLOWER', IT IS OFF: startDispatch returns before it builds anything, so the deployment admits runs through POST /v1/responses and executes none.",
            mutability: "bring_up",
            change_with: "recreate the control-plane with the new value (`palai up`)",
            reader_file: "apps/control-plane/api/deployment.go",
            reader_func: "DispatchWorkers",
            // THE DESIRED HALF (E29). These five fields exist on EVERY row the real control plane serves, and
            // the values below are what a real stack with no saved document answers: writable where the
            // catalogue says so, and desired_set false everywhere because nothing has been written. A fixture
            // that omitted `writable` would leave the console's Edit control DISABLED — so the a11y sweep
            // could not open the dialog, and the panel would be measured in a state the real profile never has.
            writable: true,
            value_grammar: "integer",
            desired: "",
            desired_set: false,
            drift: false,
          },
          {
            name: "PALAI_MODEL_PROVIDER",
            group: "model",
            value: "fake",
            set: true,
            default: "fake",
            kind: "value",
            effect:
              "The DEPLOYMENT-DEFAULT model route. Exactly one value — `provider-one` — selects a live provider; every other value falls through to the deterministic fake adapter.",
            mutability: "bring_up_default_only",
            change_with:
              "for ONE project, publish a model route (POST /v1/model-routes) — it is resolved per attempt and overrides this with no restart",
            reader_file: "apps/control-plane/cmd/palai-control-plane/main.go",
            reader_func: "modelBrokerFromEnv",
            writable: true,
            value_grammar: "token",
            desired: "",
            desired_set: false,
            drift: false,
          },
          // AN UNSET ROW, because "unset" is the state this screen most has to render well: it is the
          // difference between "one worker" and "no object store at all", and both wear the same word.
          {
            name: "PALAI_SANDBOX_IMAGE",
            group: "shell",
            value: "",
            set: false,
            default: "none — there is no shell tool; a shell call fails cleanly rather than escaping",
            kind: "value",
            effect: "The pinned command image the workspace shell tool runs inside.",
            mutability: "bring_up",
            change_with: "recreate the control-plane with the new value (`palai up`)",
            reader_file: "apps/control-plane/cmd/palai-control-plane/main.go",
            reader_func: "shellRunnerFromEnv",
            writable: false,
            not_writable_because:
              "an IMAGE REFERENCE, for the same reason: it is the container every workspace shell call runs inside, with the workspace mounted. Choosing it is a supply-chain decision made at install time, not a setting.",
            desired: "",
            desired_set: false,
            drift: false,
          },
          // A PATH ROW. The compose stack passes this (compose.yaml:126), and it is what makes the credential
          // rule visible on the screen: the master key's PATH is reported and the key never is.
          {
            name: "PALAI_SECRET_MASTER_KEY_FILE",
            group: "identity",
            value: "/run/secrets/master_key",
            set: true,
            default: "unset — the DB-backed secret store is DISABLED",
            kind: "path",
            effect: "The file holding the master key the envelope-encrypted secret store redeems through.",
            mutability: "bring_up",
            change_with:
              "a stored secret is rotated live through POST /v1/secret-refs and needs no restart; changing the MASTER KEY's location needs a recreate",
            reader_file: "apps/control-plane/cmd/palai-control-plane/main.go",
            reader_func: "main",
            writable: false,
            not_writable_because:
              "a path, and the sharpest one: it names the file the ENTIRE secret store redeems through. Moving it from a form points the store at a file the operator chose and the process reads at boot with no further question.",
            desired: "",
            desired_set: false,
            drift: false,
          },
        ],
        warnings: [
          {
            code: "model_provider_fake",
            severity: "advisory",
            headline: "Every run without a published model route is answered by the deterministic fake adapter.",
            detail:
              'The deployment default route is chosen by an exact match on `provider-one`; PALAI_MODEL_PROVIDER is "fake", so the fallback is the fake adapter. Its output is fabricated and renders exactly like a real answer.',
            remedy:
              "A project that publishes a model route (POST /v1/model-routes) dispatches through that instead, with no restart.",
            settings: ["PALAI_MODEL_PROVIDER", "PALAI_MODEL"],
          },
        ],
        // NULL, NOT AN EMPTY OBJECT, and it matches what the real profile's stack answers: nothing has ever
        // written a desired document to it. The two are DIFFERENT facts — "nobody has configured this
        // machine from the panel" versus "somebody saved a document with nothing in it" — and the console
        // renders a different sentence for each, so a fixture that flattened them would prove the wrong one.
        //
        // When it is NOT null the real body carries `plane` and `scope_id` beside the revision, because a
        // document is scoped: `control_plane` is this process, `runner_pool` is a pool's machines. Only the
        // first has a reader today.
        desired: null,
      }),
  },
  {
    method: "GET",
    pattern: "/v1/usage",
    handle: (_request, response) =>
      sendJSON(response, 200, {
        object: "usage_summary",
        organization_id: "org_local",
        project_id: "proj_local",
        meters: USAGE_METERS,
        // EMPTY ARRAYS, not absent keys: readBudgets/readQuotas initialise `[]budgetView{}` so the real
        // summary carries both keys over an empty scope. And they are empty for the same reason the two
        // list routes below are — see USAGE_BUDGETS.
        budgets: [],
        quotas: [],
      }),
  },
  { method: "GET", pattern: "/v1/usage/ledger", handle: (request, response) => pageSlice(USAGE_LEDGER, requestURL(request), response) },
  // THE LIMIT COLLECTIONS ARE EMPTY ON PURPOSE, AND IT IS A PROOF RATHER THAN A GAP. A bootstrap stack holds
  // no budget and no quota — nothing creates one, there is no CLI verb for either — so an empty answer here
  // is what the real API returns, and it is what makes "an empty metering panel renders a SENTENCE, never a
  // blank region" assertable on BOTH profiles instead of only on whichever one happens to be thin. Inventing
  // a limit row would have bought two exercised column renderers and cost the only deterministic empty-state
  // proof on this surface.
  { method: "GET", pattern: "/v1/budgets", handle: (_request, response) => sendJSON(response, 200, listView([])) },
  { method: "GET", pattern: "/v1/quotas", handle: (_request, response) => sendJSON(response, 200, listView([])) },
  // THE SESSION COLLECTION. Four rows for the four verbs api/router.go registers, and the write pair matters
  // as much as the read pair: PATCH is how a wall of `derived` labels becomes a list an operator can read,
  // and it is the only route in this table whose 400 is about a MISSING field rather than a malformed one
  // (commands.go rename: an absent `name` is a request that lost its field, and succeeding at nothing is how
  // a caller comes to believe a rename happened that did not).
  { method: "GET", pattern: "/v1/sessions", handle: (request, response) => pageSlice(filterSessions(requestURL(request)), requestURL(request), response) },
  {
    method: "POST",
    pattern: "/v1/sessions",
    handle: (request, response) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        if (body.name !== undefined && typeof body.name !== "string") {
          return sendProblem(response, 400, "invalid_request", "the request body is not a valid session write");
        }
        sessionSeq += 1;
        const now = new Date().toISOString();
        const row = {
          id: ses(900 + sessionSeq),
          object: "session",
          status: "active",
          created_at: now,
          organization_id: "org_local",
          project_id: "prj_local",
          name: typeof body.name === "string" ? body.name : "",
          // A session created through the API has no runs yet, so there is no first prompt to derive a label
          // from — `none` unless the caller supplied one. This is the row the empty state's action produces.
          name_source: typeof body.name === "string" && body.name !== "" ? "operator" : "none",
          agents: [],
          input_tokens: 0,
          output_tokens: 0,
          // No span keys at all: this session has never run, and the real create answers exactly these
          // eleven fields (measured against the running stack, 2026-07-31). See SESSIONS above.
        };
        // Newest first, which is the order the real keyset (created_at DESC, id DESC) serves.
        SESSIONS.unshift(row);
        sessions.set(row.id, { approved: true, denied: false, model: MODEL });
        response.writeHead(201, { "content-type": "application/json; charset=utf-8", "cache-control": "no-store", location: `/v1/sessions/${row.id}` });
        response.end(JSON.stringify(row));
      }),
  },
  {
    method: "GET",
    pattern: "/v1/sessions/{session_id}",
    handle: (_request, response, { session_id: id }) => {
      const row = findSession(id);
      if (row === undefined) return sendProblem(response, 404, "not_found", "no such session in this project");
      sendJSON(response, 200, row);
    },
  },
  {
    method: "PATCH",
    pattern: "/v1/sessions/{session_id}",
    handle: (request, response, { session_id: id }) =>
      drainBody(request, (raw) => {
        const body = parseBody(raw);
        if (typeof body.name !== "string") return sendProblem(response, 400, "invalid_request", "name is required");
        if ([...body.name].length > 200) return sendProblem(response, 400, "invalid_request", "name must be at most 200 characters");
        const row = findSession(id);
        if (row === undefined) return sendProblem(response, 404, "not_found", "no such session in this project");
        row.name = body.name;
        // CLEARING IS A REAL VALUE, and it does not leave the row claiming an operator chose an empty label:
        // an empty name falls back to whatever the row can derive, which for these fixtures is the prompt cut
        // they were seeded with — `derived` — or `none` for the one that never had a prompt.
        row.name_source = body.name === "" ? (row.id === ses(4) ? "none" : "derived") : "operator";
        if (body.name === "" && row.name_source === "derived") row.name = "Push the release branch.";
        sendJSON(response, 200, row);
      }),
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
  // __reset restores every collection a test can MUTATE, so a spec file can start from the fixture as
  // authored rather than from whatever the previous file — or the previous colour-scheme project — left
  // behind. It is deliberately explicit about WHAT it restores: a reset that silently misses a collection
  // is worse than none, because the file that calls it then believes it is isolated.
  if (method === "POST" && pathname === "/__reset") {
    SESSIONS.length = 0;
    for (const row of structuredClone(SESSIONS_PRISTINE)) SESSIONS.push(row);
    const fresh = structuredClone(ADMIN_PRISTINE);
    for (const key of Object.keys(ADMIN)) ADMIN[key] = fresh[key];
    return sendJSON(response, 200, { reset: ["sessions", ...Object.keys(ADMIN)], sessions: SESSIONS.length });
  }
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
    if (!introspect.nonV1Paths.includes(pathname)) introspect.nonV1Paths.push(`${method} ${pathname}`);
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
