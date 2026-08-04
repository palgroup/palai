// The BROWSER-side relay client. It talks ONLY to the same-origin relay under /api/palai/* — never to
// the upstream directly, never with a credential (the key lives server-side, lib/palai.ts). It imports
// NO @palai/sdk module, so no credential path can ever be bundled here. This is the surface the
// public-API-only network-intercept proof pins: every data request the browser makes goes through
// /api/palai/v1/* (the /v1 public API) or /api/palai/stream (the canonical run stream).

const RELAY = "/api/palai";

export interface Problem {
  code: string;
  status: number;
  detail: string;
}

export class RelayError extends Error {
  readonly problem: Problem;
  constructor(problem: Problem) {
    super(`${problem.code}: ${problem.detail}`);
    this.problem = problem;
  }
}

async function toProblem(res: Response): Promise<RelayError> {
  try {
    const body = (await res.json()) as Partial<Problem>;
    return new RelayError({ code: body.code ?? "error", status: res.status, detail: body.detail ?? res.statusText });
  } catch {
    return new RelayError({ code: "error", status: res.status, detail: res.statusText });
  }
}

// apiGet reads a /v1 resource through the relay. `path` is the upstream path AFTER /v1 (e.g. "/projects").
export async function apiGet<T = unknown>(path: string): Promise<T> {
  const res = await fetch(`${RELAY}/v1${path}`, { headers: { Accept: "application/json" } });
  if (!res.ok) throw await toProblem(res);
  return (await res.json()) as T;
}

// apiSend writes a /v1 resource through the relay (POST/PUT/PATCH/DELETE).
//
// PUT joined the union with E29's desired configuration, and the relay grew a PUT export in the same
// change. Widening this type WITHOUT that export would have compiled and shipped a form that gets a 405 —
// a Next.js Route Handler serves only the methods it exports — which is the "declared, and nothing
// happens" defect the /deployment screen exists to expose.
export async function apiSend<T = unknown>(method: "POST" | "PUT" | "PATCH" | "DELETE", path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${RELAY}/v1${path}`, {
    method,
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw await toProblem(res);
  return (await res.json()) as T;
}

// artifactHref is the same-origin relay URL for an artifact's bytes — an <a href> download that still
// routes through /v1/*, so the browser never addresses the upstream or the object store directly.
export function artifactHref(artifactId: string): string {
  return `${RELAY}/v1/artifacts/${encodeURIComponent(artifactId)}/content`;
}

// readSessionEvents re-reads a session's canonical event journal (E25 T8, feature list O2). It is how a
// FINISHED run's timeline is rebuilt, and it is the only way: /v1 has no JSON event-list route, so the
// history of a run is the same SSE stream a live run reads, replayed from sequence 0.
//
// It TERMINATES rather than tails, and that is the server's behaviour rather than a hope: api/events.go's
// pump returns as soon as it has written a terminal event type, closing the stream. A run still in flight
// would tail with heartbeats instead, which is why the caller owns an AbortSignal.
//
// EventSource is deliberately not used. The frames carry `event: <type>` (contracts.Event MarshalSSE), so
// an EventSource would need a listener registered per event NAME — a list this console would then have to
// keep in step with the journal's vocabulary — and on the server's clean close it would RECONNECT and
// replay the whole run again. A read of the body ends when the stream ends.
export async function readSessionEvents(
  sessionId: string,
  onEvent: (event: Record<string, unknown>) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetch(`${RELAY}/v1/sessions/${encodeURIComponent(sessionId)}/events`, {
    headers: { Accept: "text/event-stream" },
    signal,
  });
  if (!res.ok || res.body === null) throw await toProblem(res);

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let end = buffer.indexOf("\n\n");
    while (end !== -1) {
      // Only the `data:` lines are read. `id:` and `event:` restate what the payload already carries, and a
      // `: heartbeat` comment carries nothing — a frame with no data line is skipped rather than parsed.
      const data = buffer
        .slice(0, end)
        .split("\n")
        .filter((line) => line.startsWith("data:"))
        .map((line) => line.slice("data:".length).trim())
        .join("");
      buffer = buffer.slice(end + 2);
      if (data !== "") onEvent(JSON.parse(data) as Record<string, unknown>);
      end = buffer.indexOf("\n\n");
    }
  }
}

// streamRun starts a run and yields each projected ndjson frame. It POSTs the prompt to the stream relay
// and reads the newline-delimited canonical projection (lib/timeline lanes). The signal aborts the read
// (a disconnect closes the upstream transport but does NOT cancel the run — LP6).
// `agentRevisionId` is the OPTIONAL pin (E25 T6). Omitted — not sent as an empty string — when no revision is
// chosen, so an unpinned run's request body stays exactly `{prompt}` and the relay's upstream body stays
// exactly `{input}`. tests/config-journey.spec.ts asserts that on the WIRE rather than by outcome.
// `sessionId` is what makes a run a TURN rather than a SINGLE SHOT. Omitted, the admission mints a fresh
// session and the model sees nothing before this prompt; present, admission resolves that session, requires
// it to be ACTIVE, and appends to it (packages/coordinator/store.go:742). app/api/palai/stream/route.ts
// carries the measurement and the reason this console could never do the second one until now.
export async function streamRun(
  prompt: string,
  onFrame: (frame: Record<string, unknown>) => void,
  signal?: AbortSignal,
  agentRevisionId?: string,
  outputSchema?: string,
  sessionId?: string,
): Promise<void> {
  const res = await fetch(`${RELAY}/stream`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      prompt,
      // Each optional key is OMITTED rather than sent empty, so an unpinned, unconstrained run's
      // request body stays exactly `{prompt}` and the relay's upstream body exactly `{input}` —
      // the property tests/config-journey.spec.ts asserts on the WIRE.
      ...(sessionId === undefined || sessionId === "" ? {} : { session_id: sessionId }),
      ...(agentRevisionId === undefined || agentRevisionId === "" ? {} : { agent_revision_id: agentRevisionId }),
      ...(outputSchema === undefined || outputSchema.trim() === "" ? {} : { output_schema: outputSchema }),
    }),
    signal,
  });
  if (!res.ok || res.body === null) throw await toProblem(res);

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let nl = buffer.indexOf("\n");
    while (nl !== -1) {
      const line = buffer.slice(0, nl).trim();
      buffer = buffer.slice(nl + 1);
      if (line !== "") onFrame(JSON.parse(line) as Record<string, unknown>);
      nl = buffer.indexOf("\n");
    }
  }
}
