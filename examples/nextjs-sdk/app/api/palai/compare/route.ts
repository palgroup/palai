import { randomUUID } from "node:crypto";

import { PalaiAPIError, PalaiError, type Response as PalaiResponse } from "@palai/sdk";

import { getPalaiClient } from "@/lib/palai";
import { RawAPIVersion, rawBaseURL, rawHeaders } from "@/lib/raw";

// Both paths run on the Node runtime for the same reason app/api/palai/route.ts does: the SDK's
// server path uses node:crypto, and force-dynamic keeps the credential out of the static build.
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// POST runs THE SAME operation twice — once through @palai/sdk, once through a bare fetch to
// /v1/responses — and returns both outcomes side by side so the difference is visible rather than
// asserted.
//
// WHAT "RAW" MEANS HERE, because the honest answer is narrower than it looks. Both halves run on
// the SERVER. This is raw HTTP versus the SDK, NOT browser versus server: the browser still talks
// only to this handler, and the API key still never leaves the server. Palai deliberately dropped
// browser-direct tokens (sdks/typescript/README.md), so a "raw fetch from the browser" comparison
// would be demonstrating something the product refuses to do. The panel says so on screen.
//
// WHAT THE COMPARISON ACTUALLY TEACHES, and each line of it was measured against a live stack
// rather than reasoned about:
//
//   1. Idempotency-Key is MANDATORY and the SDK mints it for you. Omit it on the raw path and the
//      control plane answers 400 missing_idempotency_key ("the Idempotency-Key header is
//      required") — resources/responses.ts:42 mints one per create and reuses it across transport
//      retries. The raw path below mints its own; `omitIdempotencyKey` in the request body drives
//      it deliberately so the failure is demonstrable rather than described.
//   2. A create is 202 + `queued`, NOT a finished answer. Both paths must then poll (or stream).
//      The raw path shows the poll loop the SDK hides.
//   3. API-Version rides every SDK request (client.ts:19). The raw path sets it explicitly; a
//      caller who forgets it is pinned to whatever the server defaults to.
//   4. Errors arrive as RFC 9457 problem documents. The SDK narrows them to typed PalaiAPIError
//      with a stable `code` and `requestId`; the raw path gets the JSON and must read it itself.
export async function POST(request: Request): Promise<Response> {
  let body: CompareRequest;
  try {
    body = parseBody(await request.json());
  } catch (err) {
    return problemResponse(400, "invalid_request", err instanceof Error ? err.message : "invalid body");
  }

  // The session is created ONCE, on the SDK path, and both halves then post turns into it — that is
  // what makes "session" mean a shared conversation here rather than two unrelated runs. A
  // single-shot request creates no session and each half stands alone.
  let sessionID: string | null = null;
  if (body.mode === "session") {
    try {
      const session = await getPalaiClient().sessions.create();
      sessionID = session.id;
    } catch (err) {
      return problemResponse(502, "session_create_failed", describe(err));
    }
  }

  // SINGLE-SHOT RUNS THE TWO HALVES CONCURRENTLY; A SESSION RUNS THEM IN ORDER, and the difference
  // is not a style choice — it is what a session IS.
  //
  // A session is a SERIAL conversation. The first version of this handler used Promise.all for both
  // modes and the session mode failed intermittently: two turns posted into one session at the same
  // instant race, and one of them comes back with nothing. Measured, not predicted — it returned a
  // null status on one run and completed on the next with the request unchanged.
  //
  // Running them in order also makes the session mode demonstrate the thing it exists to
  // demonstrate: the raw half's turn arrives AFTER the SDK's, in the same conversation, so the
  // second turn's input_tokens carry the first turn's exchange. Concurrency would have shown two
  // unrelated runs that merely shared an id.
  let sdk: Outcome;
  let raw: Outcome;
  if (sessionID === null) {
    [sdk, raw] = await Promise.all([
      viaSDK(body.prompt, sessionID, request.signal),
      viaRawFetch(body.prompt, sessionID, body.omitIdempotencyKey === true, request.signal),
    ]);
  } else {
    sdk = await viaSDK(body.prompt, sessionID, request.signal);
    raw = await viaRawFetch(body.prompt, sessionID, body.omitIdempotencyKey === true, request.signal);
  }

  return Response.json(
    { mode: body.mode, sessionId: sessionID, sdk, raw },
    { headers: { "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" } },
  );
}

