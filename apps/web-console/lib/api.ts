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

// apiGet reads a /v1 resource through the relay. `path` is the upstream path AFTER /v1 (e.g. "/organizations").
export async function apiGet<T = unknown>(path: string): Promise<T> {
  const res = await fetch(`${RELAY}/v1${path}`, { headers: { Accept: "application/json" } });
  if (!res.ok) throw await toProblem(res);
  return (await res.json()) as T;
}

// apiSend writes a /v1 resource through the relay (POST/PATCH/DELETE).
export async function apiSend<T = unknown>(method: "POST" | "PATCH" | "DELETE", path: string, body?: unknown): Promise<T> {
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

// streamRun starts a run and yields each projected ndjson frame. It POSTs the prompt to the stream relay
// and reads the newline-delimited canonical projection (lib/timeline lanes). The signal aborts the read
// (a disconnect closes the upstream transport but does NOT cancel the run — LP6).
export async function streamRun(
  prompt: string,
  onFrame: (frame: Record<string, unknown>) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetch(`${RELAY}/stream`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ prompt }),
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
