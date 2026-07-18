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

function streamEvents(request, response) {
  streamOpens += 1;
  response.writeHead(200, {
    "content-type": "text/event-stream; charset=utf-8",
    "cache-control": "no-cache, no-transform",
    connection: "keep-alive",
    "x-accel-buffering": "no",
  });

  const events = scriptedEvents();
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

  if (method === "POST" && pathname === "/v1/responses") {
    // Drain the request body (the create request) before replying with the 202 handle.
    request.resume();
    request.once("end", () => {
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