// viaSDK is the supported path: create, then poll to a terminal state. The idempotency key, the
// API-Version header, the retry policy and the problem-document narrowing are all the SDK's.
async function viaSDK(prompt: string, sessionID: string | null, signal: AbortSignal): Promise<Outcome> {
  const client = getPalaiClient();
  const started = Date.now();
  const steps: string[] = [];
  try {
    const created = await client.responses.create(
      { input: prompt, ...(sessionID ? { session_id: sessionID } : {}) },
      { signal },
    );
    steps.push(`create -> ${created.status} (${created.id})`);

    let current: PalaiResponse = created;
    for (let i = 0; i < MAX_POLLS && !isTerminal(current.status); i += 1) {
      await sleep(POLL_MS, signal);
      current = await client.responses.retrieve(created.id, { signal });
    }
    steps.push(`poll -> ${current.status}`);

    return {
      ok: isTerminal(current.status),
      transport: "@palai/sdk",
      responseId: current.id,
      status: current.status ?? null,
      model: current.model ?? null,
      text: outputText(current.output),
      usage: (current.usage ?? null) as unknown as Record<string, unknown> | null,
      elapsedMs: Date.now() - started,
      steps,
      // The SDK writes these for the caller. Listing them is the point of the comparison.
      handledForYou: [
        "Idempotency-Key minted per create and reused across retries",
        `API-Version: ${API_VERSION} on every request`,
        "RFC 9457 problem documents narrowed to typed PalaiAPIError (code + requestId)",
        "idempotent retry policy",
      ],
      error: null,
    };
  } catch (err) {
    return failed("@palai/sdk", started, steps, err);
  }
}

// viaRawFetch is the same operation with nothing between the caller and the wire: every header,
// the poll loop, and the problem-document handling are written out by hand.
async function viaRawFetch(
  prompt: string,
  sessionID: string | null,
  omitIdempotencyKey: boolean,
  signal: AbortSignal,
): Promise<Outcome> {
  const started = Date.now();
  const steps: string[] = [];
  const base = rawBaseURL();
  try {
    const headers = rawHeaders();
    if (!omitIdempotencyKey) {
      headers["Idempotency-Key"] = randomUUID();
    }

    const createRes = await fetch(`${base}/v1/responses`, {
      method: "POST",
      headers,
      body: JSON.stringify({ input: prompt, ...(sessionID ? { session_id: sessionID } : {}) }),
      signal,
    });
    const createBody = (await createRes.json()) as Record<string, unknown>;
    steps.push(`POST /v1/responses -> HTTP ${createRes.status}`);

    // A create is 202 + queued. Anything else is a problem document and the caller must read it.
    if (createRes.status !== 202) {
      return {
        ok: false,
        transport: "raw fetch",
        responseId: null,
        status: null,
        model: null,
        text: null,
        usage: null,
        elapsedMs: Date.now() - started,
        steps,
        handledForYou: RAW_HAND_WRITTEN,
        error: {
          code: String(createBody.code ?? "http_error"),
          requestId: (createBody.request_id as string) ?? null,
          detail: String(createBody.detail ?? createBody.title ?? `HTTP ${createRes.status}`),
        },
      };
    }

    const responseId = String(createBody.id ?? "");
    let current = createBody;
    for (let i = 0; i < MAX_POLLS && !isTerminal(current.status as string); i += 1) {
      await sleep(POLL_MS, signal);
      const pollRes = await fetch(`${base}/v1/responses/${encodeURIComponent(responseId)}`, {
        method: "GET",
        headers: rawHeaders(),
        signal,
      });
      current = (await pollRes.json()) as Record<string, unknown>;
    }
    steps.push(`GET /v1/responses/{id} -> ${String(current.status)}`);

    return {
      ok: isTerminal(current.status as string),
      transport: "raw fetch",
      responseId,
      status: (current.status as string) ?? null,
      model: (current.model as string) ?? null,
      text: outputText(current.output),
      usage: (current.usage ?? null) as Record<string, unknown> | null,
      elapsedMs: Date.now() - started,
      steps,
      handledForYou: RAW_HAND_WRITTEN,
      error: null,
    };
  } catch (err) {
    return failed("raw fetch", started, steps, err);
  }
}

