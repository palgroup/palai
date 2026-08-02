import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// The approve/deny relay for the button the chat renders. It is a RELAY and not a decision: the credential
// stays server-side and the control plane owns every gate — the approver allow-list, the deadline and the
// one-shot request hash are all checked inside ApplyApprovalDecision, which is the one throat both this
// surface and Slack pass through.
//
// THE request_hash IS FORWARDED, NEVER MINTED HERE. It is the one-shot binding: it comes off the approval
// frame the run emitted, so an approval authorizes the exact call that was proposed and not whatever the
// arguments became afterwards. `POST /v1/approvals/{id}/approve` refuses a body without one — "an approval
// id alone authorizes nothing" — and a relay that supplied its own would be defeating the binding on the
// operator's behalf.
//
// UNTIL 2026-08-01 THIS ROUTE COULD NOT HAVE WORKED, and that is worth stating where someone copying it will
// read it: a publication approval was invisible to GET /v1/approvals and POST .../approve answered 404 on
// one, even with the correct hash. The button existed on other surfaces, accepted the click, and applied
// nothing until the approval expired. Both halves are fixed control-plane side.
export async function POST(request: Request): Promise<Response> {
  let body: { approvalId?: unknown; requestHash?: unknown; approve?: unknown; reason?: unknown };
  try {
    body = (await request.json()) as typeof body;
  } catch {
    return problem(400, "invalid_request", "the request body must be JSON");
  }
  const approvalId = typeof body.approvalId === "string" ? body.approvalId : "";
  const requestHash = typeof body.requestHash === "string" ? body.requestHash : "";
  if (approvalId === "" || requestHash === "") {
    return problem(400, "invalid_request", "approvalId and requestHash are both required");
  }
  const approve = body.approve !== false;

  // GUARDED FOR THE SAME REASON /api/palai/auto-approve IS, AND FOUND BY LOOKING RATHER THAN BY BEING
  // TOLD. The arming relay showed an operator a raw `SyntaxError` when the control plane was down;
  // sweeping every /api/palai/* and /api/chat/* handler for the same shape found exactly two unguarded
  // upstream fetches, and they are the two OPERATOR-DECISION surfaces — arming a session and answering
  // an approval. The others already wrap theirs. Of the two, this one is the worse place for an
  // exception to surface: it is the button standing between an agent and somebody's branch.
  let upstream: Response;
  try {
    upstream = await fetch(
      `${rawBaseURL()}/v1/approvals/${encodeURIComponent(approvalId)}/${approve ? "approve" : "deny"}`,
      {
        method: "POST",
        headers: rawHeaders(),
        body: JSON.stringify({
          request_hash: requestHash,
          ...(approve ? {} : { reason: typeof body.reason === "string" ? body.reason : "denied from the demo chat" }),
        }),
      },
    );
  } catch (error) {
    return problem(
      502,
      "connection_error",
      `the control plane did not respond: ${error instanceof Error ? error.message : "unreachable"}`,
    );
  }

  // THE UPSTREAM STATUS IS PASSED THROUGH RATHER THAN FLATTENED. 403 (not an approver), 409 (no longer
  // decidable) and 404 (unknown or foreign) have three different fixes, and a relay that turned all three
  // into "failed" would send the operator to the wrong one.
  const text = await upstream.text();
  return new Response(text, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("content-type") ?? "application/json",
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

function problem(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}
