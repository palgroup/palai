import type { RequestOptions } from "../client.ts";

// The plumbing every resource beyond Responses shares: per-call transport options, the opaque
// cursor list parameters, and the two page envelopes the API returns. Kept in one place so each
// resource stays as thin as responses.ts rather than re-deriving the query/option dance.

// CallOptions is the transport control a caller may override per request. It mirrors the tail of
// RetrieveOptions in responses.ts (signal + deadline + retry budget), without the create-only
// idempotency key.
export interface CallOptions {
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
  maxRetries?: number;
}

// DownloadOptions is CallOptions without maxRetries: a byte download is a single attempt (a
// partially-drained stream is not safely retryable), so the retry knob is deliberately unrepresentable
// here rather than silently ignored. timeoutMs bounds the CONNECT phase only — once headers arrive the
// byte stream is bounded by `signal`, so a large object never trips the connect deadline mid-transfer.
export interface DownloadOptions {
  signal?: AbortSignal | undefined;
  timeoutMs?: number;
}

// ListParams are the shared pagination + basic filters the whole read/LIST surface accepts
// (api/pagination.go): an opaque, tenant-bound `after` cursor, a page `limit`, and the two basic
// filters. `status` is only honored on the responses/sessions lists — the server rejects it
// elsewhere, so it is passed through, never faked here. Camel-cased at the edge, snake-cased on the
// wire (created_after/created_before).
export interface ListParams {
  after?: string;
  limit?: number;
  status?: string;
  createdAfter?: string;
  createdBefore?: string;
}

// Page is the cursor-paginated envelope (contracts.Page): a slice of rows plus the opaque cursor to
// fetch the next page. The SDK passes `next_cursor` straight back as the next `after` — it never
// parses the cursor (it is tenant-bound and server-minted).
export interface Page<T> {
  data: T[];
  has_more: boolean;
  next_cursor?: string | null;
  previous_cursor?: string | null;
}

// ListView is the un-paginated `{object:"list", data:[...]}` envelope the provisioning/secret-ref
// admin reads return (identity store listView) — a full, small, tenant-scoped set, no cursor.
export interface ListView<T> {
  object: string;
  data: T[];
}

// callArgs projects CallOptions onto the client's RequestOptions, dropping absent numeric fields so
// exactOptionalPropertyTypes stays satisfied (a bare `timeoutMs: undefined` is not assignable).
export function callArgs(options: CallOptions = {}): RequestOptions {
  const out: RequestOptions = { signal: options.signal };
  if (options.timeoutMs !== undefined) out.timeoutMs = options.timeoutMs;
  if (options.maxRetries !== undefined) out.maxRetries = options.maxRetries;
  return out;
}

// listPath appends the shared list query to a path. Absent params add no key, so a bare list() sends
// a clean path; a present param is URL-encoded by URLSearchParams.
export function listPath(path: string, params: ListParams = {}): string {
  const query = new URLSearchParams();
  if (params.after !== undefined) query.set("after", params.after);
  if (params.limit !== undefined) query.set("limit", String(params.limit));
  if (params.status !== undefined) query.set("status", params.status);
  if (params.createdAfter !== undefined) query.set("created_after", params.createdAfter);
  if (params.createdBefore !== undefined) query.set("created_before", params.createdBefore);
  const qs = query.toString();
  return qs === "" ? path : `${path}?${qs}`;
}

// enc is the path-segment encoder every resource uses for ids (matches responses.ts).
export function enc(segment: string): string {
  return encodeURIComponent(segment);
}