const POLL_MS = 1_000;
const MAX_POLLS = 90;

// API_VERSION is what the panel DISPLAYS. It is imported from lib/raw.ts rather than re-typed, so
// the sentence on screen cannot drift from the header the raw request actually sends — the two
// used to be separate literals here and that is exactly the kind of second copy this tree keeps
// finding wrong.
const API_VERSION = RawAPIVersion;

// RAW_HAND_WRITTEN is the same list viaSDK reports as handled, phrased as work this path does
// itself — the two render side by side and the difference IS the demonstration.
const RAW_HAND_WRITTEN = [
  "Idempotency-Key minted by hand (omit it and the API answers 400 missing_idempotency_key)",
  `API-Version: ${API_VERSION} set by hand`,
  "problem documents parsed by hand (no typed error, no narrowing)",
  "poll loop written by hand",
];

interface CompareRequest {
  prompt: string;
  mode: "single" | "session";
  omitIdempotencyKey?: boolean;
}

interface Outcome {
  ok: boolean;
  transport: string;
  responseId: string | null;
  status: string | null;
  model: string | null;
  text: string | null;
  usage: Record<string, unknown> | null;
  elapsedMs: number;
  steps: string[];
  handledForYou: string[];
  error: { code: string; requestId: string | null; detail: string } | null;
}

function parseBody(input: unknown): CompareRequest {
  const body = (input ?? {}) as Record<string, unknown>;
  if (typeof body.prompt !== "string" || body.prompt.trim() === "") {
    throw new Error("a non-empty 'prompt' string is required");
  }
  const mode = body.mode === "session" ? "session" : "single";
  return {
    prompt: body.prompt,
    mode,
    ...(body.omitIdempotencyKey === true ? { omitIdempotencyKey: true } : {}),
  };
}

function failed(transport: string, started: number, steps: string[], err: unknown): Outcome {
  return {
    ok: false,
    transport,
    responseId: null,
    status: null,
    model: null,
    text: null,
    usage: null,
    elapsedMs: Date.now() - started,
    steps,
    handledForYou: transport === "raw fetch" ? RAW_HAND_WRITTEN : [],
    error: toError(err),
  };
}

function toError(err: unknown): { code: string; requestId: string | null; detail: string } {
  if (err instanceof PalaiAPIError) {
    return {
      code: err.code,
      requestId: err.requestId ?? null,
      detail: err.problem.detail ?? err.problem.title ?? err.code,
    };
  }
  if (err instanceof PalaiError) {
    return { code: "connection_error", requestId: null, detail: err.message };
  }
  return { code: "internal_error", requestId: null, detail: describe(err) };
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// isTerminal names the states a response stops in. `queued` and `in_progress` are the two it moves
// through — measured on a live stack: a create answers `queued`, and a run that calls tools reports
// `in_progress` until it finishes.
function isTerminal(status: string | null | undefined): boolean {
  return status === "completed" || status === "failed" || status === "cancelled" || status === "incomplete";
}

function outputText(output: unknown): string | null {
  if (!Array.isArray(output)) {
    return null;
  }
  const parts = output
    .map((item) => (item as Record<string, unknown>)?.content)
    .filter((content): content is string => typeof content === "string");
  return parts.length > 0 ? parts.join("\n") : null;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new Error("aborted"));
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(new Error("aborted"));
      },
      { once: true },
    );
  });
}

function problemResponse(status: number, code: string, detail: string): Response {
  return new Response(
    JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }),
    { status, headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" } },
  );
}
