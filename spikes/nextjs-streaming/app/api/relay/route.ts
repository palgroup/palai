import { openPalaiStream } from "../../../lib/server-client";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

const streamHeaders = {
  "Cache-Control": "no-cache, no-transform",
  Connection: "keep-alive",
  "Content-Type": "text/event-stream; charset=utf-8",
  "X-Accel-Buffering": "no",
};

export async function GET(request: Request): Promise<Response> {
  const upstreamAbort = new AbortController();
  let reader: ReadableStreamDefaultReader<Uint8Array> | null = null;
  let finished = false;

  const cleanup = () => {
    request.signal.removeEventListener("abort", handleDownstreamAbort);
  };
  const abortUpstream = async (reason?: unknown) => {
    if (finished) {
      return;
    }
    finished = true;
    cleanup();
    if (!upstreamAbort.signal.aborted) {
      upstreamAbort.abort(reason);
    }
    if (reader !== null) {
      await reader.cancel(reason).catch(() => undefined);
    }
  };
  const handleDownstreamAbort = () => {
    void abortUpstream(request.signal.reason);
  };
  request.signal.addEventListener("abort", handleDownstreamAbort, { once: true });

  let upstream: Response;
  try {
    upstream = await openPalaiStream({
      lastEventId: request.headers.get("Last-Event-ID"),
      signal: upstreamAbort.signal,
    });
  } catch {
    cleanup();
    return unavailableResponse();
  }

  if (!upstream.ok || upstream.body === null) {
    cleanup();
    await upstream.body?.cancel().catch(() => undefined);
    return unavailableResponse();
  }

  reader = upstream.body.getReader();
  const body = new ReadableStream<Uint8Array>({
    async cancel(reason) {
      await abortUpstream(reason);
    },
    async pull(controller) {
      try {
        const next = await reader!.read();
        if (next.done) {
          finished = true;
          cleanup();
          reader!.releaseLock();
          controller.close();
          return;
        }
        controller.enqueue(next.value);
      } catch {
        cleanup();
        if (upstreamAbort.signal.aborted) {
          try {
            controller.close();
          } catch {
            // Downstream cancellation may already have closed the stream.
          }
          return;
        }
        finished = true;
        controller.error(new Error("Upstream stream failed"));
      }
    },
  });

  return new Response(body, { headers: streamHeaders });
}

function unavailableResponse(): Response {
  return new Response("Upstream stream unavailable\n", {
    headers: { "Cache-Control": "no-store" },
    status: 502,
  });
}
