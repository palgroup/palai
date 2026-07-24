import { test } from "node:test";
import assert from "node:assert/strict";

import { Palai } from "../src/client.ts";
import { Orchestrator, workflowIdempotencyKey, isTerminalStatus } from "../src/orchestrator.ts";

// --- test doubles -----------------------------------------------------------------

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string | undefined;
}

function recordingFetch(
  handler: (call: Call, attempt: number) => globalThis.Response,
): { fetch: typeof fetch; calls: Call[] } {
  const calls: Call[] = [];
  const fetchImpl = (async (input: unknown, init?: RequestInit) => {
    const call: Call = {
      url: String(input),
      method: init?.method ?? "GET",
      headers: (init?.headers ?? {}) as Record<string, string>,
      body: typeof init?.body === "string" ? init.body : undefined,
    };
    calls.push(call);
    return handler(call, calls.length);
  }) as unknown as typeof fetch;
  return { fetch: fetchImpl, calls };
}

function json(status: number, body: unknown): globalThis.Response {
  return new globalThis.Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

function newClient(fetchImpl: typeof fetch): Palai {
  return new Palai({ apiKey: "sk-test", baseURL: "http://palai.test", fetch: fetchImpl, backoffBaseMs: 1, backoffMaxMs: 2 });
}

// idempotentServer is a scripted FAKE control plane: POST /v1/responses dedupes by Idempotency-Key
// (same key ⇒ the ORIGINAL run replayed), GET /v1/responses/{id} returns the stored run at its
// current status. It is the server half of the single-retry-owner contract the kit relies on.
function idempotentServer(runStatusByIndex: string[] = ["completed"]): { fetch: typeof fetch; calls: Call[]; runs: number } {
  const byKey = new Map<string, { id: string; run_id: string; session_id: string }>();
  const polls = new Map<string, number>();
  let seq = 0;
  const state = { runs: 0 };
  const rec = recordingFetch((call) => {
    if (call.method === "POST" && call.url.endsWith("/v1/responses")) {
      const key = call.headers["Idempotency-Key"] ?? "";
      let run = byKey.get(key);
      if (run === undefined) {
        seq += 1;
        state.runs += 1;
        run = { id: `resp_${seq}`, run_id: `run_${seq}`, session_id: `ses_${seq}` };
        byKey.set(key, run);
      }
      return json(202, { ...run, object: "response", status: "queued", model: "", output: [], usage: {} });
    }
    if (call.method === "GET" && call.url.includes("/v1/responses/")) {
      const id = call.url.split("/v1/responses/")[1]!;
      const n = (polls.get(id) ?? 0) + 1;
      polls.set(id, n);
      const status = runStatusByIndex[Math.min(n - 1, runStatusByIndex.length - 1)]!;
      return json(200, { id, run_id: id.replace("resp", "run"), session_id: id.replace("resp", "ses"), object: "response", status, model: "", output: [{ type: "output_text", text: "done" }], usage: {} });
    }
    if (call.method === "POST" && call.url.includes("/cancel")) {
      return json(202, {});
    }
    if (call.method === "POST" && call.url.includes("/commands")) {
      return json(202, { id: "cmd_1", object: "command", kind: "send_message" });
    }
    return json(404, {});
  });
  return { fetch: rec.fetch, calls: rec.calls, get runs() { return state.runs; } } as { fetch: typeof fetch; calls: Call[]; runs: number };
}

// --- key derivation: the single retry owner ---------------------------------------

test("workflowIdempotencyKey is a stable, pure function of the workflow id", () => {
  assert.equal(workflowIdempotencyKey("wf-abc"), workflowIdempotencyKey("wf-abc"), "same workflow id must derive the same key");
  assert.notEqual(workflowIdempotencyKey("wf-abc"), workflowIdempotencyKey("wf-xyz"), "different workflow ids must derive different keys");
  assert.match(workflowIdempotencyKey("wf-abc"), /^wf_[0-9a-f]{32}$/);
  assert.throws(() => workflowIdempotencyKey(""), /must not be empty/);
});

test("isTerminalStatus separates settled runs from queued/running", () => {
  for (const s of ["completed", "failed", "canceled", "timed_out", "budget_exceeded", "expired"]) {
    assert.equal(isTerminalStatus(s), true, `${s} is terminal`);
  }
  for (const s of ["queued", "running"]) {
    assert.equal(isTerminalStatus(s), false, `${s} is not terminal`);
  }
});

// --- STEP 1: create with workflow-id metadata + derived key -----------------------

test("start tags the run with workflow_id metadata and the derived idempotency key, keeping the two identities separate", async () => {
  const srv = idempotentServer();
  const orch = new Orchestrator(newClient(srv.fetch));
  const run = await orch.start("wf-order-42", { input: "do the work" });

  const create = srv.calls.find((c) => c.method === "POST" && c.url.endsWith("/v1/responses"))!;
  assert.equal(create.headers["Idempotency-Key"], workflowIdempotencyKey("wf-order-42"), "the create carries the workflow-derived key");
  assert.deepEqual(JSON.parse(create.body!).metadata, { workflow_id: "wf-order-42" }, "the workflow id rides metadata");

  assert.equal(run.workflowId, "wf-order-42");
  assert.notEqual(run.runId, run.workflowId, "the canonical run id is NOT the workflow id");
  assert.ok(run.runId.startsWith("run_") && run.responseId.startsWith("resp_"), "the run identity is server-minted");
});

// --- STEP 5 / AUT-013: a retry STORM under the same workflow id settles ONE run ----

test("a retry storm under the same workflow id creates exactly ONE run (single retry owner, no multiplication)", async () => {
  const srv = idempotentServer();
  const orch = new Orchestrator(newClient(srv.fetch));

  // The scripted fake orchestrator replays the SAME workflow start 20 times concurrently — a storm.
  const runs = await Promise.all(Array.from({ length: 20 }, () => orch.start("wf-storm", { input: "x" })));

  const ids = new Set(runs.map((r) => r.runId));
  assert.equal(ids.size, 1, `the storm must collapse to one run, got ${ids.size}`);
  assert.equal(srv.runs, 1, "the server admitted exactly one run under the storm");
  // Every replay sent the identical key — the reason the server could dedupe.
  const keys = new Set(srv.calls.filter((c) => c.method === "POST" && c.url.endsWith("/v1/responses")).map((c) => c.headers["Idempotency-Key"]));
  assert.equal(keys.size, 1, "every replay carried the same derived key");
});

test("reconcile after a lost run id resolves to the SAME run by re-deriving the key", async () => {
  const srv = idempotentServer();
  const orch = new Orchestrator(newClient(srv.fetch));
  const original = await orch.start("wf-recover", { input: "x" });
  // Orchestrator crashed, kept only its workflow id + the request. Reconcile re-derives the key.
  const recovered = await orch.reconcile("wf-recover", { input: "x" });
  assert.equal(recovered.runId, original.runId, "reconcile-by-key returns the original run, not a duplicate");
  assert.equal(srv.runs, 1);
});

// --- STEP 2: the three wait modes -------------------------------------------------

test("waitByPoll polls until the run reaches a terminal status", async () => {
  // The run is queued for the first two polls, then completed.
  const srv = idempotentServer(["queued", "queued", "completed"]);
  const orch = new Orchestrator(newClient(srv.fetch));
  const run = await orch.start("wf-poll", { input: "x" });
  const terminal = await orch.waitByPoll(run, { pollIntervalMs: 1, timeoutMs: 5_000 });
  assert.equal(terminal.status, "completed");
  const polls = srv.calls.filter((c) => c.method === "GET").length;
  assert.ok(polls >= 3, `expected at least 3 polls, got ${polls}`);
});

test("waitByPoll throws on its own deadline WITHOUT canceling the run", async () => {
  const srv = idempotentServer(["queued"]); // never settles
  const orch = new Orchestrator(newClient(srv.fetch));
  const run = await orch.start("wf-timeout", { input: "x" });
  await assert.rejects(() => orch.waitByPoll(run, { pollIntervalMs: 1, timeoutMs: 5 }), /did not settle/);
  assert.equal(srv.calls.some((c) => c.url.includes("/cancel")), false, "a poll timeout must not cancel the run");
});

// --- STEP 3: command surface ------------------------------------------------------

test("cancel propagates cancellation to the canonical run", async () => {
  const srv = idempotentServer();
  const orch = new Orchestrator(newClient(srv.fetch));
  const run = await orch.start("wf-cancel", { input: "x" });
  await orch.cancel(run);
  const cancel = srv.calls.find((c) => c.url.includes("/cancel"))!;
  assert.ok(cancel.url.endsWith(`/v1/responses/${run.responseId}/cancel`), "cancel targets the canonical run id");
});

test("sendMessage steers the run's session", async () => {
  const srv = idempotentServer();
  const orch = new Orchestrator(newClient(srv.fetch));
  const run = await orch.start("wf-msg", { input: "x" });
  await orch.sendMessage(run, "focus on the tests");
  const cmd = srv.calls.find((c) => c.url.includes("/commands"))!;
  assert.ok(cmd.url.includes(`/v1/sessions/${run.sessionId}/commands`));
  assert.equal(JSON.parse(cmd.body!).message, "focus on the tests");
});

// --- composed activity ------------------------------------------------------------

test("runActivity composes start + poll wait into one durable activity", async () => {
  const srv = idempotentServer(["queued", "completed"]);
  const orch = new Orchestrator(newClient(srv.fetch));
  const terminal = await orch.runActivity("wf-activity", { input: "x" }, { wait: "poll", pollIntervalMs: 1 });
  assert.equal(terminal.status, "completed");
  assert.equal(srv.runs, 1, "the activity created one run");
});
