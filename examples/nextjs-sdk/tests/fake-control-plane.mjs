// A deterministic fake control-plane the SDK connects to over the network, so the browser
// proof runs against a scripted canonical event stream instead of the live provider — no
// credential spend, no flakiness. It is the browser-test counterpart of the fake model
// adapter (adapters/models/fake): the same two-model-call / one-add-tool / "12" exchange,
// projected as canonical events (spikes/nextjs-streaming/test/fake-upstream.mjs precedent).
//
// It speaks the exact HTTP contract the Task-13 SDK drives: POST /v1/responses (202 handle),
// GET /v1/sessions/{id}/events (resumable SSE), GET /v1/responses/{id} (terminal projection),
// POST /v1/responses/{id}/cancel. Every data endpoint requires a Bearer token, so the proof
// also shows the SDK sends the credential server-side — while the browser scan shows it never
// reaches the client. A /__introspect endpoint reports the cancel count so a test can assert
// a browser abort closed the transport WITHOUT cancelling the run (LP6: disconnect ≠ cancel).
import { readFileSync } from "node:fs";
import { createServer } from "node:http";

const PORT = Number(process.env.FAKE_UPSTREAM_PORT ?? 3101);
const SESSION_ID = "ses_live_proof_0001";
const RESPONSE_ID = "resp_live_proof_0001";
const RUN_ID = "run_live_proof_0001";
const MODEL = "fake";
const FINAL_OUTPUT = [{ type: "output_text", text: "12" }];
const USAGE = { input_tokens: 24, output_tokens: 8, total_tokens: 32, tool_calls: 1 };
// Gap between frames: long enough to reliably abort mid-stream, short enough to keep the
// happy path snappy (14 frames ≈ 1.7s total).
const FRAME_GAP_MS = 120;

let cancelCalls = 0;
let streamOpens = 0;
let closes = 0; // SSE transports torn down (normal end or client disconnect)
let terminalsSent = 0; // streams that reached the terminal run.completed.v1 frame

// The scripted canonical event envelopes (protocols/schemas/execution/event-types.json).
// Each becomes one SSE frame; the nested `data` carries the type-specific payload the Route
// Handler projects. The terminal is run.completed.v1 (SDK isTerminalEvent stops the stream).
function scriptedEvents() {
  const base = {
    source: "palai://fake-control-plane",
    specversion: "1.0",
    session_id: SESSION_ID,
    run_id: RUN_ID,
    datacontenttype: "application/json",
  };
  const rows = [
    ["response.queued.v1", {}],
    ["run.running.v1", {}],
    ["model_step.created.v1", { model_request_id: "mreq_1" }],
    ["model_step.delta.v1", { model_request_id: "mreq_1", text: "The sum " }],
    ["model_step.delta.v1", { model_request_id: "mreq_1", text: "is being " }],
    ["model_step.delta.v1", { model_request_id: "mreq_1", text: "computed. " }],
    ["tool_call.proposed.v1", { tool_call_id: "tcall_add_1", name: "add", arguments: { a: 7, b: 5 } }],
    ["tool_call.completed.v1", { tool_call_id: "tcall_add_1", name: "add", result: "12" }],
    ["model_step.completed.v1", { model_request_id: "mreq_1" }],
    ["model_step.created.v1", { model_request_id: "mreq_2" }],
    ["model_step.delta.v1", { model_request_id: "mreq_2", text: "12" }],
    ["model_step.completed.v1", { model_request_id: "mreq_2" }],
    ["usage.updated.v1", { ...USAGE }],
    ["run.completed.v1", { outcome: "completed" }],
  ];
  return rows.map(([type, data], i) => ({
    ...base,
    id: `evt_${String(i + 1).padStart(4, "0")}`,
    type,
    sequence: i + 1,
    time: new Date(Date.UTC(2026, 0, 1, 0, 0, i + 1)).toISOString(),
    data,
  }));
}

function bearer(request) {
  const header = request.headers["authorization"];
  if (typeof header !== "string" || !header.startsWith("Bearer ")) {
    return null;
  }
  const token = header.slice("Bearer ".length).trim();
  return token === "" ? null : token;
}

function sendJSON(response, status, body) {
  const payload = JSON.stringify(body);
  response.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
  });
  response.end(payload);
}

function sendProblem(response, status, code) {
  response.writeHead(status, { "content-type": "application/problem+json; charset=utf-8" });
  response.end(
    JSON.stringify({
      type: `https://docs.palai.dev/problems/${code}`,
      title: code,
      status,
      code,
      request_id: "req_fake_0001",
    }),
  );
}

// The run terminal states, copied from apps/control-plane/api/events.go:52. The production endpoint
// CLOSES THE CONNECTION after emitting one of these, and that is not a detail — it is the mechanism
// behind the defect this suite now guards. A session with two runs replays the first run's terminal to
// anyone who opens the journal at cursor 0, and the server hangs up there.
const TERMINAL_EVENT_TYPES = new Set([
  "run.completed.v1",
  "run.failed.v1",
  "run.canceled.v1",
  "run.cancelled.v1",
  "run.timed_out.v1",
  "run.budget_exceeded.v1",
]);

