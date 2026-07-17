// Importing the API-key client marks this module server-only: bundling it for the browser
// throws rather than shipping the secret (see ./server-only.ts).
import "./server-only.ts";

import type { Response as PalaiResponse } from "./generated/types.ts";
import { errorForResponse, isRetryableStatus, PalaiConnectionError } from "./errors.ts";
import { delay, fullJitterBackoff, type StreamTransport } from "./stream.ts";
import { Responses } from "./resources/responses.ts";

// APIVersion is the dated contract this SDK speaks; it rides every request (spec §20.13).
export const APIVersion = "2026-07-16";

export interface PalaiOptions {
  baseURL?: string;
  apiKey?: string;
  /** Reserved for multi-project keys; the server derives scope from the key today. */
  project?: string;
  /** Injectable for tests and custom transports; defaults to the global fetch. */
  fetch?: typeof fetch;
  /** Max retry attempts after the first try for a retryable failure. Default 2. */
  maxRetries?: number;
  /** Total deadline for a request including retries and backoff, ms. Default 60000. */
  timeoutMs?: number;
  backoffBaseMs?: number;
  backoffMaxMs?: number;
}

export interface RequestOptions {
  body?: unknown;
  idempotencyKey?: string;
  signal?: AbortSignal | undefined;
  maxRetries?: number;
  timeoutMs?: number;
  accept?: string;
}

export interface ApiResult<T> {
  status: number;
  headers: Headers;
  body: T;
}

// Palai is the transport client: Bearer auth, the dated API version, idempotent retry, and
// typed RFC 9457 errors. Resource groups (responses) hang off it. It implements
// StreamTransport so the streaming layer opens SSE connections through the same auth.
export class Palai implements StreamTransport {
  readonly baseURL: string;
  readonly project: string | undefined;
  readonly responses: Responses;

  #apiKey: string;
  #fetch: typeof fetch;
  #maxRetries: number;
  #timeoutMs: number;
  #backoffBaseMs: number;
  #backoffMaxMs: number;

  constructor(options: PalaiOptions = {}) {
    // Precedence: explicit option, then the Palai-scoped environment. No other provider's
    // environment is ever read — the SDK never silently picks up an unrelated key.
    const apiKey = options.apiKey ?? env("PALAI_API_KEY");
    if (!apiKey) {
      throw new Error(
        "@palai/sdk: an API key is required — pass { apiKey } or set PALAI_API_KEY. " +
          "Keep it server-side; never expose it to the browser.",
      );
    }
    this.#apiKey = apiKey;
    this.baseURL = trimTrailingSlash(options.baseURL ?? env("PALAI_BASE_URL") ?? "http://localhost:8080");
    this.project = options.project ?? env("PALAI_PROJECT");
    this.#fetch = options.fetch ?? globalThis.fetch;
    this.#maxRetries = options.maxRetries ?? 2;
    this.#timeoutMs = options.timeoutMs ?? 60_000;
    this.#backoffBaseMs = options.backoffBaseMs ?? 200;
    this.#backoffMaxMs = options.backoffMaxMs ?? 10_000;
    this.responses = new Responses(this);
  }

  async request<T>(method: string, path: string, options: RequestOptions = {}): Promise<ApiResult<T>> {
    const url = this.baseURL + path;
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.#apiKey}`,
      "API-Version": APIVersion,
      Accept: options.accept ?? "application/json",
    };
    // The idempotency key is built once and rides every retry, so a create that is retried
    // after a network blip settles exactly one response (spec §20.9, §35.3).
    if (options.idempotencyKey !== undefined) {
      headers["Idempotency-Key"] = options.idempotencyKey;
    }
    let bodyText: string | undefined;
    if (options.body !== undefined) {
      bodyText = JSON.stringify(options.body);
      headers["Content-Type"] = "application/json";
    }

    const maxRetries = options.maxRetries ?? this.#maxRetries;
    const deadline = Date.now() + (options.timeoutMs ?? this.#timeoutMs);
    let attempt = 0;
    for (;;) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) {
        throw new PalaiConnectionError(`${method} ${path} exceeded its ${options.timeoutMs ?? this.#timeoutMs}ms deadline`);
      }
      const signal = combineSignals(options.signal, AbortSignal.timeout(remaining));

