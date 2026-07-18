import { test } from "node:test";
import assert from "node:assert/strict";

import { parseEventStream, ResponseStream, type StreamTransport } from "../src/stream.ts";
import type { Response as PalaiResponse } from "../src/generated/types.ts";

// --- SSE byte-stream helpers ------------------------------------------------------

// sseBody streams the given text chunks as a fetch-style ReadableStream, so a test can
// split frames across chunk boundaries exactly where it wants.
function sseBody(...chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(encoder.encode(chunk));
      }
      controller.close();
    },
  });
}

function sseResponse(...chunks: string[]): globalThis.Response {
  return new globalThis.Response(sseBody(...chunks), {
    status: 200,
    headers: { "content-type": "text/event-stream" },
  });
}

// eventFrame renders one CloudEvents envelope as an SSE frame (id + event + data).
function eventFrame(id: string, type: string): string {
  const data = JSON.stringify({
    specversion: "1.0",
    id,
    source: "palai",
    type,
    time: "2026-07-18T00:00:00Z",
    sequence: 1,
    data: {},
  });
  return `id: ${id}\nevent: ${type}\ndata: ${data}\n\n`;
}

const completedResponse: PalaiResponse = {
  id: "resp_1",
  object: "response",
  status: "completed",
  created_at: "2026-07-18T00:00:00Z",
  model: "fake-model",
  output: [{ type: "output_text", text: "done" }],
  usage: { input_tokens: 5, output_tokens: 3, total_tokens: 8 },
};

// FakeTransport scripts one SSE response per (re)connection and records the Last-Event-ID
// each connection was opened with, so a test can prove resumption.
class FakeTransport implements StreamTransport {
  readonly connections: Array<string | null> = [];
  #scripts: Array<() => globalThis.Response>;
  #retrieve: PalaiResponse;

  constructor(scripts: Array<() => globalThis.Response>, retrieve: PalaiResponse = completedResponse) {
    this.#scripts = scripts;
    this.#retrieve = retrieve;
  }

  openEventStream(_sessionID: string, lastEventId: string | null): Promise<globalThis.Response> {
    this.connections.push(lastEventId);
    const script = this.#scripts.shift();
    if (script === undefined) {
      throw new Error("FakeTransport ran out of scripted connections");
    }
    return Promise.resolve(script());
  }