// streamEvents replays the journal from `after_sequence` and tails it, exactly as
// apps/control-plane/api/events.go does.
//
// THE CURSOR IS NOT DECORATION HERE. This fake ignored `after_sequence` until 2026-08-02 and always
// replayed from the start, which made it MORE FORGIVING than production in precisely the dimension the
// chat depends on: a second turn that resumed past the first run's terminal looked identical to one
// that did not, so the suite could not tell a fixed adapter from a broken one. That is the "a fake more
// generous than production" trap CLAUDE.md names, pointed at a cursor.
function streamEvents(request, response, events = scriptedEvents()) {
  streamOpens += 1;
  const after = Number(new URL(request.url, "http://127.0.0.1").searchParams.get("after_sequence") ?? 0);
  events = events.filter((e) => e.sequence > (Number.isFinite(after) ? after : 0));
  response.writeHead(200, {
    "content-type": "text/event-stream; charset=utf-8",
    "cache-control": "no-cache, no-transform",
    connection: "keep-alive",
    "x-accel-buffering": "no",
  });

  let index = 0;
  let timer = null;

  const stop = () => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  };
  // A client disconnect (the browser aborted; the SDK closed its upstream transport) stops
  // the stream — it does NOT cancel anything. The run's cancel count stays 0.
  response.once("close", () => {
    closes += 1;
    stop();
  });

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
    const event = events[index++];
    if (event.type === "run.completed.v1") {
      terminalsSent += 1;
    }
    const frame = `id: ${event.id}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`;
    response.write(frame);
    // CLOSE AFTER A TERMINAL FRAME — events.go:149, "clean close after the terminal event". Whether
    // this is the LAST event in the journal is irrelevant to the server, and that is the whole point:
    // a journal holding a second run still hangs up here.
    if (TERMINAL_EVENT_TYPES.has(event.type)) {
      stop();
      response.end();
      return;
    }
    timer = setTimeout(pump, FRAME_GAP_MS);
  };

  // First frame promptly, the rest paced; the leading gap gives the client a window to abort.
  timer = setTimeout(pump, FRAME_GAP_MS);
}


// ---------------------------------------------------------------------------------------------
// THE CODING SCENARIO.
//
// Everything above proves the ORIGINAL /page. This block exists so that /chat — the repo picker, the
// tool card, the approval and the workspace panel — is driven by a test rather than only by hand.
//
// Every payload below is a SHAPE MEASURED ON THE LIVE CONTROL PLANE on 2026-08-02, not an invented
// convenience. A fake that is more generous than production writes code against a shape that does not
// exist, and this tree has been bitten by that twice. In particular:
//
//   * tool_call.executing.v1 / tool_call.completed.v1 NOW CARRY `tool_name`, because production does
//     since E30 T2. This comment used to say the opposite and it was right at the time: the frames
//     carried no name, and the fake reproduced that so the test could assert the screen SAID SO. The
//     defect is fixed, so the fake tracks the fix — a fake still emitting the old shape would test the
//     new renderer against bytes the control plane no longer sends, which is the same class of error
//     as a fake more generous than production, only pointed backwards.
//   * approval.requested.v1 for a PUBLICATION carries {publication_id, operation, branch,
//     request_hash, display} and NO approval_id, no remote, no base, no head_sha and no credential.
//     That is what forces the server-side join to GET /v1/approvals, and it is the bug that made the
//     old chat's Approve button post null.
//   * approval.approved.v1 is followed by NO publication.published.v1, exactly as the live stack
//     behaves with no GitHub App configured, so the "THE PUSH IS NOT CONFIRMED" notice is exercised.
// ---------------------------------------------------------------------------------------------
const CODING_SESSION_ID = "ses_coding_proof_0001";
const CODING_RESPONSE_ID = "resp_coding_proof_0001";
const CODING_RUN_ID = "run_coding_proof_0001";
const PUBLICATION_ID = "pub_coding_0001";
const APPROVAL_ID = "apr_coding_0001";
const REQUEST_HASH = "req_hash_coding_0001";
const PATCH_ARTIFACT_ID = "art_patch_0001";
const TRANSCRIPT_ARTIFACT_ID = "art_transcript_0001";

// THE AUTHORED FILE COMES SECOND ON PURPOSE, and that ordering is the fixture's whole job.
//
// MEASURED on the live stack 2026-08-02: a `swift build` inside the clone wrote 40-odd files under
// `.build/`, and because the changeset lists them in path order the panel selected a compiler MODULE
// CACHE as the file to show. The clone has no `.gitignore` (verified by cloning it), so the changeset
// is right to include them — the screen was wrong to present them as the run's work.
//
// A fixture whose only file was CONTRIBUTING.md could not tell a panel that picks the first AUTHORED
// file from one that picks the first file, because they would be the same file. So `.build/` entries
// are here, and one of them sorts FIRST.
const PATCH_BODY = [
  "diff --git a/.build/artefact-one.txt b/.build/artefact-one.txt",
  "new file mode 100644",
  "index 0000000..1111111",
  "--- /dev/null",
  "+++ b/.build/artefact-one.txt",
  "@@ -0,0 +1 @@",
  "+compiler output, not the agent's work",
  "diff --git a/.build/ModuleCache/Combine.swiftmodule b/.build/ModuleCache/Combine.swiftmodule",
  "new file mode 100644",
  "index 0000000..2222222",
  "--- /dev/null",
  "+++ b/.build/ModuleCache/Combine.swiftmodule",
  "@@ -0,0 +1 @@",
  "+module cache",
  "diff --git a/CONTRIBUTING.md b/CONTRIBUTING.md",
  "new file mode 100644",
  "index 0000000..261ff3b",
  "--- /dev/null",
  "+++ b/CONTRIBUTING.md",
  "@@ -0,0 +1 @@",
  "+Be kind.",
  "",
].join("\n");

// The exact renderer at execution/changeset.go:196-224: "$ cmd", then "exit: n", then stdout, then
// an optional "stderr: …". The second block deliberately has a NON-ZERO exit so the panel's failure
// styling is driven too.
const TRANSCRIPT_BODY = [
  "$ git -C repo add CONTRIBUTING.md",
  "exit: 0",
  "$ git -C repo status --short",
  "exit: 0",
  "A  CONTRIBUTING.md",
  "$ git -C repo push",
  "exit: 128",
  "stderr: fatal: No configured push destination.",
  "",
].join("\n");


