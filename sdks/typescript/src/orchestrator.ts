// The orchestrator kit is server-only: it drives durable runs with the API-key client, so
// importing it marks its importer server-only (never bundle it for the browser — see ./server-only.ts).
import "./server-only.ts";

import { createHash } from "node:crypto";

import type { Command, Response as PalaiResponse, ResponseCreateRequest } from "./generated/types.ts";
import type { Palai } from "./client.ts";
import { delay, ResponseStream } from "./stream.ts";

// ─────────────────────────────────────────────────────────────────────────────────────────
// External-orchestrator kit — the §35.1 five-step contract as thin helpers over the client.
//
// This kit lets an EXTERNAL durable orchestrator (Temporal, Restate, a CI pipeline, or a plain
// script) drive a Palai run through the canonical five steps without re-implementing any of the
// client's HTTP, idempotent-retry, or reconnecting-SSE machinery — it composes `client.responses`
// and `client.sessions.commands`. It writes NO vendor adapter: the contract it pins is the same
// one ANY orchestrator follows (docs/orchestrator-kit.md, §35.2).
//
// The two identities the contract keeps SEPARATE — never conflated:
//   • workflowId  — the EXTERNAL orchestrator's durable workflow identity. The orchestrator owns it
//                   and may replay it from its own history any number of times (a retry storm).
//   • runId / responseId / sessionId — Palai's CANONICAL run identity, minted by the server on the
//                   FIRST admission. The workflow id never replaces it and is never inferred from it.
//
// The bridge between them is the idempotency key, derived deterministically from the workflow id
// (workflowIdempotencyKey). It is the SINGLE retry owner (§35.2): every replay under the same
// workflow id carries the same key, so the server settles exactly ONE run — a storm of retries does
// not multiply runs (AUT-013). An orchestrator that lost its state re-derives the same key from its
// own workflow id and reconciles to the same run without ever having persisted the run id.
// ─────────────────────────────────────────────────────────────────────────────────────────

// Terminal run statuses (spec §22.3): a run in one of these has settled. queued/running are the only
// non-terminal states. A terminal-FAILED run is a real OUTCOME, so a wait RESOLVES with it (the caller
// inspects `status` / `error`) rather than throwing.
const TERMINAL_STATUSES = new Set(["completed", "failed", "canceled", "timed_out", "budget_exceeded", "expired"]);

export function isTerminalStatus(status: string): boolean {
  return TERMINAL_STATUSES.has(status);
}

// WorkflowRun binds the external workflow id to Palai's canonical run identity + the derived key. It is
// the handle every step takes; the orchestrator persists it (or just the workflowId + request, and
// re-derives the rest via reconcile).
export interface WorkflowRun {
  /** The external orchestrator's workflow identity (its own, replay-stable). */
  readonly workflowId: string;
  /** Derived from workflowId — the single retry owner that collapses replays to one run. */
  readonly idempotencyKey: string;
  /** Palai's canonical run identity (server-minted; never equal to workflowId). */
  readonly responseId: string;
  readonly runId: string;
  readonly sessionId: string;
}

export interface StartOptions {
  /** Override the derived key (advanced — the default derives it from workflowId, which is what makes
   *  a replay safe). Supply your own only to dedupe across an identity the workflow id does not capture. */
  idempotencyKey?: string;
  /** A signed callback registered at create for the WEBHOOK wait mode (spec §35.1 step 2). The URL rides
   *  the request's `callback` field; Palai POSTs the terminal result to it. The body is untrusted — the
   *  receiver reconciles by reading the canonical result back (result()), it does not trust the payload. */
  callback?: Record<string, unknown>;
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
}

export interface WaitOptions {
  signal?: AbortSignal | undefined;
  /** The ORCHESTRATOR's wait deadline — distinct from the run's own execution deadline, which Palai
   *  owns (§35.2). Exceeding it throws; the run keeps going and can be reconciled later. */
  timeoutMs?: number;
  /** Poll cadence for waitByPoll. */
  pollIntervalMs?: number;
  /** Resume cursor for waitByStream after a persisted disconnect. */
  lastEventId?: string | null;
}