      let response: globalThis.Response;
      try {
        response = await this.#fetch(url, {
          method,
          headers,
          signal,
          ...(bodyText !== undefined ? { body: bodyText } : {}),
        });
      } catch (cause) {
        if (options.signal?.aborted) {
          throw new PalaiConnectionError(`${method} ${path} was canceled`, { cause });
        }
        // Network failure or a per-attempt timeout: retry within budget.
        if (attempt >= maxRetries || !withinBudget(deadline, attempt, this.#backoffBaseMs, this.#backoffMaxMs)) {
          throw new PalaiConnectionError(`${method} ${path} failed to reach the server`, { cause });
        }
        await delay(fullJitterBackoff(attempt, this.#backoffBaseMs, this.#backoffMaxMs), options.signal);
        attempt += 1;
        continue;
      }

      if (response.ok) {
        return await parseJSON<T>(response);
      }

      if (isRetryableStatus(response.status) && attempt < maxRetries) {
        const wait = retryAfterMs(response) ?? fullJitterBackoff(attempt, this.#backoffBaseMs, this.#backoffMaxMs);
        if (Date.now() + wait < deadline) {
          await response.body?.cancel().catch(() => {});
          await delay(wait, options.signal);
          attempt += 1;
          continue;
        }
      }
      const text = await response.text();
      throw errorForResponse(response.status, text, response.headers.get("Request-Id") ?? undefined);
    }
  }

  // openEventStream opens the raw SSE response for a session with Bearer auth and, on a
  // resume, the Last-Event-ID cursor. It does not retry — ResponseStream owns reconnection.
  openEventStream(
    sessionID: string,
    lastEventId: string | null,
    signal: AbortSignal | undefined,
  ): Promise<globalThis.Response> {
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.#apiKey}`,
      "API-Version": APIVersion,
      Accept: "text/event-stream",
    };
    if (lastEventId !== null) {
      headers["Last-Event-ID"] = lastEventId;
    }
    return this.#fetch(`${this.baseURL}/v1/sessions/${encodeURIComponent(sessionID)}/events`, {
      method: "GET",
      headers,
      cache: "no-store",
      ...(signal ? { signal } : {}),
    });
  }

  retrieveResponse(responseID: string): Promise<PalaiResponse> {
    return this.responses.retrieve(responseID);
  }
}

// env reads a Palai-scoped variable, trimmed, or undefined. It never throws in a runtime
// without process (the browser-safe entrypoint never constructs this client).
function env(name: string): string | undefined {
  if (typeof process === "undefined" || process.env === undefined) {
    return undefined;
  }
  const value = process.env[name];
  return value && value.trim() !== "" ? value.trim() : undefined;
}

function trimTrailingSlash(url: string): string {
  return url.endsWith("/") ? url.slice(0, -1) : url;
}

// combineSignals joins the caller's cancel signal with the per-attempt timeout so either
// aborts the fetch; with no caller signal it is just the timeout.
function combineSignals(caller: AbortSignal | undefined, timeout: AbortSignal): AbortSignal {
  return caller ? AbortSignal.any([caller, timeout]) : timeout;
}

// withinBudget reports whether the next backoff would still leave time before the deadline.
function withinBudget(deadline: number, attempt: number, baseMs: number, maxMs: number): boolean {
  return Date.now() + fullJitterBackoff(attempt, baseMs, maxMs) < deadline;
}

// retryAfterMs honors a server Retry-After header (delta-seconds) so the client backs off
// as instructed rather than on its own schedule; a non-numeric value is ignored.
function retryAfterMs(response: globalThis.Response): number | null {
  const header = response.headers.get("Retry-After");
  if (header === null) {
    return null;
  }
  const seconds = Number(header);
  return Number.isFinite(seconds) && seconds >= 0 ? seconds * 1000 : null;
}

async function parseJSON<T>(response: globalThis.Response): Promise<ApiResult<T>> {
  const text = await response.text();
  const body = (text === "" ? undefined : JSON.parse(text)) as T;
  return { status: response.status, headers: response.headers, body };
}
