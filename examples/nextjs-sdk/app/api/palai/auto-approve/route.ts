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

  // AN UNREACHABLE UPSTREAM IS AN ANSWER, AND THIS FETCH USED TO BE UNGUARDED.
  //
  // MEASURED while the control plane was down: `fetch` threw, Next answered 500 with an empty body,
  // and the operator read `Failed to execute 'json' on 'Response': Unexpected end of JSON input` as
  // the response to "arm this session". That is the tool-error wedge in another costume — an ordinary
  // failure with no representation, so it escapes as something structural. A person can act on "the
  // control plane did not respond"; nobody can act on a SyntaxError.
  let res: Response;
  try {
    res = await fetch(`${rawBaseURL()}/v1/sessions/${encodeURIComponent(sessionId)}`, {
      method: "PATCH",
      headers: { ...rawHeaders(), "Content-Type": "application/json" },
      body: JSON.stringify(patch),
      cache: "no-store",
    });
  } catch (error) {
    return problem(
      502,
      "connection_error",
      `the control plane did not respond: ${error instanceof Error ? error.message : "unreachable"}`,
    );
  }

  // The upstream body is passed through UNCHANGED — it is the re-read session projection, carrying
  // both flags and the principal that armed them, and the screen renders its state from that rather
  // than from what it optimistically asked for. A toggle that shows what it REQUESTED rather than
  // what the server RECORDED is a toggle that lies whenever the request was refused.
  const text = await res.text();

  // UNCHANGED MEANS UNCHANGED-IF-IT-IS-JSON. A reachable control plane can still answer with an empty
  // body, an HTML error page or a proxy's plain-text 502, and relaying those verbatim hands the
  // browser bytes its `res.json()` cannot parse — the same exception by a longer road. The status is
  // still passed through, because 403, 409 and 404 have three different fixes.
  if (!isJSON(res.headers.get("Content-Type")) || text.trim() === "") {
    return problem(
      res.status === 200 ? 502 : res.status,
      "upstream_not_json",
      `the control plane answered HTTP ${res.status} with a body that is not JSON` +
        (text.trim() === "" ? " (it was empty)" : `: ${text.slice(0, 200)}`),
    );
  }

  return new Response(text, {
    status: res.status,
    headers: {
      "Content-Type": res.headers.get("Content-Type") ?? "application/json",
      "Cache-Control": "no-store",
    },
  });
}

function isJSON(contentType: string | null): boolean {
  return contentType !== null && /\bapplication\/(problem\+)?json\b/i.test(contentType);
}

function problem(status: number, code: string, detail: string): Response {
  return new Response(
    JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }),
    { status, headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" } },
  );
}
