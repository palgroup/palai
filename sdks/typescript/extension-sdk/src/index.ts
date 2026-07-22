// @palai/extension-sdk — the server-side helper set for building a remote_http
// tool endpoint that speaks tool-http.v1 (spec §28.23/§28.24, TOL-018). It gives a
// customer's tool server the four contract-correct primitives without letting it
// get the wire shape or the MAC wrong: define-tool schema emit, signed-invocation
// verify + result-callback sign (standard-webhooks HMAC both ways), normalized
// {result|problem} bodies, and the tool_call_id idempotency store. The SDK NEVER
// assigns trust; it only makes producing the contract correct (spec §28.23). The
// wire envelope type is imported from the generated contracts — never re-declared.
import { createHmac, timingSafeEqual } from "node:crypto";

import type { ToolHTTPCallback } from "../../src/generated/types.ts";

// Bytes is the secret/body input the HMAC accepts: a utf8 string or raw bytes.
// (node:crypto's BinaryLike is wider — it includes ArrayBuffer, which hmac.update
// rejects — so we narrow to the shapes both createHmac and update take.)
type Bytes = string | Uint8Array;

export const PROTOCOL = "tool-http.v1";
export const SIGNATURE_VERSION = "v1";

// standard-webhooks attempt header names (protocol constants, not a reinvented shape).
export const HEADER_ID = "Webhook-Id";
export const HEADER_TIMESTAMP = "Webhook-Timestamp";
export const HEADER_SIGNATURE = "Webhook-Signature";
export const HEADER_CALLBACK_TOKEN = "Tool-Callback-Token";

// canonical renders v as sorted-key compact JSON — byte-identical to the Go
// (encoding/json sorted map keys, HTML escaping off) and Python (json.dumps
// sort_keys, compact) legs. The ONE definition of canonical bytes for this leg.
function sortKeys(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (v !== null && typeof v === "object") {
    const out: Record<string, unknown> = {};
    for (const key of Object.keys(v as Record<string, unknown>).sort()) {
      out[key] = sortKeys((v as Record<string, unknown>)[key]);
    }
    return out;
  }
  return v;
}

function canonical(v: unknown): string {
  return JSON.stringify(sortKeys(v));
}

// ToolDefinition is the executor-config subset a tool server declares to register
// a remote_http tool revision (spec §28.4). A secret is a secret_ref handle only.
export interface ToolDefinition {
  executor: string;
  description?: string;
  input_schema: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
  replay_class?: string;
  timeout_ms?: number;
  executor_config?: Record<string, unknown>;
  secret_ref?: string;
}

// defineTool emits the tool revision registration body as canonical bytes (a
// string), dropping unset optional fields with the same omit rule as the Go/Py legs.
export function defineTool(def: ToolDefinition): string {
  if (!def.executor) throw new Error("extsdk: tool definition needs an executor");
  if (def.input_schema === undefined || def.input_schema === null) {
    throw new Error("extsdk: tool definition needs an input_schema");
  }
  const body: Record<string, unknown> = { executor: def.executor, input_schema: def.input_schema };
  if (def.description) body.description = def.description;
  if (def.output_schema !== undefined) body.output_schema = def.output_schema;
  if (def.replay_class) body.replay_class = def.replay_class;
  if (def.timeout_ms !== undefined) body.timeout_ms = def.timeout_ms;
  if (def.executor_config !== undefined) body.executor_config = def.executor_config;
  if (def.secret_ref) body.secret_ref = def.secret_ref;
  return canonical(body);
}

// sign computes the hex HMAC-SHA-256 over the standard-webhooks signed input:
// version, delivery id, unix timestamp, and the EXACT raw body, joined by "."
// (spec §21.5). Byte-identical to the Go/Py legs (proven by the shared corpus).
export function sign(secret: Bytes, deliveryId: string, ts: number, body: Bytes): string {
  const mac = createHmac("sha256", secret);
  mac.update(`${SIGNATURE_VERSION}.${deliveryId}.${ts}.`);
  mac.update(body);
  return mac.digest("hex");
}