// ---------------------------------------------------------------------------------------------
// THE TOOL-CALLS READ (E30 T2), answered with BYTES THE REAL TOOLS PRODUCED.
//
// THE ARGUMENTS ARE `argv`, NOT `command`, AND THAT CORRECTION COST A SILENT BUG. This fake first
// sent `{"command": "xcodebuild …"}` — a shape I had invented. The real `palai.workspace.shell`
// sends `{"argv": ["bash", "-c", "cd repo && xcodebuild …"]}`, measured on a live run 2026-08-02.
// Against the live control plane the renderer found no command, drew NOTHING, and every iOS card
// silently vanished — no error, no blank card, no card. A fake shaped differently from production is
// worse than no fake, because the suite stays green while the product is broken.
//
// The event frames carry the tool NAME and deliberately not the arguments or the result, so this is
// where the iOS renderer gets what it draws. The three results below are read from
// tests/fixtures/*.txt, captured by running the real `xcodebuild` and `xcrun simctl` on a Mac
// against a real iOS Simulator destination — NOT hand-written to match the parser.
//
// That direction matters: a fixture invented to satisfy the parser proves the parser agrees with its
// author. These bytes came out of Xcode 26.6 and the parser has to agree with THEM.
// ---------------------------------------------------------------------------------------------
const IOS_FIXTURES = {
  build: readFileSync(new URL("./fixtures/xcodebuild-build-fail.txt", import.meta.url), "utf8"),
  test: readFileSync(new URL("./fixtures/xcodebuild-test-ok.txt", import.meta.url), "utf8"),
  sim: readFileSync(new URL("./fixtures/simctl-list.txt", import.meta.url), "utf8"),
};

const CODING_TOOL_CALLS = [
  {
    id: "tcall_coding_1",
    object: "tool_call",
    name: "palai.workspace.shell",
    state: "completed",
    replay_class: "irreversible",
    arguments: { argv: ["bash", "-c", "git -C repo add CONTRIBUTING.md"] },
    result: { exit_code: 0, stdout: "" },
    created_at: "2026-08-02T00:00:03Z",
    updated_at: "2026-08-02T00:00:04Z",
  },
  {
    id: "tcall_ios_build",
    object: "tool_call",
    name: "palai.workspace.shell",
    state: "completed",
    replay_class: "irreversible",
    arguments: { argv: ["bash", "-c", "cd repo && xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build"] },
    // exit 65 is what xcodebuild returns for a build failure — a RESULT FIELD, not a transport error.
    result: { exit_code: 65, stdout: IOS_FIXTURES.build },
    created_at: "2026-08-02T00:00:05Z",
    updated_at: "2026-08-02T00:00:06Z",
  },
  {
    id: "tcall_ios_test",
    object: "tool_call",
    name: "palai.workspace.shell",
    state: "completed",
    replay_class: "irreversible",
    arguments: { argv: ["bash", "-c", "cd repo && xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' test"] },
    result: { exit_code: 0, stdout: IOS_FIXTURES.test },
    created_at: "2026-08-02T00:00:07Z",
    updated_at: "2026-08-02T00:00:08Z",
  },
  {
    id: "tcall_ios_sim",
    object: "tool_call",
    name: "palai.workspace.shell",
    state: "completed",
    replay_class: "irreversible",
    arguments: { argv: ["bash", "-c", "xcrun simctl list devices"] },
    result: { exit_code: 0, stdout: IOS_FIXTURES.sim },
    created_at: "2026-08-02T00:00:09Z",
    updated_at: "2026-08-02T00:00:10Z",
  },
];

// The session's standing authorization, as the fake records it. It starts OFF for both halves — the
// state every session is born in — and the PATCH below moves only the half the body names, which is
// the property the screen's toggle depends on.
const codingAutoApprove = { auto_approve_tools: false, auto_approve_publications: false, auto_approve_set_by: "" };

// A RUN WHOSE TOOL FRAMES CARRY NO NAME — what a stack that has not taken E30 T2 still emits, and
// what a run that STARTED before the upgrade replays. The screen has to draw that honestly rather
// than as a tool named "", so the suite needs to be able to produce one.
//
// The flag is set by the create request and read by the event stream opened immediately after it;
// app/api/chat/route.ts awaits responses.create() before it opens the stream, so the ordering holds.
// It is cleared by /__reset-coding along with everything else.
let codingOmitsToolName = false;

// A RUN WHOSE LEDGER ROWS NEVER LAND. `tool_call.completed.v1` is journalled before the tool_calls row
// is committed, so a read fired the instant the frame arrives can miss — measured as five "This call
// ran, but its output could not be read" rows on a screen whose ledger, read a minute later, had all
// eight rows with full stdout and exit codes. The adapter retries; this is the case where retrying
// never helps, and the screen must say the join failed rather than draw an empty successful build.
let codingUnjoinable = false;

// A RUN THAT FAILED TO PROVISION ITS WORKSPACE — the path an operator meets FIRST and the one this
// suite had never driven. MEASURED on the live stack 2026-08-02: the owner typed "selam", the run came
// back `status: "failed"` carrying a complete, actionable error, and the chat said the answer "never
// became readable ... The run itself is on the control plane and did not fail." Both halves false, and
// the truth was in the response the whole time.
let codingFailsProvisioning = false;
const CODING_FAIL_ERROR = {
  code: "workspace_provisioning_failed",
  title: "Workspace could not be prepared",
  detail:
    "the run's repository workspace could not be prepared: git fetch: exit status 128: could not read " +
    "Username for 'https://github.com': terminal prompts disabled. Check the binding's clone_url and " +
    "default branch, and whether a private repository needs a connection_ref naming a token in this " +
    "deployment's secret store.",
  request_id: "req_c460acf1fake0000",
};

