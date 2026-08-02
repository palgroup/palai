// ============================================================
// AI Chat — Client-side API helpers (SSE streaming)
//
// COPIED FROM palcore, apps/web/src/ai-chat/api.ts, and adapted to Palai. The owner's directive was
// "palcore'un chat'i ile birebir aynı yap direkt onu al kopyala sonra palai sdk kullanacak şekilde
// güncelle" — take the chat, copy it, then change the data layer.
//
// WHAT SURVIVED THE COPY: the AsyncGenerator interface. The UI still writes
// `for await (const evt of streamGenerate(...))` exactly as palcore's does, and the event union has the
// same names and the same shapes for the events both products have.
//
// WHAT CHANGED, AND WHY:
//   * palcore's transport is a WebSocket (`/api/project/ws/:projectId`) opened BEFORE an HTTP POST that
//     returns {sessionId, jobId}. Palai has no such WS: its journal is server-sent events at
//     GET /v1/sessions/{id}/events, read server-side by this app's own /api/chat so no key reaches the
//     browser. One POST, one SSE body, no second connection to race.
//   * palcore filters inbound frames by sessionId because its WS is shared across the whole project.
//     This stream carries one turn, so there is nothing to filter here — the filtering that matters
//     happens server-side, by run_id, and the reason is in app/api/chat/route.ts.
//   * `images` and `selectionContext` are NOT carried. palcore attaches screenshots and canvas
//     selections because it is a design tool with a canvas. This demo has neither, and shipping an
//     attach button whose payload nothing accepts is the exact defect this tree names most often — a
//     surface that is declared and not wired.
//
// WHAT WAS ADDED: the Palai half. An approval a human must decide, a publication receipt, a replay
// class, and the ledger join that carries what a tool actually ran. palcore has no equivalent of any of
// them, so they are additions rather than translations.
// ============================================================

// ── Stream Event Types ────────────────────────────────────────

export type AIStreamEvent =
  | { type: "session"; sessionId: string }
  | { type: "text_delta"; text: string }
  | { type: "thinking_delta"; text: string }
  // `id` IS AN ADDITION TO palcore's SHAPE AND IT FIXES A BUG RATHER THAN CARRYING ONE OVER.
  // palcore matches a result to its call by NAME (AIChat.tsx:549):
  //     tc.name === event.toolName || (event as any).name && tc.status === 'running'
  // `&&` binds tighter than `||`, so the second arm is "this event has a name AND this call is
  // running" — it closes the first running call of ANY name. Two concurrent shell calls finish onto
  // each other. Palai gives every call a `tool_call_id` on both frames, so this matches on that.
  | { type: "tool_call"; id: string; toolName: string; replayClass: string | null; nameUnavailable: boolean }
  | { type: "tool_result"; id: string; toolName: string; replayClass: string | null; nameUnavailable: boolean }
  | ({ type: "tool_detail"; id: string } & Record<string, unknown>)
  | ({ type: "approval"; id: string } & Record<string, unknown>)
  | ({ type: "publication"; id: string } & Record<string, unknown>)
  | { type: "notice"; level: "warn" | "error"; text: string }
  | { type: "run"; responseId: string; runId?: string | null; status: string }
  | { type: "usage"; input_tokens: number | null; output_tokens: number | null; total_tokens: number | null }
  | { type: "done" }
  | { type: "error"; message: string };

// ── Streaming ─────────────────────────────────────────────────
//
// palcore's `streamGenerate` opens a WS, waits for OPEN, fires the dispatch POST, then drains frames
// until done/error/closed. Palai needs only the POST: this app's Route Handler holds the credential,
// opens Palai's journal itself, and streams the mapped frames back on the SAME response body.

export async function* streamGenerate(params: {
  prompt: string;
  sessionId?: string;
  bindingId?: string;
  signal?: AbortSignal;
}): AsyncGenerator<AIStreamEvent> {
  const response = await fetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      prompt: params.prompt,
      sessionId: params.sessionId,
      bindingId: params.bindingId,
    }),
    signal: params.signal,
  });

  if (!response.ok || response.body === null) {
    // A problem+json body carries the sentence the operator should read; the status alone does not.
    const detail = await response
      .json()
      .then((b: { detail?: string }) => b?.detail ?? "")
      .catch(() => "");
    throw new Error(detail !== "" ? detail : `the chat route answered HTTP ${response.status}`);
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      // SSE frames are separated by a BLANK LINE, and a chunk boundary can fall anywhere — including
      // inside a JSON payload. Splitting on "\n" and parsing each line is the bug this loop exists not
      // to have.
      let sep: number;
      while ((sep = buffer.indexOf("\n\n")) !== -1) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const line = frame.split("\n").find((l) => l.startsWith("data:"));
        if (line === undefined) continue;
        let parsed: unknown;
        try {
          parsed = JSON.parse(line.slice(5).trim());
        } catch {
          continue;
        }
        if (parsed === null || typeof parsed !== "object") continue;
        const event = parsed as AIStreamEvent;
        if (typeof (event as { type?: unknown }).type !== "string") continue;
        yield event;
        if (event.type === "done" || event.type === "error") return;
      }
    }
  } finally {
    try {
      await reader.cancel();
    } catch {
      // Already torn down.
    }
  }
}

// ── The decision a human makes ────────────────────────────────
//
// Not a stream event: an approval is answered by a request of its own, and the run stays parked until
// somebody answers. The one-shot request hash is FORWARDED, never minted here — it is what makes an
// approval authorize the exact call proposed and not whatever it becomes afterwards.
export async function decideApproval(approvalId: string, requestHash: string, approve: boolean): Promise<void> {
  const res = await fetch("/api/chat/approve", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ approvalId, requestHash, approve }),
  });
  if (res.ok) return;
  // 403 (not an approver), 409 (no longer decidable) and 404 (unknown) have three different fixes; the
  // relay passes the status through so the screen can say which one it was.
  const body = (await res.json().catch(() => ({}))) as { detail?: string };
  throw new Error(body.detail ?? `HTTP ${res.status}`);
}
