import type { Event, Response as PalaiResponse } from "./generated/types.ts";
import { errorForResponse, PalaiConnectionError } from "./errors.ts";

// A parsed Server-Sent Events frame (WHATWG event stream format). `data` is the joined
// data lines; `id` updates the reconnection cursor; `event` is the event name the server
// set. A frame with neither data nor event (a bare heartbeat comment) is never emitted.
export interface SSEFrame {
  id: string | undefined;
  event: string | undefined;
  data: string;
}

// parseEventStream frames a byte stream into SSE frames. It decodes incrementally
// (TextDecoder stream mode preserves a multi-byte character split across chunks), buffers
// a partial trailing line across reads, joins multi-line `data:` fields with newlines, and
// ignores comment lines (a leading colon), which the server uses for keep-alive. It never
// buffers the whole response — one frame is produced per blank-line dispatch.
export async function* parseEventStream(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<SSEFrame> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let frame = newFrame();
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      buffer += decoder.decode(value, { stream: true });
      let newline: number;
      while ((newline = buffer.indexOf("\n")) !== -1) {
        // Tolerate CRLF as well as LF line endings.
        let line = buffer.slice(0, newline);
        if (line.endsWith("\r")) {
          line = line.slice(0, -1);
        }
        buffer = buffer.slice(newline + 1);
        if (line === "") {
          if (frame.data !== "" || frame.event !== undefined || frame.id !== undefined) {
            yield frame;
          }
          frame = newFrame();
          continue;
        }
        applyField(frame, line);
      }
    }
  } finally {
    // Releasing the reader lets the caller cancel the body to close the socket.
    reader.releaseLock();
  }
}

interface MutableFrame {
  id: string | undefined;
  event: string | undefined;
  data: string;
}

function newFrame(): MutableFrame {
  return { id: undefined, event: undefined, data: "" };
}

// applyField parses one SSE line into the current frame. A leading colon is a comment; a
// field is `name` or `name:value` with one optional space stripped after the colon.
function applyField(frame: MutableFrame, line: string): void {
  if (line.startsWith(":")) {
    return; // comment / heartbeat
  }
  const colon = line.indexOf(":");
  const field = colon === -1 ? line : line.slice(0, colon);
  let value = colon === -1 ? "" : line.slice(colon + 1);
  if (value.startsWith(" ")) {
    value = value.slice(1);
  }
  switch (field) {
    case "id":
      frame.id = value;
      break;
    case "event":
      frame.event = value;
      break;
    case "data":
      frame.data = frame.data === "" ? value : `${frame.data}\n${value}`;
      break;
    default:
      // retry: and unknown fields are not used by this consumer.
      break;
  }
}

const TERMINAL_EVENT = /^(run|response)\.(completed|failed|canceled|timed_out|budget_exceeded)\.v[0-9]+$/;

// isTerminalEvent reports whether an event closes the run, so the stream stops rather than
// reconnecting when the server closes a completed stream (spec §22.3, §24.4).
export function isTerminalEvent(event: Event): boolean {
  return TERMINAL_EVENT.test(event.type);
}

// StreamTransport is the slice of the client the stream needs: it opens the raw SSE
// response (Bearer auth + Last-Event-ID applied by the client) and retrieves the canonical
// terminal Response. Kept structural so stream.ts does not import the client (no cycle).
export interface StreamTransport {
  openEventStream(
    sessionID: string,
    lastEventId: string | null,
    signal: AbortSignal | undefined,
  ): Promise<globalThis.Response>;
  retrieveResponse(responseID: string): Promise<PalaiResponse>;
}

// StreamStart is the identity a stream resolves to when its underlying response is
// created (or already exists): the response to retrieve at the end and the session to
// subscribe to.
export interface StreamStart {
  responseID: string;
  sessionID: string;
}

export interface ResponseStreamInit {
  transport: StreamTransport;
  /** Runs once, lazily, on first consumption — typically the create POST. */
  start: () => Promise<StreamStart>;
  signal?: AbortSignal | undefined;
  lastEventId?: string | null;
  maxReconnects?: number;
  backoffBaseMs?: number;
  backoffMaxMs?: number;
}