// AN UPSTREAM THAT ANSWERS WITH SOMETHING THAT IS NOT JSON. Measured on the live stack while its
// control plane was down: the arming control showed the operator
//     Failed to execute 'json' on 'Response': Unexpected end of JSON input
// a raw JS exception, as the answer to "arm this session". The relay's fetch was unguarded, so an
// unreachable upstream became a 500 with an empty body, and the component's res.json() threw. An
// upstream that is down is an ANSWER — "the control plane did not respond" is a sentence a person can
// act on; a SyntaxError is not. This flag makes that state reachable from a test.
let brokenUpstream = false;

let approvalDecision = null; // null | "approved" | "denied"
const createdBindings = [];
const createdSecrets = [];
// What each create carried, in order. The `instructions` layer is recorded because the repository hint
// moved OFF the user's text and onto it, and "the bubble is clean" is satisfied just as well by a hint
// that was deleted — so the suite has to be able to see both halves.
const codingInstructions = [];
const codingInputs = [];
// What each create carried as `agent_id`. The steering moved onto a published agent revision, so
// "the bubble is clean" and "instructions is empty" are BOTH satisfied by a build that sends no
// steering at all — this is the field that says the agent was actually pinned.
const codingAgentIds = [];
// The agents surface, as module state. Not reset by /__reset-coding: the demo resolves its agent
// ONCE PER SERVER PROCESS (lib/agent.ts caches the promise), so clearing the fake's copy between
// tests would desynchronise the two and make later tests measure a provisioning that the app has
// already cached away. The agent is deployment config, not per-test state.
const agentProfiles = [];
const agentRevisions = {};

function codingEvents() {
  const named = (payload) => (codingOmitsToolName ? { ...payload, tool_name: undefined } : payload);
  const base = {
    source: "palai://fake-control-plane",
    specversion: "1.0",
    session_id: CODING_SESSION_ID,
    run_id: CODING_RUN_ID,
    datacontenttype: "application/json",
  };
  const rows = [
    ["run.queued.v1", { run_id: CODING_RUN_ID, state: "queued" }],
    ["run.running.v1", { run_id: CODING_RUN_ID, state: "running" }],
    ["tool_call.executing.v1", named({ run_id: CODING_RUN_ID, replay_class: "irreversible", tool_call_id: "tcall_coding_1", tool_name: "palai.workspace.shell" })],
    ["tool_call.completed.v1", named({ run_id: CODING_RUN_ID, tool_call_id: "tcall_coding_1", tool_name: "palai.workspace.shell" })],
    // THE iOS CALLS. Three shell calls, named, each answered by the tool-calls read below with REAL
    // captured output: a failing build, a passing test run, and a simulator boot.
    ["tool_call.executing.v1", { run_id: CODING_RUN_ID, replay_class: "irreversible", tool_call_id: "tcall_ios_build", tool_name: "palai.workspace.shell" }],
    ["tool_call.completed.v1", { run_id: CODING_RUN_ID, tool_call_id: "tcall_ios_build", tool_name: "palai.workspace.shell" }],
    ["tool_call.executing.v1", { run_id: CODING_RUN_ID, replay_class: "irreversible", tool_call_id: "tcall_ios_test", tool_name: "palai.workspace.shell" }],
    ["tool_call.completed.v1", { run_id: CODING_RUN_ID, tool_call_id: "tcall_ios_test", tool_name: "palai.workspace.shell" }],
    ["tool_call.executing.v1", { run_id: CODING_RUN_ID, replay_class: "irreversible", tool_call_id: "tcall_ios_sim", tool_name: "palai.workspace.shell" }],
    ["tool_call.completed.v1", { run_id: CODING_RUN_ID, tool_call_id: "tcall_ios_sim", tool_name: "palai.workspace.shell" }],
    ["approval.requested.v1", {
      publication_id: PUBLICATION_ID,
      operation: "push_branch",
      branch: "agent/ws_fake/run_fake",
      request_hash: REQUEST_HASH,
      display: "push agent/ws_fake/run_fake @ 4f4f89e -> http://127.0.0.1:8177/demo-target.git",
    }],
    ["approval.approved.v1", { publication_id: PUBLICATION_ID, command_id: "cmd_coding_0001" }],
    ["run.completed.v1", { run_id: CODING_RUN_ID, state: "completed" }],
  ];
  return rows.map(([type, data], i) => ({
    ...base,
    id: `evt_coding_${String(i + 1).padStart(4, "0")}`,
    type,
    sequence: i + 1,
    time: new Date(Date.UTC(2026, 7, 2, 0, 0, i + 1)).toISOString(),
    data,
  }));
}

// ---------------------------------------------------------------------------------------------
// THE SECOND TURN, AND IT IS HERE BECAUSE THE FIRST TURN WAS THE ONLY ONE ANYONE HAD EVER DRIVEN.
//
// MEASURED on the live stack 2026-08-02, session ses_0438bd21b5b6562890fabbec865c10ea: turn one
// ("selam") rendered; turn two ("build al projeyi") rendered "run queued", "run completed" and NOTHING
// ELSE. The run was fine — six tool calls and a full answer, both readable afterwards on
// GET /v1/responses/resp_47c7eb59…{,/tool-calls}. The screen dropped the entire turn.
//
// Cause: ONE SESSION, MANY RUNS. The adapter opened the journal at cursor 0, the server replayed run
// one, hit its `run.completed.v1`, and hung up (events.go:149). api/events.go:50 states the assumption
// in its own words — "an LP session carries a single run, so its terminal is the journal's end" — and a
// chat is the counterexample.
//
// So the fake's coding session now holds TWO runs in ONE journal, with run two's sequences continuing
// after run one's. A build that opens at cursor 0 sees run one and stops; only a build that starts past
// it renders this turn. The deterministic suite can now tell those two apart, which it could not before.
const CODING_RESPONSE_2_ID = "resp_coding_proof_0002";
const CODING_RUN_2_ID = "run_coding_proof_0002";

