import { getPalaiClient } from "@/lib/palai";
import { problem, relayError } from "@/lib/relay";

// The SDK's server path uses node:crypto, so this runs on the Node runtime; force-dynamic keeps it
// out of the static build (the credential is never present at build time).
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// POST relays a durable steer/interrupt command to a session — E08's steering product, driven from
// the SDK. The browser sends only { sessionId, message, mode? }; the API key stays server-side (see
// lib/palai.ts). command_id idempotency is minted inside the SDK. The browser never holds a token:
// this is the server-relay that makes the browser-direct-token DROP correct rather than limiting.
export async function POST(request: Request): Promise<Response> {
  let sessionId: string;
  let message: string;
  let mode: "steer" | "interrupt";
  try {
    const body = (await request.json()) as { sessionId?: unknown; message?: unknown; mode?: unknown };
    if (typeof body.sessionId !== "string" || body.sessionId === "") {
      return problem(400, "invalid_request", "a 'sessionId' string is required");
    }
    if (typeof body.message !== "string" || body.message.trim() === "") {
      return problem(400, "invalid_request", "a non-empty 'message' string is required");
    }
    sessionId = body.sessionId;
    message = body.message;
    mode = body.mode === "interrupt" ? "interrupt" : "steer";
  } catch {
    return problem(400, "invalid_request", "request body must be JSON with a 'sessionId' and 'message'");
  }

  const client = getPalaiClient();
  try {
    const command =
      mode === "interrupt"
        ? await client.sessions.commands.interrupt(sessionId, { message })
        : await client.sessions.commands.steer(sessionId, { message });
    // Curated projection only — id, kind, delivery, status — never the raw command payload.
    return Response.json({ id: command.id, kind: command.kind, delivery: mode, status: command.status });
  } catch (err) {
    return relayError(err);
  }
}