// ResponseStream is a resumable, typed consumer of a run's event stream. Iterating it
// yields each canonical Event (unknown event types are delivered, not dropped: API-009).
// A transport drop before a terminal event reconnects from the last seen id via
// Last-Event-ID with full-jitter backoff; a terminal event or an explicit cancel stops
// without reconnecting. Breaking out of the iteration closes the transport.
export class ResponseStream implements AsyncIterable<Event> {
  #transport: StreamTransport;
  #start: () => Promise<StreamStart>;
  #started: StreamStart | undefined;
  #startPromise: Promise<StreamStart> | undefined;
  #signal: AbortSignal | undefined;
  #lastEventId: string | null;
  #maxReconnects: number;
  #backoffBaseMs: number;
  #backoffMaxMs: number;
  #iterator: AsyncGenerator<Event> | undefined;

  constructor(init: ResponseStreamInit) {
    this.#transport = init.transport;
    this.#start = init.start;
    this.#signal = init.signal;
    this.#lastEventId = init.lastEventId ?? null;
    this.#maxReconnects = init.maxReconnects ?? 5;
    this.#backoffBaseMs = init.backoffBaseMs ?? 100;
    this.#backoffMaxMs = init.backoffMaxMs ?? 5_000;
  }

  // responseID / sessionID resolve once the stream has started (its create has returned);
  // they are undefined before first consumption.
  get responseID(): string | undefined {
    return this.#started?.responseID;
  }
  get sessionID(): string | undefined {
    return this.#started?.sessionID;
  }

  // lastEventId exposes the reconnection cursor reached so far (the id of the last event
  // delivered), so a caller can persist it and resume a new stream later.
  get lastEventId(): string | null {
    return this.#lastEventId;
  }

