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
const introspect = { v1Requests: 0, nonV1Requests: 0, beareredV1Requests: 0, unbeareredV1Requests: 0, paths: [] };

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

function sendProblem(response, status, code) {
  response.writeHead(status, { "content-type": "application/problem+json; charset=utf-8" });
  response.end(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, request_id: "req_fake_console" }));
}

function listView(data) {
  return { object: "list", data };
}
function page(data) {
  return { data, has_more: false, next_cursor: null, previous_cursor: null };
}

// The static admin fixtures — the §47.1 surface. Secret-ref rows are metadata only (name/version); a
// connection carries a secret REF name, never a value; an api-key row is metadata only.
const ADMIN = {
  organizations: listView([{ id: "org_local", object: "organization", display_name: "Local Org" }]),
  projects: listView([{ id: "proj_local", object: "project", organization_id: "org_local", display_name: "Default Project" }]),
  "api-keys": listView([{ id: "key_admin", object: "api_key", project_id: "proj_local", scopes: ["provision", "responses"], revoked_at: null }]),
  "model-connections": listView([{ id: "mc_1", object: "model_connection", provider: "fake", secret_ref: "provider-key" }]),
  "model-routes": listView([{ id: "mr_1", object: "model_route", name: "default" }]),
  "secret-refs": listView([{ name: "provider-key", version: 2, object: "secret_ref" }]),
  "knowledge-bases": listView([{ id: "kb_1", object: "knowledge_base", display_name: "Docs KB" }]),
  agents: page([{ id: "agt_1", object: "agent" }]),
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

export const ROUTES = [
  { method: "GET", pattern: "/v1/organizations", handle: adminList("organizations") },
  { method: "GET", pattern: "/v1/projects", handle: adminList("projects") },
  { method: "GET", pattern: "/v1/api-keys", handle: adminList("api-keys") },
  { method: "GET", pattern: "/v1/model-connections", handle: adminList("model-connections") },
  { method: "GET", pattern: "/v1/model-routes", handle: adminList("model-routes") },
  { method: "GET", pattern: "/v1/secret-refs", handle: adminList("secret-refs") },
  { method: "GET", pattern: "/v1/knowledge-bases", handle: adminList("knowledge-bases") },
  { method: "GET", pattern: "/v1/agents", handle: adminList("agents") },
  { method: "GET", pattern: "/v1/agents/{agent_id}/revisions", handle: (_req, res) => sendJSON(res, 200, AGENT_REVISIONS) },

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
