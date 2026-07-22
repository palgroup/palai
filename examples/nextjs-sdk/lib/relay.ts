import { PalaiAPIError, PalaiError } from "@palai/sdk";

// Shared relay helpers for the server-side Route Handlers. They render stable, code-carrying
// problem bodies to the browser and NEVER forward the raw provider payload or the credential —
// the same server-only stance as app/api/palai/route.ts.

export function problem(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}

// relayError maps an SDK error to a stable problem body. A typed API error keeps its stable code +
// status + request id; a transport error is a generic connection problem; anything else is a 500.
export function relayError(err: unknown): Response {
  if (err instanceof PalaiAPIError) {
    return problem(err.status, err.code, err.problem.detail ?? err.problem.title ?? err.code);
  }
  if (err instanceof PalaiError) {
    return problem(502, "connection_error", err.message);
  }
  return problem(500, "internal_error", "the request could not be relayed");
}