// signatureHeader builds the Webhook-Signature value: one space-separated v1=
// field per secret, so a rotation overlap (old + new) is accepted either way.
export function signatureHeader(deliveryId: string, ts: number, body: Bytes, ...secrets: Bytes[]): string {
  return secrets.map((s) => `${SIGNATURE_VERSION}=${sign(s, deliveryId, ts, body)}`).join(" ");
}

function constantTimeEqual(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  if (ab.length !== bb.length) return false;
  return timingSafeEqual(ab, bb);
}

// verify is the receiver-side check: it recomputes the MAC over the raw body,
// compares in constant time (crypto.timingSafeEqual), enforces the timestamp
// tolerance on BOTH skew directions (the replay window), and accepts a header
// carrying several v1= values (rotation overlap). Mirrors the Go/Py Verify.
export function verify(
  secret: Bytes,
  deliveryId: string,
  ts: number,
  body: Bytes,
  header: string,
  now: number,
  toleranceSeconds: number,
): boolean {
  const skew = now - ts;
  if (skew > toleranceSeconds || skew < -toleranceSeconds) return false;
  const want = sign(secret, deliveryId, ts, body);
  for (const field of header.split(/\s+/)) {
    if (!field.startsWith(`${SIGNATURE_VERSION}=`)) continue;
    if (constantTimeEqual(field.slice(SIGNATURE_VERSION.length + 1), want)) return true;
  }
  return false;
}

// callbackHeaders returns the standard-webhooks headers a tool server posts a
// result callback with (multi-secret during rotation). The one-use callback token
// header is added by the caller from the invoke's callback.token.
export function callbackHeaders(deliveryId: string, ts: number, body: Bytes, ...secrets: Bytes[]): Record<string, string> {
  return {
    [HEADER_ID]: deliveryId,
    [HEADER_TIMESTAMP]: String(ts),
    [HEADER_SIGNATURE]: signatureHeader(deliveryId, ts, body, ...secrets),
  };
}

// syncResult / syncProblem build the synchronous 200 body a remote_http server may
// answer with (exactly one of the two shapes, spec §28.24), as canonical bytes.
export function syncResult(result: Record<string, unknown>): string {
  return canonical({ result });
}
export function syncProblem(problem: Record<string, unknown>): string {
  return canonical({ problem });
}

// callback / callbackProblem build the tool-http.v1 result callback envelope,
// typed as the generated ToolHTTPCallback contract (no hand-rolled shape), as
// canonical bytes the server signs with callbackHeaders.
export function callback(operationId: string, toolCallId: string, result: Record<string, unknown>): string {
  const envelope: ToolHTTPCallback = {
    protocol: PROTOCOL,
    operation_id: operationId,
    tool_call_id: toolCallId,
    result,
  };
  return canonical(envelope);
}
export function callbackProblem(operationId: string, toolCallId: string, problem: Record<string, unknown>): string {
  const envelope: ToolHTTPCallback = {
    protocol: PROTOCOL,
    operation_id: operationId,
    tool_call_id: toolCallId,
    problem,
  };
  return canonical(envelope);
}

// IdempotencyStore is the in-memory tool_call_id replay guard a tool server keys on
// the invoke's Idempotency-Key (= tool_call_id) and request_hash, mirroring the
// control-plane executor's rule (spec §28.24): a same-hash duplicate replays the
// stored answer, a diverged hash is a 409. A multi-replica server backs the same
// rule with its own shared store — the SEMANTICS are what the SDK pins.
export type IdempotencyOutcome = "fresh" | "replay" | "conflict";

export interface Classification {
  outcome: IdempotencyOutcome;
  response?: string;
}

export class IdempotencyStore {
  #seen = new Map<string, { requestHash: string; response: string }>();

  classify(toolCallId: string, requestHash: string): Classification {
    const seen = this.#seen.get(toolCallId);
    if (!seen) return { outcome: "fresh" };
    if (seen.requestHash !== requestHash) return { outcome: "conflict" };
    return { outcome: "replay", response: seen.response };
  }

  store(toolCallId: string, requestHash: string, response: string): void {
    this.#seen.set(toolCallId, { requestHash, response });
  }
}
