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
//   * tool_call.executing.v1 carries {run_id, replay_class, tool_call_id} and NO NAME, so the test
//     can assert the screen says the name is missing. A fake that added `name` would delete the very
//     defect the demo is honest about.
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

let approvalDecision = null; // null | "approved" | "denied"
const createdBindings = [];
const createdSecrets = [];

function codingEvents() {
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
    ["tool_call.executing.v1", { run_id: CODING_RUN_ID, replay_class: "irreversible", tool_call_id: "tcall_coding_1" }],
    ["tool_call.completed.v1", { run_id: CODING_RUN_ID, tool_call_id: "tcall_coding_1" }],
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
