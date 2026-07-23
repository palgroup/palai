// SDK-conformance runner (E16 T2): the TypeScript leg of the shared, language-agnostic
// fixture corpus under tests/conformance/sdk/. It is NOT a test — it is a filter the Go
// harness drives: it reads {"vectors":[{category,name,input}]} on stdin, runs each vector
// through the REAL @palai/sdk surface, and writes {"outputs":[{category,name,output}]} on
// stdout as NORMALIZED JSON. The harness canonical-bytes-diffs that output against the
// corpus's expected output (and, in Wave 2, against the Python/Go runners' output). A
// category the SDK does not expose (signature-verify — the TS SDK ships no webhook verify)
// is simply omitted; the harness's reference decode still validates those vectors.
//
// This file is the STABLE runner contract T3/T4 mirror in their own language: same stdin
// envelope, same per-category output shapes documented in tests/conformance/sdk/README.md.

import { Palai } from "../src/client.ts";
import { errorForResponse } from "../src/errors.ts";
import { isTerminalEvent, parseEventStream } from "../src/stream.ts";
import type { Event } from "../src/generated/types.ts";

const BASE = "http://localhost:8080";

interface Vector {
  category: string;
  name: string;
  input: Record<string, unknown>;
}

interface Output {
  category: string;
  name: string;
  output: unknown;
}

// A sentinel the dispatcher returns for a vector this runner cannot decode, so the harness
// records it as "unsupported by TS" rather than a wrong answer.
const UNSUPPORTED = Symbol("unsupported");

async function readStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(chunk as Buffer);
  }
  return Buffer.concat(chunks).toString("utf8");
}

function streamFromString(text: string): ReadableStream<Uint8Array> {
  const bytes = new TextEncoder().encode(text);
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(bytes);
      controller.close();
    },
  });
}

// --- per-category decoders --------------------------------------------------------------
// request-encode / event-decode / error-map drive REAL @palai/sdk surfaces (client transport,
// parseEventStream, errorForResponse). unknown-field / envelope-decode are hand-rolled JSON
// projections: Page/ListView and the typed models are TypeScript interfaces erased at runtime,
// so there is no runtime decoder to call — the corpus stresses these against the struct-based
// Go/Python runners, where a naive decode WOULD strip unknowns or conflate the envelopes.

// requestEncode drives the resource method through a capturing fetch and reports the exact
// outgoing wire request (method, path, idempotency key, body) the SDK produced. The FIRST
// request wins the capture, so stream() — which fires the same create POST before opening the
// SSE GET — reports its create request, proving stream shares create's encoding.
async function requestEncode(input: Record<string, unknown>): Promise<unknown> {
  let captured: { method: string; url: string; idempotencyKey: string | null; body: string | undefined } | undefined;
  const captureFetch: typeof fetch = (info, init) => {
    const url = typeof info === "string" ? info : info.toString();
    if (captured === undefined) {
      const headers = new Headers(init?.headers ?? undefined);
      captured = {
        method: init?.method ?? "GET",
        url,
        idempotencyKey: headers.get("Idempotency-Key"),
        body: typeof init?.body === "string" ? init.body : undefined,
      };
    }
    // stream() opens the session SSE after the create POST: answer it with a single terminal
    // frame so finalResponse() resolves and the run ends cleanly.
    if (url.includes("/events")) {
      const terminal =
        'id: e1\nevent: run.completed.v1\ndata: {"specversion":"1.0","id":"e1","source":"palai","type":"run.completed.v1","time":"2026-07-18T00:00:00Z","sequence":1,"data":{}}\n\n';
      return Promise.resolve(
        new Response(streamFromString(terminal), { status: 200, headers: { "content-type": "text/event-stream" } }),
      );
    }
    return Promise.resolve(
      new Response(
        JSON.stringify({
          id: "resp_stub",
          session_id: "sess_stub",
          object: "response",
          status: "completed",
          model: "fake-1",
          output: [],
          usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
          created_at: "2026-07-18T00:00:00Z",
        }),
        { status: 202, headers: { "content-type": "application/json" } },
      ),
    );
  };

  const client = new Palai({ apiKey: "conf", baseURL: BASE, fetch: captureFetch });
  const method = input.method as string;
  const args = (input.args ?? {}) as Record<string, unknown>;
  const options = (input.options ?? {}) as Record<string, unknown>;

  if (input.resource !== "responses") {
    return UNSUPPORTED;
  }
  switch (method) {
    case "create":
      await client.responses.create(args as never, options as never);
      break;
    case "stream":
      await client.responses.stream(args as never, options as never).finalResponse();
      break;
    case "list":
      await client.responses.list(args as never);
      break;
    case "retrieve":
      await client.responses.retrieve(args.id as string);
      break;
    default:
      return UNSUPPORTED;
  }
  if (captured === undefined) {
    throw new Error(`request-encode: no request captured for ${method}`);
  }
  const path = captured.url.startsWith(BASE) ? captured.url.slice(BASE.length) : captured.url;
  const out: Record<string, unknown> = { method: captured.method, path };
  if (captured.idempotencyKey !== null) {
    out.idempotency_key = captured.idempotencyKey;
  }
  if (captured.body !== undefined) {
    out.body = JSON.parse(captured.body);
  }
  return out;
}