// How many turns this session has been asked for. The Nth create (N >= 2) is answered with run two.
let codingTurns = 0;

function codingEventsTurn2() {
  const base = {
    source: "palai://fake-control-plane",
    specversion: "1.0",
    session_id: CODING_SESSION_ID,
    run_id: CODING_RUN_2_ID,
    datacontenttype: "application/json",
  };
  const rows = [
    ["run.queued.v1", { run_id: CODING_RUN_2_ID, state: "queued" }],
    ["run.running.v1", { run_id: CODING_RUN_2_ID, state: "running" }],
    ["tool_call.executing.v1", { run_id: CODING_RUN_2_ID, replay_class: "reversible", tool_call_id: "tcall_turn2_build", tool_name: "palai.workspace.shell" }],
    ["tool_call.completed.v1", { run_id: CODING_RUN_2_ID, tool_call_id: "tcall_turn2_build", tool_name: "palai.workspace.shell" }],
    ["run.completed.v1", { run_id: CODING_RUN_2_ID, state: "completed" }],
  ];
  // Run one occupies sequences 1..N; run two continues from there, because they share one journal.
  const offset = codingEvents().length;
  return rows.map(([type, data], i) => ({
    ...base,
    id: `evt_coding_${String(offset + i + 1).padStart(4, "0")}`,
    type,
    sequence: offset + i + 1,
    time: new Date(Date.UTC(2026, 7, 2, 0, 1, i + 1)).toISOString(),
    data,
  }));
}

// The session's whole journal. A run's frames exist only once its create has been made — including run
// ONE. That is what the control plane does, and a fake that pre-populates a journal is a fake in which
// an adapter reading the cursor before it created anything would see events that cannot exist yet.
function codingJournal() {
  // A provisioning failure never reaches a model step: the workspace could not be prepared, so there is
  // no text, no tool call and nothing to stream. The journal is three frames and a terminal — which is
  // exactly why the UI's fallback path is the ONLY thing an operator sees on this turn, and why it
  // getting the sentence wrong mattered so much.
  if (codingFailsProvisioning) {
    const base = {
      source: "palai://fake-control-plane",
      specversion: "1.0",
      session_id: CODING_SESSION_ID,
      run_id: CODING_RUN_ID,
      datacontenttype: "application/json",
    };
    return [
      ["run.queued.v1", { run_id: CODING_RUN_ID, state: "queued" }],
      ["run.provisioning.v1", { run_id: CODING_RUN_ID, state: "provisioning" }],
      ["run.failed.v1", { run_id: CODING_RUN_ID, state: "failed" }],
    ].map(([type, data], i) => ({
      ...base,
      id: `evt_fail_${String(i + 1).padStart(4, "0")}`,
      type,
      sequence: i + 1,
      time: new Date(Date.UTC(2026, 7, 2, 0, 0, i + 1)).toISOString(),
      data,
    }));
  }
  if (codingTurns >= 2) return [...codingEvents(), ...codingEventsTurn2()];
  if (codingTurns >= 1) return codingEvents();
  return [];
}

const CODING_TURN2_TOOL_CALLS = [
  {
    id: "tcall_turn2_build",
    object: "tool_call",
    name: "palai.workspace.shell",
    state: "completed",
    replay_class: "reversible",
    // The command the live model eventually found, after six calls, once the hint stopped riding only
    // the first turn. It is the argument for putting the hint in `instructions`.
    arguments: { argv: ["swift", "build", "--package-path", "repo"] },
    result: { exit_code: 0, stdout: "Building for debugging...\n[4/4] Compiling PalaiDemo Greeter.swift\nBuild complete! (4.59s)\n" },
    created_at: "2026-08-02T00:01:03Z",
    updated_at: "2026-08-02T00:01:04Z",
  },
];

