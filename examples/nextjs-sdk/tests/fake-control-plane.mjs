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

function streamEvents(request, response, events = scriptedEvents()) {
  streamOpens += 1;
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

const PATCH_BODY = [
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
    arguments: { command: "git -C repo add CONTRIBUTING.md" },
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
    arguments: { command: "xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build" },
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
    arguments: { command: "xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' test" },
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
    arguments: { command: "xcrun simctl list devices" },
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

let approvalDecision = null; // null | "approved" | "denied"
const createdBindings = [];
const createdSecrets = [];

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

function handleCoding(method, pathname, request, response) {
  if (method === "POST" && pathname === "/v1/sessions") {
    request.resume();
    request.once("end", () => {
      sendJSON(response, 201, { id: CODING_SESSION_ID, object: "session", created_at: "2026-08-02T00:00:00Z" });
    });
    return true;
  }

  if (method === "GET" && pathname === `/v1/sessions/${CODING_SESSION_ID}/events`) {
    streamEvents(request, response, codingEvents());
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
    sendJSON(response, 200, { object: "list", data: CODING_TOOL_CALLS });
    return true;
  }

  // PATCH /v1/sessions/{id} — the standing authorization. It applies ONLY the half the body names,
  // exactly as the control plane does, because that is the property the screen depends on: a click
  // on one switch must not move the other.
  if (method === "PATCH" && pathname === `/v1/sessions/${CODING_SESSION_ID}`) {
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
  if (method === "POST" && pathname === "/__reset-coding") {
    codingAutoApprove.auto_approve_tools = false;
    codingAutoApprove.auto_approve_publications = false;
    codingAutoApprove.auto_approve_set_by = "";
    codingOmitsToolName = false;
    approvalDecision = null;
    request.resume();
    request.once("end", () => sendJSON(response, 200, { reset: true }));
    return true;
  }

  if (method === "GET" && pathname === "/__introspect-coding") {
    sendJSON(response, 200, { approvalDecision, createdBindings, createdSecrets });
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
        sendJSON(response, 202, {
          id: CODING_RESPONSE_ID,
          object: "response",
          status: "queued",
          model: "fake",
          session_id: CODING_SESSION_ID,
          run_id: CODING_RUN_ID,
          created_at: "2026-08-02T00:00:00Z",
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