  retrieveResponse(_responseID: string): Promise<PalaiResponse> {
    return Promise.resolve(this.#retrieve);
  }
}

function newStream(transport: FakeTransport, signal?: AbortSignal): ResponseStream {
  return new ResponseStream({
    transport,
    start: async () => ({ responseID: "resp_1", sessionID: "ses_1" }),
    ...(signal ? { signal } : {}),
    backoffBaseMs: 1,
    backoffMaxMs: 2,
  });
}

// --- low-level SSE parser ---------------------------------------------------------

test("parseEventStream frames id/event/data, joins multi-line data, ignores comments and CRLF", async () => {
  const body = sseBody(
    ": keep-alive\n",
    "id: e1\nevent: run.progress.v1\ndata: line-a\ndata: line-b\n\n",
    "id: e2\r\nevent: run.completed.v1\r\ndata: done\r\n\r\n",
  );
  const frames = [];
  for await (const frame of parseEventStream(body)) {
    frames.push(frame);
  }
  assert.equal(frames.length, 2);
  assert.deepEqual(frames[0], { id: "e1", event: "run.progress.v1", data: "line-a\nline-b" });
  assert.deepEqual(frames[1], { id: "e2", event: "run.completed.v1", data: "done" });
});

test("parseEventStream reassembles a frame split across chunk boundaries", async () => {
  // The event is delivered in three chunks that split mid-field and mid-line.
  const body = sseBody("id: e1\neve", "nt: run.progress.v1\nda", "ta: hello\n\n");
  const frames = [];
  for await (const frame of parseEventStream(body)) {
    frames.push(frame);
  }
  assert.deepEqual(frames, [{ id: "e1", event: "run.progress.v1", data: "hello" }]);
});

// --- typed AsyncIterable ----------------------------------------------------------

test("iterating a ResponseStream yields typed events up to and including the terminal", async () => {
  const transport = new FakeTransport([
    () => sseResponse(eventFrame("e1", "model_step.created.v1"), eventFrame("e2", "run.completed.v1")),
  ]);
  const types: string[] = [];
  for await (const event of newStream(transport)) {
    types.push(event.type);
  }
  assert.deepEqual(types, ["model_step.created.v1", "run.completed.v1"]);
  assert.equal(transport.connections.length, 1);
});

test("an unknown event type is delivered, not dropped (open enum)", async () => {
  const transport = new FakeTransport([
    () => sseResponse(eventFrame("e1", "some.brand.new.v9"), eventFrame("e2", "run.completed.v1")),
  ]);
  const types: string[] = [];
  for await (const event of newStream(transport)) {
    types.push(event.type);
  }
  assert.deepEqual(types, ["some.brand.new.v9", "run.completed.v1"]);
});

// --- resumption -------------------------------------------------------------------

test("a drop before the terminal reconnects from Last-Event-ID and dedupes the boundary", async () => {
  const transport = new FakeTransport([
    // First connection drops after e1 with no terminal event.
    () => sseResponse(eventFrame("e1", "run.progress.v1")),
    // Reconnect redelivers e1 inclusively, then completes with e2.
    () => sseResponse(eventFrame("e1", "run.progress.v1"), eventFrame("e2", "run.completed.v1")),
  ]);
  const ids: string[] = [];
  for await (const event of newStream(transport)) {
    ids.push(event.id);
  }
  // Two connections; the second resumed from the e1 cursor.
  assert.deepEqual(transport.connections, [null, "e1"]);
  // e1 is delivered exactly once despite the inclusive redelivery.
  assert.deepEqual(ids, ["e1", "e2"]);
});

// --- explicit cancel --------------------------------------------------------------

test("an explicit cancel stops the stream without reconnecting", async () => {
  const transport = new FakeTransport([
    () => sseResponse(eventFrame("e1", "run.progress.v1")), // ends without terminal
    () => sseResponse(eventFrame("e2", "run.completed.v1")), // must NOT be reached after cancel
  ]);
  const controller = new AbortController();
  const seen: string[] = [];
  for await (const event of newStream(transport, controller.signal)) {
    seen.push(event.id);
    controller.abort();
  }
  assert.deepEqual(seen, ["e1"]);
  // Aborted before the reconnect: only the first connection was ever opened.
  assert.equal(transport.connections.length, 1);
});

// --- finalResponse ----------------------------------------------------------------

test("finalResponse drains to the terminal and resolves the canonical Response", async () => {
  const transport = new FakeTransport([
    () => sseResponse(eventFrame("e1", "model_step.completed.v1"), eventFrame("e2", "run.completed.v1")),
  ]);
  const stream = newStream(transport);
  const final = await stream.finalResponse();
  assert.equal(final.status, "completed");
  assert.equal(final.model, "fake-model");
  assert.equal(final.output[0]?.type, "output_text");
});

// --- cancel during backoff --------------------------------------------------------

test("a cancel that lands during the reconnect backoff ends the stream quietly", async (t) => {
  // Pin the full-jitter so the backoff window is a known, comfortably wide 100ms; the abort
  // below fires well inside it, exercising the delay-abort path rather than the open path.
  t.mock.method(Math, "random", () => 0.5);
  const controller = new AbortController();
  let opens = 0;
  const transport: StreamTransport = {
    openEventStream() {
      opens += 1;
      // Every open fails, so #run enters the reconnect backoff; a second open would mean the
      // cancel failed to stop reconnection.
      return Promise.reject(new Error("connection dropped"));
    },
    retrieveResponse: () => Promise.resolve(completedResponse),
  };
  const stream = new ResponseStream({
    transport,
    start: async () => ({ responseID: "resp_1", sessionID: "ses_1" }),
    signal: controller.signal,
    backoffBaseMs: 200, // 0.5 jitter → a 100ms sleep
    backoffMaxMs: 200,
  });
  setTimeout(() => controller.abort(), 10); // land inside the 100ms backoff sleep

  const seen: string[] = [];
  // Must resolve quietly (no throw), like every other cancel path — not leak the AbortError.
  for await (const event of stream) {
    seen.push(event.id);
  }
  assert.deepEqual(seen, []);
  assert.equal(opens, 1, "canceled mid-backoff: the stream must not reconnect");
});