  #ensureStarted(): Promise<StreamStart> {
    if (this.#startPromise === undefined) {
      this.#startPromise = this.#start().then((started) => (this.#started = started));
    }
    return this.#startPromise;
  }

  [Symbol.asyncIterator](): AsyncGenerator<Event> {
    if (this.#iterator !== undefined) {
      throw new PalaiConnectionError("this response stream has already been consumed");
    }
    this.#iterator = this.#run();
    return this.#iterator;
  }

  // finalResponse drains the stream to its terminal event, then returns the canonical
  // terminal Response (status, model, output, usage, and — on failure — the problem-shaped
  // error). It shares the single underlying iteration, so it must not be combined with a
  // separate manual iteration of the same stream.
  async finalResponse(): Promise<PalaiResponse> {
    const iterator = this[Symbol.asyncIterator]();
    for (;;) {
      const next = await iterator.next();
      if (next.done) {
        break;
      }
      if (isTerminalEvent(next.value)) {
        break;
      }
    }
    await iterator.return?.(undefined);
    const { responseID } = await this.#ensureStarted();
    return this.#transport.retrieveResponse(responseID);
  }

  async *#run(): AsyncGenerator<Event> {
    let start: StreamStart;
    try {
      start = await this.#ensureStarted();
    } catch (cause) {
      if (isAbort(cause, this.#signal)) {
        return; // the create was canceled before the stream opened
      }
      throw cause; // a create failure surfaces to the consumer
    }
    let reconnects = 0;
    for (;;) {
      // Capture the cursor this connection resumes from, so a server that redelivers the
      // boundary event inclusively does not double-deliver it to the consumer.
      const resumedFrom = this.#lastEventId;
      let response: globalThis.Response;
      try {
        response = await this.#transport.openEventStream(start.sessionID, resumedFrom, this.#signal);
      } catch (cause) {
        if (isAbort(cause, this.#signal)) {
          return; // explicit cancel: stop, do not reconnect
        }
        if (reconnects >= this.#maxReconnects) {
          throw new PalaiConnectionError("event stream could not be (re)opened", { cause });
        }
        if (!(await this.#backoffSleep(reconnects))) {
          return; // canceled mid-backoff: end quietly, like every other cancel path
        }
        reconnects += 1;
        continue;
      }
      if (!response.ok) {
        // A status error (404 unknown session, 410 gone) is terminal, not a drop.
        const body = await safeText(response);
        throw errorForResponse(response.status, body, response.headers.get("Request-Id") ?? undefined);
      }
      if (response.body === null) {
        return;
      }
      const frames = parseEventStream(response.body);
      // On a resume, drop a single redelivered boundary event whose id equals the cursor.
      let dedupePending = resumedFrom !== null;
      try {
        for await (const frame of frames) {
          if (frame.id !== undefined) {
            this.#lastEventId = frame.id;
          }
          if (frame.data === "") {
            continue; // keep-alive with no payload
          }
          const event = decodeEvent(frame.data);
          if (event === null) {
            continue; // a non-JSON data line is not a canonical event
          }
          if (dedupePending) {
            dedupePending = false;
            if (frame.id !== undefined && frame.id === resumedFrom) {
              continue; // duplicate of the resume boundary
            }
          }
          yield event;
          if (isTerminalEvent(event)) {
            return; // terminal reached: stop, do not reconnect
          }
        }
      } catch (cause) {
        if (isAbort(cause, this.#signal)) {
          return;
        }
        // fall through to reconnect below
      } finally {
        // Close the body — and thus the socket — whenever this connection is left: on a
        // terminal, an abort, or the consumer breaking out of the iteration (iterator
        // close closes the transport). It is a no-op when the server already ended it.
        await frames.return(undefined);
        await response.body?.cancel().catch(() => {});
      }
      if (this.#signal?.aborted) {
        return;
      }
      // The stream ended without a terminal event: an unexpected drop. Reconnect from the
      // last seen id, bounded, with backoff.
      if (reconnects >= this.#maxReconnects) {
        throw new PalaiConnectionError("event stream dropped before a terminal event and exhausted reconnects");
      }
      if (!(await this.#backoffSleep(reconnects))) {
        return; // canceled mid-backoff: end quietly, like every other cancel path
      }
      reconnects += 1;
    }
  }

  // #backoffSleep waits out one reconnect backoff, cancelably. It returns true when the
  // sleep completes, or false if the caller aborts mid-sleep — so #run ends the stream
  // quietly instead of leaking the abort's DOMException, matching every other cancel path.
  async #backoffSleep(reconnects: number): Promise<boolean> {
    try {
      await delay(fullJitterBackoff(reconnects, this.#backoffBaseMs, this.#backoffMaxMs), this.#signal);
      return true;
    } catch (cause) {
      if (isAbort(cause, this.#signal)) {
        return false;
      }
      throw cause;
    }
  }
}

function decodeEvent(data: string): Event | null {
  try {
    const parsed = JSON.parse(data) as unknown;
    if (typeof parsed === "object" && parsed !== null && typeof (parsed as Event).type === "string") {
      return parsed as Event;
    }
    return null;
  } catch {
    return null;
  }
}

async function safeText(response: globalThis.Response): Promise<string> {
  try {
    return await response.text();
  } catch {
    return "";
  }
}

// isAbort reports whether an error is the caller's explicit cancellation, so the stream
// stops silently rather than surfacing an error or reconnecting.
function isAbort(cause: unknown, signal: AbortSignal | undefined): boolean {
  if (signal?.aborted) {
    return true;
  }
  return cause instanceof Error && cause.name === "AbortError";
}

// --- Shared backoff, used by both stream reconnect and the client's request retry. ---

// fullJitterBackoff returns a delay in [0, min(maxMs, baseMs * 2^attempt)] — the AWS
// "full jitter" schedule, which spreads retries so many clients do not synchronize their
// reconnect storms (§23.7). attempt is 0-based.
export function fullJitterBackoff(attempt: number, baseMs: number, maxMs: number): number {
  if (baseMs <= 0 || attempt < 0) {
    return 0;
  }
  const ceiling = Math.min(maxMs, baseMs * 2 ** attempt);
  return Math.random() * ceiling;
}

// delay resolves after ms, or rejects with the signal's reason if it aborts first, so a
// backoff sleep is itself cancelable.
export function delay(ms: number, signal?: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}
