import { PalaiAPIError, PalaiError, type Event, type Response as PalaiResponse } from "@palai/sdk";

import { getPalaiClient } from "@/lib/palai";
import { requireSession } from "@/lib/session";
import { laneFor } from "@/lib/timeline";

// The SDK's server path uses node:crypto, so this runs on the Node runtime; force-dynamic keeps the
// credential out of the static build.
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// POST starts a run and re-projects the SDK's canonical Event stream to the browser as newline-delimited
// JSON. It forwards ONLY curated canonical fields, sorted into the §47.2 lanes by laneFor — never the raw
// provider payload and never the credential. The browser talks only to this handler; the API key stays
// server-side (lib/palai.ts). The stream stays open across a waiting_for_approval pause: the browser
// resolves the approval through the /v1/* command relay, and the upstream then continues to terminal.
//
// The first frame is a `meta` carrying the session + response id so the browser can address the approve
// command and list the run's artifacts — both through the same /v1/* relay, never a backchannel.
//
// THE GATE IS THE FIRST THING THIS METHOD DOES (E25 T1). This handler is the console's most expensive
// unauthenticated surface if left open: a POST here STARTS A RUN, which spends a model budget and can drive
// real side effects. It is counted alongside the four /v1 relay methods by tests/relay-gate.spec.ts.
export async function POST(request: Request): Promise<Response> {
  const refused = requireSession(request);
  if (refused !== null) return refused;

  let prompt: string;
  // The OPTIONAL pin (E25 T6). A run may be pinned to a PUBLISHED agent revision, and the field is optional
  // deliberately rather than incidentally: with no revision the upstream body is `{input}` and NOTHING else,
  // bit-identical to what this handler has always sent, so /runs' existing behaviour and every assertion in
  // tests/journey.spec.ts hold without an edit. An empty string is refused rather than forwarded — the API
  // would have to decide what a revision named "" means, and the honest answer is that the browser sent a
  // control it should not have.
  let agentRevisionID = "";
  // The OPTIONAL §22.7 output contract. Same shape of decision as the pin above: absent means the
  // upstream body carries no `output` key at all, so an unconstrained run is bit-identical to what
  // this handler has always sent.
  //
  // THE SCHEMA IS PARSED HERE AND NOT ONLY IN THE BROWSER. The browser parses it too, to show the
  // operator a syntax error next to the box they typed it in rather than after a round trip — but a
  // relay that trusted the browser's parse would be trusting a client with what it forwards to an
  // authenticated upstream. The API refuses an unenforceable schema either way; this is about
  // sending a well-formed request, not about the API's own guarantee.
  let outputSchema: Record<string, unknown> | null = null;
  // THE OPTIONAL SESSION, AND ITS ABSENCE IS THE DIFFERENCE BETWEEN THE TWO THINGS THIS PAGE CAN DO.
  //
  // MEASURED IN THE CODE RATHER THAN INFERRED (packages/coordinator/store.go:742 and
  // apps/control-plane/api/responses.go:289): with no `session_id` the admission mints a fresh session
  // (`createSession = true`) and the run is a SINGLE SHOT — one request, one response, nothing before it.
  // With one, admission resolves that session inside the tenant, requires it to be ACTIVE, and appends to
  // it: `sessionID, createSession = existingID, false`. The model then sees the earlier turns.
  //
  // THE CONSOLE COULD NEVER DO THE SECOND ONE. `session_id` has been on the wire contract the whole time
  // (protocols/schemas/execution/response-create.json declares it, sdks/typescript's
  // ResponseCreateRequest carries it), and app/runs/page.tsx only ever READ the id back off the `meta`
  // frame — `setSessionId(String(f.sessionId ?? ""))` — and never sent one. So every run this console has
  // ever started opened a new session, and the owner's "single shot bir şey görmüyorum" is exactly right:
  // there was nothing on the screen that said which of the two was happening, because only one of them
  // could.
  //
  // Absent means the upstream body carries NO `session_id` key, so an unchained run stays bit-identical to
  // what this handler has always sent — the same rule the pin and the output contract above follow.
  let sessionID = "";
  try {
    const body = (await request.json()) as { prompt?: unknown; agent_revision_id?: unknown; output_schema?: unknown; session_id?: unknown };
    if (typeof body.prompt !== "string" || body.prompt.trim() === "") {
      return problemResponse(400, "invalid_request", "a non-empty 'prompt' string is required");
    }
    prompt = body.prompt;
    if (body.agent_revision_id !== undefined && body.agent_revision_id !== null) {
      if (typeof body.agent_revision_id !== "string" || body.agent_revision_id.trim() === "") {
        return problemResponse(400, "invalid_request", "'agent_revision_id', when present, must be a non-empty string");
      }
      agentRevisionID = body.agent_revision_id;
    }
    if (body.session_id !== undefined && body.session_id !== null && body.session_id !== "") {
      if (typeof body.session_id !== "string") {
        return problemResponse(400, "invalid_request", "'session_id', when present, must be a non-empty string");
      }
      sessionID = body.session_id;
    }
    if (body.output_schema !== undefined && body.output_schema !== null && body.output_schema !== "") {
      if (typeof body.output_schema !== "string") {
        return problemResponse(400, "invalid_request", "'output_schema', when present, must be a JSON string");
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(body.output_schema);
      } catch (err) {
        return problemResponse(400, "invalid_request", `'output_schema' is not valid JSON: ${(err as Error).message}`);
      }
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
        return problemResponse(400, "invalid_request", "'output_schema' must be a JSON object");
      }
      outputSchema = parsed as Record<string, unknown>;
    }
  } catch {
    return problemResponse(400, "invalid_request", "request body must be JSON with a 'prompt' string");
  }

  const client = getPalaiClient();
  const encoder = new TextEncoder();

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      const enqueue = (obj: Record<string, unknown>) => {
        controller.enqueue(encoder.encode(`${JSON.stringify(obj)}\n`));
      };
      try {
        const palaiStream = client.responses.stream(
          // The spreads are what keep the plain body bit-unchanged: an absent pin and an absent
          // output contract each add no key at all.
          {
            input: prompt,
            ...(sessionID === "" ? {} : { session_id: sessionID }),
            ...(agentRevisionID === "" ? {} : { agent_revision_id: agentRevisionID }),
            ...(outputSchema === null ? {} : { output: { format: "json_schema", schema: outputSchema, strict: true } }),
          },
          { signal: request.signal },
        );
        enqueue({ type: "status", status: "streaming" });

        let metaSent = false;
        for await (const event of palaiStream) {
          // Emit the session + response id as soon as the create has resolved (before the first
          // projected event), so the browser can address the approve/deny command and the artifact
          // list mid-stream — both through the /v1/* relay, never a backchannel.
          if (!metaSent && palaiStream.sessionID) {
            enqueue({ type: "meta", sessionId: palaiStream.sessionID, responseId: palaiStream.responseID ?? null });
            metaSent = true;
          }
          enqueue(projectEvent(event));
        }

        // The stream reached its terminal. If the browser is still connected, project the canonical
        // terminal Response — status, the server-selected model, output, usage.
        if (!request.signal.aborted && palaiStream.responseID) {
          const final = await client.responses.retrieve(palaiStream.responseID);
          enqueue(projectFinal(final));
        }
      } catch (err) {
        if (!request.signal.aborted) {
          enqueue(projectError(err));
        }
      } finally {
        try {
          controller.close();
        } catch {
          // Already closed because the browser disconnected — nothing to do.
        }
      }
    },
  });

  return new Response(stream, {
    headers: {
      "Content-Type": "application/x-ndjson; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

// projectEvent maps a canonical Event to a curated, lane-tagged browser projection. Each lane carries
// only the fields the timeline renders. The approval lane is the SECURITY-CRITICAL one (UI-002): it
// forwards the AUTHORITATIVE detail the approval.requested.v1 event ACTUALLY carries
// (packages/coordinator/publication.go: operation / branch / request_hash) as distinct fields AND the
// proposal-supplied `display` string SEPARATELY — the UI renders the authoritative detail and never lets
// the display substitute for it. There is no publications/approvals READ endpoint, so the event payload is
// all the console has; richer detail (action/args/diff/destination/risk/expiry) is a filed public-API GAP
// (README) the console will render when the API emits it.
function projectEvent(event: Event): Record<string, unknown> {
  const data = (event.data ?? {}) as Record<string, unknown>;
  const lane = laneFor(event.type);
  const head = { lane, type: event.type, sequence: event.sequence, time: event.time };
  switch (lane) {
    case "model_step":
      return typeof data.text === "string" ? { ...head, text: data.text } : head;
    case "tool":
      return {
        ...head,
        tool: {
          id: data.tool_call_id ?? data.child_id ?? null,
          name: data.name ?? null,
          arguments: data.arguments ?? null,
          result: data.result ?? null,
          progress: data.progress ?? null,
        },
      };
    case "approval":
      return {
        ...head,
        approval: {
          id: data.publication_id ?? null,
          resolution: approvalResolution(event.type),
          // The AUTHORITATIVE, model-independent detail the event actually carries (publication.go).
          operation: data.operation ?? null,
          branch: data.branch ?? null,
          request_hash: data.request_hash ?? null,
          // The proposal-supplied string — kept SEPARATE so the UI never substitutes it for the detail above.
          display: data.display ?? null,
        },
      };
    case "recovery":
      return { ...head, recovery: { attempt: data.attempt ?? null, detail: data.detail ?? null, checkpoint_id: data.checkpoint_id ?? null } };
    case "usage":
      return { ...head, usage: pickUsage(data) };
    default:
      return head;
  }
}

function approvalResolution(type: string): string | null {
  switch (type) {
    case "approval.approved.v1":
      return "approved";
    case "approval.denied.v1":
      return "denied";
    case "approval.expired.v1":
      return "expired";
    default:
      return null; // approval.requested.v1 — still pending
  }
}

function projectFinal(final: PalaiResponse): Record<string, unknown> {
  const projection: Record<string, unknown> = {
    type: "final",
    lane: "terminal",
    status: final.status,
    model: final.model,
    output: final.output,
    usage: pickUsage((final.usage ?? {}) as unknown as Record<string, unknown>),
  };
  if (final.error) {
    projection.error = { code: final.error.code, requestId: final.error.request_id, detail: final.error.detail ?? final.error.title };
  }
  return projection;
}

function projectError(err: unknown): Record<string, unknown> {
  if (err instanceof PalaiAPIError) {
    return { type: "error", lane: "terminal", code: err.code, requestId: err.requestId ?? null, status: err.status, detail: err.problem.detail ?? err.problem.title ?? err.code };
  }
  if (err instanceof PalaiError) {
    return { type: "error", lane: "terminal", code: "connection_error", requestId: null, detail: err.message };
  }
  return { type: "error", lane: "terminal", code: "internal_error", requestId: null, detail: "the response stream failed" };
}

function pickUsage(data: Record<string, unknown>): Record<string, unknown> {
  return {
    input_tokens: numberOr(data.input_tokens),
    output_tokens: numberOr(data.output_tokens),
    total_tokens: numberOr(data.total_tokens),
    tool_calls: numberOr(data.tool_calls),
  };
}

function numberOr(value: unknown): number | null {
  return typeof value === "number" ? value : null;
}

function problemResponse(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}
