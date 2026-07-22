import { randomUUID } from "node:crypto";

import type { Response as PalaiResponse, ResponseCreateRequest } from "../generated/types.ts";
import type { Palai } from "../client.ts";
import { ResponseStream } from "../stream.ts";
import { callArgs, listPath, type ListParams, type Page } from "./shared.ts";

export interface CreateOptions {
  /** Override the auto-generated stable idempotency key (e.g. to dedupe across processes). */
  idempotencyKey?: string;
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
  maxRetries?: number;
}

export interface RetrieveOptions {
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
  maxRetries?: number;
}

export interface StreamOptions extends CreateOptions {
  /** Resume a prior stream from this cursor instead of the beginning. */
  lastEventId?: string | null;
}

// Responses is the /v1/responses resource group: create, retrieve, cancel, and stream.
export class Responses {
  #client: Palai;

  constructor(client: Palai) {
    this.#client = client;
  }

  // create posts a new response and returns the queued Response handle from the 202 body
  // (its id, session_id, and run_id). A stable Idempotency-Key is minted once per call and
  // reused across transport retries, so a retried create settles exactly one response; pass
  // options.idempotencyKey to supply your own.
  async create(request: ResponseCreateRequest, options: CreateOptions = {}): Promise<PalaiResponse> {
    const result = await this.#client.request<PalaiResponse>("POST", "/v1/responses", {
      body: request,
      idempotencyKey: options.idempotencyKey ?? newIdempotencyKey(),
      signal: options.signal,
      ...(options.timeoutMs !== undefined ? { timeoutMs: options.timeoutMs } : {}),
      ...(options.maxRetries !== undefined ? { maxRetries: options.maxRetries } : {}),
    });
    return result.body;
  }

  // retrieve reads a stored response, including its model and, on a failed terminal, the
  // problem-shaped error. A 404 throws NotFoundError; a 410 (retention-purged) throws
  // GoneError.
  async retrieve(responseID: string, options: RetrieveOptions = {}): Promise<PalaiResponse> {
    const result = await this.#client.request<PalaiResponse>(
      "GET",
      `/v1/responses/${encodeURIComponent(responseID)}`,
      {
        signal: options.signal,
        ...(options.timeoutMs !== undefined ? { timeoutMs: options.timeoutMs } : {}),
        ...(options.maxRetries !== undefined ? { maxRetries: options.maxRetries } : {}),
      },
    );
    return result.body;
  }

  // list returns a tenant-scoped page of run history (GET /v1/responses, E13 T4), with the shared
  // opaque cursor and the basic filters (?status=, created_at bounds).
  async list(params: ListParams = {}, options: RetrieveOptions = {}): Promise<Page<PalaiResponse>> {
    const result = await this.#client.request<Page<PalaiResponse>>("GET", listPath("/v1/responses", params), callArgs(options));
    return result.body;
  }

  // cancel requests best-effort cancellation of an in-flight response (accepted as 202).
  async cancel(responseID: string, options: RetrieveOptions = {}): Promise<void> {
    await this.#client.request<unknown>("POST", `/v1/responses/${encodeURIComponent(responseID)}/cancel`, {
      signal: options.signal,
      ...(options.timeoutMs !== undefined ? { timeoutMs: options.timeoutMs } : {}),
      ...(options.maxRetries !== undefined ? { maxRetries: options.maxRetries } : {}),
    });
  }

  // stream creates a response and returns a resumable, typed event stream over its session.
  // It returns synchronously; the create fires lazily on first consumption. The stream
  // reconnects with Last-Event-ID on a drop, and stream.finalResponse() resolves the
  // canonical terminal Response.
  stream(request: ResponseCreateRequest, options: StreamOptions = {}): ResponseStream {
    return new ResponseStream({
      transport: this.#client,
      start: async () => {
        const created = await this.create(request, options);
        return {
          responseID: String(created.id),
          sessionID: String(created.session_id ?? ""),
        };
      },
      signal: options.signal,
      lastEventId: options.lastEventId ?? null,
    });
  }
}

// newIdempotencyKey mints a fresh, collision-resistant key for one logical create.
function newIdempotencyKey(): string {
  return `idem_${randomUUID()}`;
}