// eventDecode frames the SSE transcript through the SDK parser and decodes each data line
// to a canonical event, preserving unknown event types/fields and locating the terminal.
async function eventDecode(input: Record<string, unknown>): Promise<unknown> {
  const transcript = input.transcript as string;
  const events: unknown[] = [];
  let terminalIndex = -1;
  for await (const frame of parseEventStream(streamFromString(transcript))) {
    if (frame.data === "") {
      continue;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(frame.data);
    } catch {
      continue;
    }
    if (typeof parsed !== "object" || parsed === null || typeof (parsed as { type?: unknown }).type !== "string") {
      continue;
    }
    if (terminalIndex === -1 && isTerminalEvent(parsed as Event)) {
      terminalIndex = events.length;
    }
    events.push(parsed);
  }
  return { events, terminal_index: terminalIndex };
}

// errorMap projects a wire (status, body) pair to the SDK's typed error surface.
function errorMap(input: Record<string, unknown>): unknown {
  const err = errorForResponse(input.status as number, input.body as string, input.request_id as string | undefined);
  return {
    class: err.constructor.name,
    status: err.status,
    code: err.code,
    retryable: err.retryable,
    request_id: err.requestId ?? "",
  };
}

// unknownField proves the decode preserves an unknown field: a JSON round-trip keeps it.
function unknownField(input: Record<string, unknown>): unknown {
  return JSON.parse(JSON.stringify(input.value));
}

// envelopeDecode classifies and projects the two list envelopes (Page vs ListView).
function envelopeDecode(input: Record<string, unknown>): unknown {
  const env = input.envelope as Record<string, unknown>;
  if ("has_more" in env) {
    const out: Record<string, unknown> = { kind: "page", has_more: env.has_more, data: env.data };
    if (typeof env.next_cursor === "string") {
      out.next_cursor = env.next_cursor;
    }
    if (typeof env.previous_cursor === "string") {
      out.previous_cursor = env.previous_cursor;
    }
    return out;
  }
  if (env.object === "list") {
    return { kind: "list", object: env.object, data: env.data };
  }
  return UNSUPPORTED;
}

async function decode(v: Vector): Promise<unknown> {
  switch (v.category) {
    case "request-encode":
      return requestEncode(v.input);
    case "event-decode":
      return eventDecode(v.input);
    case "error-map":
      return errorMap(v.input);
    case "unknown-field":
      return unknownField(v.input);
    case "envelope-decode":
      return envelopeDecode(v.input);
    default:
      // signature-verify and any future category the SDK does not expose.
      return UNSUPPORTED;
  }
}

async function main(): Promise<void> {
  const request = JSON.parse(await readStdin()) as { vectors: Vector[] };
  const outputs: Output[] = [];
  for (const v of request.vectors) {
    const output = await decode(v);
    if (output !== UNSUPPORTED) {
      outputs.push({ category: v.category, name: v.name, output });
    }
  }
  process.stdout.write(JSON.stringify({ outputs }));
}

main().catch((err: unknown) => {
  process.stderr.write(`ts-runner: ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
  process.exit(1);
});
