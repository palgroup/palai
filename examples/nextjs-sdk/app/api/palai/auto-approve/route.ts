import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE SESSION'S STANDING AUTHORIZATION, RELAYED (E30 T1).
//
// NO KEY IN THE BROWSER. The screen sends `{sessionId, tools?, publications?}` here; the Palai
// credential is read server-side in lib/raw.ts, which is guarded by `server-only`.
//
// THE TWO HALVES ARE FORWARDED SEPARATELY AND ONLY WHEN PRESENT, which is the whole reason this
// relay does not simply pass the body through. `auto_approve_tools` and `auto_approve_publications`
// are nullable in the published schema precisely so that ABSENT means "not touching that half": a
// relay that helpfully filled in `false` for the half the operator did not mention would turn
// "arm the build commands" into "and disarm my pushes", silently, on every click.

export async function POST(request: Request): Promise<Response> {
  let body: { sessionId?: unknown; tools?: unknown; publications?: unknown };
  try {
    body = (await request.json()) as typeof body;
  } catch {
    return problem(400, "invalid_request", "the request body must be JSON");
  }

  const sessionId = typeof body.sessionId === "string" ? body.sessionId.trim() : "";
  if (sessionId === "") {
    return problem(400, "invalid_request", "sessionId is required");
  }

  const patch: Record<string, boolean> = {};
  if (typeof body.tools === "boolean") patch.auto_approve_tools = body.tools;
  if (typeof body.publications === "boolean") patch.auto_approve_publications = body.publications;
  if (Object.keys(patch).length === 0) {
    // Refused rather than sent as an empty PATCH. The control plane answers 400 for a body that
    // changes nothing, and an empty click reaching it would surface as a confusing upstream error
    // rather than as the local mistake it is.
    return problem(400, "invalid_request", "one of tools or publications must be a boolean");
  }

  const res = await fetch(`${rawBaseURL()}/v1/sessions/${encodeURIComponent(sessionId)}`, {
    method: "PATCH",
    headers: { ...rawHeaders(), "Content-Type": "application/json" },
    body: JSON.stringify(patch),
    cache: "no-store",
  });

  // The upstream body is passed through UNCHANGED — it is the re-read session projection, carrying
  // both flags and the principal that armed them, and the screen renders its state from that rather
  // than from what it optimistically asked for. A toggle that shows what it REQUESTED rather than
  // what the server RECORDED is a toggle that lies whenever the request was refused.
  const text = await res.text();
  return new Response(text, {
    status: res.status,
    headers: {
      "Content-Type": res.headers.get("Content-Type") ?? "application/json",
      "Cache-Control": "no-store",
    },
  });
}

function problem(status: number, code: string, detail: string): Response {
  return new Response(
    JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }),
    { status, headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" } },
  );
}