export interface RunActivityOptions extends StartOptions {
  /** Which wait mode the activity blocks on. "poll" and "stream" wait in-process; "webhook" returns
   *  immediately after start (the orchestrator's callback handler resolves it out of band). */
  wait?: "poll" | "stream" | "webhook";
  waitTimeoutMs?: number;
  pollIntervalMs?: number;
}

// workflowIdempotencyKey derives the stable per-workflow key. Pure function of the workflow id: same id
// ⇒ same key (replays reconcile), different id ⇒ different key. sha256-truncated so an arbitrary external
// workflow id maps to a bounded, opaque key.
export function workflowIdempotencyKey(workflowId: string): string {
  if (workflowId === "") {
    throw new Error("@palai/sdk: an orchestrator workflow id must not be empty");
  }
  return "wf_" + createHash("sha256").update(workflowId).digest("hex").slice(0, 32);
}

// Orchestrator is the kit surface: one instance wraps a Palai client and exposes the five-step contract.
export class Orchestrator {
  #client: Palai;

  constructor(client: Palai) {
    this.#client = client;
  }

  // start — STEP 1: create-with-workflow-ID-metadata + idempotency-key. The external workflow id rides
  // `metadata.workflow_id` (untrusted external correlation, never an identity override — §38.6); the
  // idempotency key is derived from it, so this exact call is safe to REPLAY. A callback in options wires
  // the webhook wait mode. Returns the WorkflowRun carrying BOTH identities.
  async start(workflowId: string, request: ResponseCreateRequest, options: StartOptions = {}): Promise<WorkflowRun> {
    const idempotencyKey = options.idempotencyKey ?? workflowIdempotencyKey(workflowId);
    const body: ResponseCreateRequest = {
      ...request,
      metadata: { ...request.metadata, workflow_id: workflowId },
      ...(options.callback !== undefined ? { callback: options.callback } : {}),
    };
    const created = await this.#client.responses.create(body, {
      idempotencyKey,
      signal: options.signal,
      ...(options.timeoutMs !== undefined ? { timeoutMs: options.timeoutMs } : {}),
    });
    return toWorkflowRun(workflowId, idempotencyKey, created);
  }

  // reconcile — STEP 5: reconcile-by-key. Re-issues start under the SAME derived key. Because the key is
  // the single retry owner, the server REPLAYS the original admission and returns the SAME run — so an
  // orchestrator recovering from a crash (holding only its workflow id + the original request) resolves
  // back to the one run it created, never a duplicate. This is start()'s idempotency made explicit as the
  // recovery entrypoint; call it with the identical request the workflow first submitted.
  reconcile(workflowId: string, request: ResponseCreateRequest, options: StartOptions = {}): Promise<WorkflowRun> {
    return this.start(workflowId, request, options);
  }

  // waitByPoll — STEP 2 (poll): polls the run to its terminal projection. The orchestrator owns this wait
  // deadline; a timeout throws WITHOUT canceling the run (reconcile/result recover it later).
  async waitByPoll(run: WorkflowRun, options: WaitOptions = {}): Promise<PalaiResponse> {
    const deadline = Date.now() + (options.timeoutMs ?? 60_000);
    const interval = options.pollIntervalMs ?? 250;
    for (;;) {
      const resp = await this.#client.responses.retrieve(run.responseId, { signal: options.signal });
      if (isTerminalStatus(resp.status)) {
        return resp;
      }
      if (Date.now() + interval > deadline) {
        throw new Error(`@palai/sdk: run ${run.runId} did not settle within the ${options.timeoutMs ?? 60_000}ms poll deadline`);
      }
      await delay(interval, options.signal);
    }
  }

  // waitByStream — STEP 2 (SSE): drains the run's session event stream to its terminal event and returns
  // the canonical terminal Response. It reuses ResponseStream, so a transport drop reconnects with
  // Last-Event-ID (loss-less) — the kit adds no SSE code of its own. start() runs once here and returns
  // the ALREADY-created identity, so the wait subscribes without minting a second run.
  waitByStream(run: WorkflowRun, options: WaitOptions = {}): Promise<PalaiResponse> {
    const stream = new ResponseStream({
      transport: this.#client,
      start: async () => ({ responseID: run.responseId, sessionID: run.sessionId }),
      signal: options.signal,
      lastEventId: options.lastEventId ?? null,
    });
    return stream.finalResponse();
  }

  // result — reads the canonical terminal Response by run identity. The WEBHOOK wait mode resolves here:
  // when Palai POSTs the callback, the orchestrator's handler calls result() to READ BACK the canonical
  // outcome rather than trusting the (untrusted) webhook body. STEP 4's structured result + artifacts ride
  // this Response (output + metadata; artifacts via client.artifacts.listForResponse).
  result(run: WorkflowRun, options: WaitOptions = {}): Promise<PalaiResponse> {
    return this.#client.responses.retrieve(run.responseId, { signal: options.signal });
  }

  // sendMessage — STEP 3 (message command): delivers a steer message to the run's session. Durable +
  // idempotent server-side (the command_id), so an orchestrator activity retry does not double-deliver.
  sendMessage(run: WorkflowRun, message: string, options: { commandId?: string; signal?: AbortSignal } = {}): Promise<Command> {
    return this.#client.sessions.commands.steer(
      run.sessionId,
      { message, ...(options.commandId !== undefined ? { commandId: options.commandId } : {}) },
      { ...(options.signal !== undefined ? { signal: options.signal } : {}) },
    );
  }

  // cancel — STEP 3 (cancel command): propagates cancellation to the run. Naturally idempotent (a canceled
  // terminal is monotonic), so a retried cancel settles once. This is cancel PROPAGATION: the orchestrator
  // canceling its workflow tells Palai to cancel the run it owns.
  cancel(run: WorkflowRun, options: { signal?: AbortSignal } = {}): Promise<void> {
    return this.#client.responses.cancel(run.responseId, { ...(options.signal !== undefined ? { signal: options.signal } : {}) });
  }

  // runActivity composes the happy path into ONE durable activity an orchestrator schedules: start
  // (idempotent) → wait (chosen mode) → return the terminal Response (structured result + artifacts). On
  // an orchestrator retry with the same workflowId it replays to the same run and resumes the wait — no
  // duplicate run. The webhook mode returns the started (possibly still-running) Response immediately; the
  // orchestrator's callback handler resolves it with result().
  async runActivity(workflowId: string, request: ResponseCreateRequest, options: RunActivityOptions = {}): Promise<PalaiResponse> {
    const run = await this.start(workflowId, request, options);
    const waitOpts: WaitOptions = {
      signal: options.signal,
      ...(options.waitTimeoutMs !== undefined ? { timeoutMs: options.waitTimeoutMs } : {}),
      ...(options.pollIntervalMs !== undefined ? { pollIntervalMs: options.pollIntervalMs } : {}),
    };
    switch (options.wait ?? "poll") {
      case "stream":
        return this.waitByStream(run, waitOpts);
      case "webhook":
        return this.result(run, waitOpts);
      default:
        return this.waitByPoll(run, waitOpts);
    }
  }
}

// toWorkflowRun projects a created Response into the WorkflowRun handle, coercing the branded id types to
// plain strings.
function toWorkflowRun(workflowId: string, idempotencyKey: string, created: PalaiResponse): WorkflowRun {
  return {
    workflowId,
    idempotencyKey,
    responseId: String(created.id),
    runId: String(created.run_id ?? ""),
    sessionId: String(created.session_id ?? ""),
  };
}