function handleCoding(method, pathname, request, response) {
  if (method === "POST" && pathname === "/v1/sessions") {
    request.resume();
    request.once("end", () => {
      sendJSON(response, 201, { id: CODING_SESSION_ID, object: "session", created_at: "2026-08-02T00:00:00Z" });
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/sessions/${CODING_SESSION_ID}/events`) {
    streamEvents(request, response, codingJournal());
    return true;
  }

  if (method === "GET" && pathname === `/v1/responses/${CODING_RESPONSE_2_ID}`) {
    sendJSON(response, 200, {
      id: CODING_RESPONSE_2_ID,
      object: "response",
      status: "completed",
      model: "fake",
      session_id: CODING_SESSION_ID,
      run_id: CODING_RUN_2_ID,
      created_at: "2026-08-02T00:01:00Z",
      output: [{ type: "message", content: "Build complete — `swift build --package-path repo` succeeded." }],
      usage: { input_tokens: 9, output_tokens: 12, total_tokens: 21, tool_calls: 1 },
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/responses/${CODING_RESPONSE_2_ID}/tool-calls`) {
    sendJSON(response, 200, { object: "list", data: CODING_TURN2_TOOL_CALLS });
    return true;
  }

  if (method === "GET" && pathname === `/v1/responses/${CODING_RESPONSE_ID}` && codingFailsProvisioning) {
    // `output` is EMPTY and stays empty — there is no answer, and waiting for one proves a negative.
    // `error` is where the truth is, and `status` says so on the first read.
    sendJSON(response, 200, {
      id: CODING_RESPONSE_ID,
      object: "response",
      status: "failed",
      model: "fake",
      session_id: CODING_SESSION_ID,
      run_id: CODING_RUN_ID,
      created_at: "2026-08-02T00:00:00Z",
      output: [],
      error: CODING_FAIL_ERROR,
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/responses/${CODING_RESPONSE_ID}`) {
    sendJSON(response, 200, {
      id: CODING_RESPONSE_ID,
      object: "response",
      status: "completed",
      model: "fake",
      session_id: CODING_SESSION_ID,
      run_id: CODING_RUN_ID,
      created_at: "2026-08-02T00:00:00Z",
      output: [{ type: "message", content: "Added CONTRIBUTING.md and requested a push." }],
      usage: { input_tokens: 11, output_tokens: 7, total_tokens: 18, tool_calls: 1 },
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/responses/${CODING_RESPONSE_ID}/tool-calls`) {
    // An empty list is what an uncommitted ledger looks like from here: the endpoint answers 200 with
    // no row for the call id the frames carried. It is NOT a 404 and NOT an error, which is precisely
    // why a single read could be mistaken for "this tool produced nothing".
    sendJSON(response, 200, { object: "list", data: codingUnjoinable ? [] : CODING_TOOL_CALLS });
    return true;
  }

  // PATCH /v1/sessions/{id} — the standing authorization. It applies ONLY the half the body names,
  // exactly as the control plane does, because that is the property the screen depends on: a click
  // on one switch must not move the other.
  if (method === "PATCH" && pathname === `/v1/sessions/${CODING_SESSION_ID}`) {
    if (brokenUpstream) {
      // Not problem+json, not JSON at all — exactly what a proxy, a crashed process or an empty 500
      // body looks like from the relay's side.
      response.writeHead(500, { "content-type": "text/plain; charset=utf-8" });
      response.end("");
      return true;
    }
    let raw = "";
    request.on("data", (chunk) => { raw += chunk; });
    request.once("end", () => {
      let body = {};
      try { body = JSON.parse(raw || "{}"); } catch { body = {}; }
      if (typeof body.auto_approve_tools === "boolean") codingAutoApprove.auto_approve_tools = body.auto_approve_tools;
      if (typeof body.auto_approve_publications === "boolean") codingAutoApprove.auto_approve_publications = body.auto_approve_publications;
      // The principal is stamped SERVER-SIDE from the verified key and never taken from the body —
      // the same rule the control plane applies, and reproducing it here is what lets the spec assert
      // the screen shows whose authority it is.
      codingAutoApprove.auto_approve_set_by =
        codingAutoApprove.auto_approve_tools || codingAutoApprove.auto_approve_publications ? "key:demo-operator" : "";
      sendJSON(response, 200, { id: CODING_SESSION_ID, object: "session", status: "active", ...codingAutoApprove });
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/responses/${CODING_RESPONSE_ID}/artifacts`) {
    sendJSON(response, 200, {
      object: "list",
      data: [
        { id: PATCH_ARTIFACT_ID, object: "artifact", run_id: CODING_RUN_ID, logical_type: "patch", media_type: "text/x-diff", size_bytes: PATCH_BODY.length },
        { id: TRANSCRIPT_ARTIFACT_ID, object: "artifact", run_id: CODING_RUN_ID, logical_type: "test-result", media_type: "text/plain", size_bytes: TRANSCRIPT_BODY.length },
      ],
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/artifacts/${PATCH_ARTIFACT_ID}/content`) {
    response.writeHead(200, { "content-type": "text/x-diff; charset=utf-8" });
    response.end(PATCH_BODY);
    return true;
  }
  if (method === "GET" && pathname === `/v1/artifacts/${TRANSCRIPT_ARTIFACT_ID}/content`) {
    response.writeHead(200, { "content-type": "text/plain; charset=utf-8" });
    response.end(TRANSCRIPT_BODY);
    return true;
  }

  if (method === "GET" && pathname === "/v1/repository-bindings") {
    sendJSON(response, 200, {
      data: [
        { id: "repo_fake_withcred", object: "repository_binding", provider: "github", repository_identity: "acme/with-credential", clone_url: "https://example.invalid/acme/with-credential.git", default_branch: "main", connection_ref: "gh-pat-acme" },
        { id: "repo_fake_nocred", object: "repository_binding", provider: "github", repository_identity: "acme/no-credential", clone_url: "https://example.invalid/acme/no-credential.git", default_branch: "trunk" },
        ...createdBindings,
      ],
      has_more: false,
    });
    return true;
  }

  if (method === "POST" && pathname === "/v1/repository-bindings") {
    readBody(request, (body) => {
      const parsed = safeJSON(body);
      // The control plane's own gate (api/repository_bindings.go:91): http(s) only.
      if (!/^https?:\/\//i.test(String(parsed.clone_url ?? ""))) {
        sendProblem(response, 400, "invalid_request");
        return;
      }
      const created = {
        id: `repo_fake_${createdBindings.length + 1}`,
        object: "repository_binding",
        provider: "github",
        repository_identity: parsed.repository_identity,
        clone_url: parsed.clone_url,
        default_branch: parsed.default_branch ?? "",
        ...(parsed.connection_ref ? { connection_ref: parsed.connection_ref } : {}),
      };
      createdBindings.push(created);
      sendJSON(response, 201, created);
    });
    return true;
  }

  if (method === "POST" && pathname === "/v1/secret-refs") {
    readBody(request, (body) => {
      const parsed = safeJSON(body);
      // MEASURED: this endpoint IS strict (identity/store.go:514 DisallowUnknownFields), unlike
      // /v1/repository-bindings. The fake refuses a third field for the same reason production does.
      const keys = Object.keys(parsed);
      if (keys.some((k) => k !== "name" && k !== "value")) {
        sendProblem(response, 400, "invalid_request");
        return;
      }
      if (!parsed.name) { sendProblem(response, 400, "invalid_request"); return; }
      if (!parsed.value) { sendProblem(response, 400, "invalid_request"); return; }
      createdSecrets.push(String(parsed.name));
      // The 201 body is METADATA ONLY — the value is never echoed.
      sendJSON(response, 201, { name: parsed.name, object: "secret_ref", version: 1, updated_at: "2026-08-02T00:00:00Z" });
    });
    return true;
  }

  if (method === "GET" && pathname === "/v1/approvals") {
    sendJSON(response, 200, {
      data: approvalDecision === null
        ? [{
            id: APPROVAL_ID,
            object: "approval",
            kind: "publication",
            run_id: CODING_RUN_ID,
            session_id: CODING_SESSION_ID,
            request_hash: REQUEST_HASH,
            created_at: "2026-08-02T00:00:00Z",
            publication_id: PUBLICATION_ID,
            operation: "push_branch",
            remote: "http://127.0.0.1:8177/demo-target.git",
            branch: "agent/ws_fake/run_fake",
            base: "main",
            head_sha: "4f4f89e3b2d083f46a9f539fc82220f5e1417630",
            credential_ref: "demo-local-token",
            credential: "this repository binding's own credential",
          }]
        : [],
      has_more: false,
    });
    return true;
  }

  if (method === "POST" && (pathname === `/v1/approvals/${APPROVAL_ID}/approve` || pathname === `/v1/approvals/${APPROVAL_ID}/deny`)) {
    readBody(request, (body) => {
      const parsed = safeJSON(body);
      // "an approval id alone authorizes nothing" — api/approvals.go:236.
      if (parsed.request_hash !== REQUEST_HASH) {
        sendProblem(response, 400, "invalid_request");
        return;
      }
      approvalDecision = pathname.endsWith("/approve") ? "approved" : "denied";
      sendJSON(response, 200, { id: APPROVAL_ID, object: "approval.decision", decision: approvalDecision });
    });
    return true;
  }

  // A TEST-CONTROL RESET, and it exists because the leak it fixes was real. `codingAutoApprove` is
  // module state on ONE server shared by every spec (workers: 1, fullyParallel: false), so a test
  // that armed the session left it armed for the next one — and two specs passed or failed purely on
  // the order they happened to run in. A test whose result depends on its neighbours is measuring
  // the suite, not the product.
  if (method === "POST" && pathname === "/__break-upstream") {
    brokenUpstream = true;
    request.resume();
    request.once("end", () => sendJSON(response, 200, { broken: true }));
    return true;
  }

  if (method === "POST" && pathname === "/__reset-coding") {
    brokenUpstream = false;
    codingAutoApprove.auto_approve_tools = false;
    codingAutoApprove.auto_approve_publications = false;
    codingAutoApprove.auto_approve_set_by = "";
    codingOmitsToolName = false;
    codingUnjoinable = false;
    codingFailsProvisioning = false;
    approvalDecision = null;
    codingTurns = 0;
    codingInstructions.length = 0;
    codingInputs.length = 0;
    codingAgentIds.length = 0;
    request.resume();
    request.once("end", () => sendJSON(response, 200, { reset: true }));
    return true;
  }

  // ------------------------------------------------------------------------------------------
  // THE AGENTS SURFACE. The demo resolves its agent BY NAME and provisions it if absent, so the
  // fake has to model the same four calls or the suite would be proving nothing about the path a
  // real deployment takes.
  //
  // IT REPRODUCES THE TWO PROPERTIES THAT MATTER, both measured on the live control plane:
  //   * `tools` ABSENT from a revision body stays ABSENT on the stored revision. automation/
  //     agents.go:60 — "Tools nil imposes no capability ceiling (a non-nil set — even empty — is
  //     the ceiling the resolver intersects)". A fake that helpfully defaulted `tools: []` would
  //     be modelling the EMPTY CEILING, which is the exact bug this omission avoids, and the
  //     suite would go green on a shape that grants the agent nothing.
  //   * a revision is created as a DRAFT and only `status: "published"` after the publish call.
  //     The demo looks for a published revision carrying its instructions; a fake that published
  //     on create would hide a missing publish step.
  if (method === "GET" && pathname === "/v1/agents") {
    sendJSON(response, 200, { object: "list", data: agentProfiles, has_more: false });
    return true;
  }
  if (method === "POST" && pathname === "/v1/agents") {
    readBody(request, (raw) => {
      const parsed = safeJSON(raw);
      if (!parsed.name) { sendProblem(response, 400, "invalid_request"); return; }
      const created = { id: `aprof_fake_${agentProfiles.length + 1}`, object: "agent", name: String(parsed.name) };
      agentProfiles.push(created);
      agentRevisions[created.id] = [];
      sendJSON(response, 201, created);
    });
    return true;
  }
  {
    const revList = /^\/v1\/agents\/([^/]+)\/revisions$/.exec(pathname);
    if (revList && method === "GET") {
      sendJSON(response, 200, { object: "list", data: agentRevisions[revList[1]] ?? [], has_more: false });
      return true;
    }
    if (revList && method === "POST") {
      readBody(request, (raw) => {
        const parsed = safeJSON(raw);
        // The real endpoint strictly decodes (DisallowUnknownFields, automation/agents.go:113), so a
        // field outside the executable-config subset is a 400 rather than something quietly dropped.
        const allowed = new Set(["model", "tools", "instructions", "tool_sets", "mcp_connections", "skills", "hooks", "environment"]);
        if (Object.keys(parsed).some((k) => !allowed.has(k))) { sendProblem(response, 400, "invalid_request"); return; }
        const list = agentRevisions[revList[1]];
        if (list === undefined) { sendProblem(response, 404, "not_found"); return; }
        const rev = {
          id: `arev_fake_${revList[1]}_${list.length + 1}`,
          object: "agent_revision",
          agent_id: revList[1],
          revision_number: list.length + 1,
          model: parsed.model ?? "",
          instructions: parsed.instructions ?? "",
          // ABSENT stays ABSENT — see the header. `null` is what the live API returns for a nil set.
          tools: Object.hasOwn(parsed, "tools") ? parsed.tools : null,
          status: "draft",
        };
        list.push(rev);
        sendJSON(response, 201, rev);
      });
      return true;
    }
    const pub = /^\/v1\/agents\/([^/]+)\/revisions\/([^/]+)\/publish$/.exec(pathname);
    if (pub && method === "POST") {
      const rev = (agentRevisions[pub[1]] ?? []).find((r) => r.id === pub[2]);
      if (rev === undefined) { sendProblem(response, 404, "not_found"); return true; }
      rev.status = "published";
      request.resume();
      request.once("end", () => sendJSON(response, 200, rev));
      return true;
    }
  }

  if (method === "GET" && pathname === "/__introspect-coding") {
    // `codingInstructions` and `codingInputs` are index-aligned: one entry per create, in order. A test
    // asserting the operator's bubble is clean has to also assert the hint STILL REACHED the model, or
    // deleting the hint entirely would satisfy it.
    sendJSON(response, 200, {
      approvalDecision,
      createdBindings,
      createdSecrets,
      codingInstructions,
      codingInputs,
      codingAgentIds,
      agentProfiles,
      agentRevisions,
      codingTurns,
    });
    return true;
  }

  return false;
}

function readBody(request, done) {
  let raw = "";
  request.on("data", (chunk) => { raw += chunk; });
  request.once("end", () => done(raw));
}

function safeJSON(raw) {
  try { return JSON.parse(raw || "{}"); } catch { return {}; }
}

const server = createServer((request, response) => {
  const url = new URL(request.url ?? "/", `http://127.0.0.1:${PORT}`);
  const { pathname } = url;
  const method = request.method ?? "GET";

  if (method === "GET" && pathname === "/healthz") {
    sendJSON(response, 200, { status: "ok" });
    return;
  }
  if (method === "GET" && pathname === "/__introspect") {
    sendJSON(response, 200, { cancelCalls, streamOpens, closes, terminalsSent });
    return;
  }

  // Every data endpoint is credential-gated: the SDK must present the server-side Bearer.
  if (bearer(request) === null) {
    sendProblem(response, 401, "authentication_required");
    return;
  }

  if (handleCoding(method, pathname, request, response)) {
    return;
  }

  if (method === "POST" && pathname === "/v1/responses") {
    // Drain the request body (the create request) before replying with the 202 handle.
    let createBody = "";
    request.on("data", (chunk) => { createBody += chunk; });
    request.once("end", () => {
      // A create carrying the coding session belongs to the coding scenario; everything else keeps
      // the original single-shot proof exactly as it was.
      if (safeJSON(createBody).session_id === CODING_SESSION_ID) {
        // The suite asks for the pre-E30 shape by saying so in the turn itself, which keeps the
        // scenario visible in the test that uses it rather than hidden in a setup call.
        codingOmitsToolName = String(safeJSON(createBody).input ?? "").includes("unnamed tool");
        codingUnjoinable = String(safeJSON(createBody).input ?? "").includes("unjoinable");
        codingFailsProvisioning = String(safeJSON(createBody).input ?? "").includes("unprovisionable");
        // THE INSTRUCTIONS LAYER IS RECORDED, because the whole point of moving the repository hint
        // off the user's text is that it still REACHES the model — on every turn. A test that only
        // asserted the bubble is clean would pass just as well if the hint had been deleted.
        codingInstructions.push(String(safeJSON(createBody).instructions ?? ""));
        codingAgentIds.push(String(safeJSON(createBody).agent_id ?? ""));
        codingInputs.push(String(safeJSON(createBody).input ?? ""));
        codingTurns += 1;
        const second = codingTurns >= 2;
        sendJSON(response, 202, {
          id: second ? CODING_RESPONSE_2_ID : CODING_RESPONSE_ID,
          object: "response",
          status: "queued",
          model: "fake",
          session_id: CODING_SESSION_ID,
          run_id: second ? CODING_RUN_2_ID : CODING_RUN_ID,
          created_at: second ? "2026-08-02T00:01:00Z" : "2026-08-02T00:00:00Z",
          output: [],
          usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
        });
        return;
      }
      sendJSON(response, 202, {
        id: RESPONSE_ID,
        object: "response",
        status: "queued",
        model: MODEL,
        session_id: SESSION_ID,
        run_id: RUN_ID,
        created_at: "2026-01-01T00:00:00Z",
        output: [],
        usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
      });
    });
    return;
  }

  if (method === "GET" && pathname === `/v1/sessions/${SESSION_ID}/events`) {
    streamEvents(request, response);
    return;
  }

  if (method === "GET" && pathname === `/v1/responses/${RESPONSE_ID}`) {
    sendJSON(response, 200, {
      id: RESPONSE_ID,
      object: "response",
      status: "completed",
      model: MODEL,
      session_id: SESSION_ID,
      run_id: RUN_ID,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:02Z",
      output: FINAL_OUTPUT,
      usage: USAGE,
    });
    return;
  }

  if (method === "POST" && pathname === `/v1/responses/${RESPONSE_ID}/cancel`) {
    cancelCalls += 1;
    sendJSON(response, 202, { id: RESPONSE_ID, status: "canceling" });
    return;
  }

  sendProblem(response, 404, "not_found");
});

server.listen(PORT, "127.0.0.1", () => {
  process.stdout.write(`fake-control-plane listening on http://127.0.0.1:${PORT}\n`);
});
