import { AGENT_INSTRUCTIONS, AGENT_NAME, resolveCodingAgent } from "@/lib/agent";
import { problem } from "@/lib/relay";
import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE iOS AGENT: the rules, tools and skills a run is given, as a durable object rather than a prompt
// pasted into every request.
//
// WHY AN AGENT AND NOT `instructions` ON THE RESPONSE. Both work for one run. An agent is what makes the
// twentieth run identical to the first: it is versioned, published, and referenced by id, so "what the
// model was told" is a thing you can look up after the fact instead of reconstructing from whatever the
// client happened to send that day. The response then carries `agent_id` and nothing else.
//
// MEASURED 2026-08-02 against the live control plane, because the shape is not one call:
//
//   POST /v1/agents                                    -> aprof_… (a NAMED LINEAGE, no config)
//   POST /v1/agents/{id}/revisions                     -> arev_… (a DRAFT carrying the config)
//   POST /v1/agents/{id}/revisions/{rev}/publish       -> makes that draft the one runs get
//
// The accepted revision fields came from the API itself rather than from reading Go — sending an
// unsupported one answers, verbatim:
//
//   "the revision carries an unsupported field (accepted: model, tools, instructions, tool_sets,
//    mcp_connections, skills, hooks)"
//
// ‼️ A DRAFT THAT IS NEVER PUBLISHED IS NOT AN ERROR AND NOT AN AGENT. It is created, it has an id, and
// runs do not get it. So this route publishes in the same request: a caller that got a 201 and assumed
// it had an agent would be pointing runs at a lineage whose config nothing ever applied.

// THE RULE SET AND THE TOOL LIST THAT USED TO LIVE HERE HAVE MOVED, NOT VANISHED.
//
// Its three rules a reader would miss — read before you edit, show a screenshot after a visible change,
// prefer the smallest change — are now in AGENT_INSTRUCTIONS in lib/agent.ts, folded in when the two
// agent definitions became one. That is what makes this a merge rather than a deletion.
//
// The tool list did NOT move, and that is the decision rather than an oversight: see the POST below for
// the measurement. Deleting them here is deliberate — an unused `const` naming a second, different
// agent config is the thing a future reader copies back into service.

export async function GET(): Promise<Response> {
  try {
    const upstream = await fetch(`${rawBaseURL()}/v1/agents`, { headers: rawHeaders(), cache: "no-store" });
    const text = await upstream.text();
    return new Response(text, {
      status: upstream.status,
      headers: { "Content-Type": "application/json", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" },
    });
  } catch (err) {
    return problem(502, "agents_unavailable", `the control plane could not be reached: ${String(err)}`);
  }
}

// POST creates the iOS agent: lineage, revision, publish. It answers with the id a response carries.
// THE POST NO LONGER CREATES AN AGENT OF ITS OWN. It delegates to lib/agent.ts, which is the single
// definition of the iOS agent in this tree.
//
// WHY, AND IT IS NOT A PREFERENCE. Until 2026-08-02 there were TWO creators: this route, and
// resolveCodingAgent(). They disagreed on all three fields that decide what a run can do — this one
// published `tools: [file, shell, commit]`, `model: gpt-4o-mini`, and a different rule set; the other
// omits `tools`, pins `claude-sonnet-5`, and carries the repository layout the demo measured. Two
// creators is not a redundancy, it is a coin toss nobody can see land.
//
// WHAT SETTLED IT WAS MEASUREMENT, NOT SENIORITY:
//   * a real chat turn resolves `ios-coder` (aprof_ec2d04f4…, provisioned:false). This route is
//     reachable only by hand — NOTHING in the UI calls it, verified by grep across app, components,
//     lib and tests — so no run has ever resolved what it publishes.
//   * it created a NEW profile on EVERY call. `POST /v1/agents` unconditionally, no resolve-by-name,
//     so `curl`ing it twice leaves two lineages named "ios-agent" and a later reader cannot tell which
//     one a run got. The same shape put EIGHT bindings for one repository in the rail.
//   * THE CEILING COULD NOT BE PROVEN EITHER WAY ON THIS STACK, and that is stated rather than
//     resolved: with an empty project grant, an agent WITH the ceiling and an agent WITHOUT it both
//     got zero tools. A ceiling clamps the project grant (automation/agents.go:60 — a non-nil set,
//     even empty, is the ceiling the resolver intersects), and there is nothing here to clamp. On the
//     stack that did grant tools (:63925) the ceiling-free agent ran six calls and a real xcodebuild.
//     So the evidence supports omitting it and does NOT support adding it; if somebody later wants the
//     ceiling, it belongs in lib/agent.ts with a run that shows it refusing something.
//
// THE SECURITY INTENT OF THE OLD LIST IS NOT LOST. Its comment said publish tools were absent so the
// demo "could not push without being asked" — but that is enforced by the APPROVAL GATE, which parks a
// publication and waits for a human, not by a tool list. A ceiling that silently empties the tool set
// makes the model claim it has no shell; it does not make a push safer.
export async function POST(request: Request): Promise<Response> {
  // The body is still read so an existing caller passing {name} gets a clear answer rather than a
  // silently ignored field: this route has one agent to give and it is not renameable from here.
  let requested = "";
  try {
    const body = (await request.json()) as { name?: string };
    if (typeof body?.name === "string") requested = body.name.trim();
  } catch {
    // An empty body is the ordinary case.
  }
  if (requested !== "" && requested !== AGENT_NAME) {
    return problem(
      409,
      "agent_name_fixed",
      `this deployment defines one coding agent, "${AGENT_NAME}". Creating a second one under another ` +
        "name is what produced two agents disagreeing about their own tools; edit lib/agent.ts instead.",
    );
  }

  try {
    const agent = await resolveCodingAgent();
    return Response.json({
      agentId: agent.id,
      revisionId: agent.revisionId,
      name: agent.name,
      provisioned: agent.provisioned,
      // `tools` is reported as null rather than omitted: null is the stored value and it MEANS
      // "no ceiling", which is a different statement from "this response forgot to say".
      tools: null,
      instructions: AGENT_INSTRUCTIONS,
    });
  } catch (err) {
    return problem(502, "agent_create_failed", String(err));
  }
}

async function post(path: string, body: unknown): Promise<{ ok: boolean; status: number; json: unknown; text: string }> {
  const res = await fetch(`${rawBaseURL()}${path}`, {
    method: "POST",
    headers: rawHeaders(),
    body: JSON.stringify(body),
    cache: "no-store",
  });
  const text = await res.text();
  let json: unknown = null;
  try {
    json = JSON.parse(text);
  } catch {
    // A non-JSON body from the control plane is itself the diagnosis; it is carried through as text.
  }
  return { ok: res.ok, status: res.status, json, text };
}

// relayFailure carries the control plane's OWN problem document through rather than replacing it with a
// generic one. Its `detail` is the sentence that names the unsupported field or the missing capability,
// and that sentence is the whole value of the failure.
function relayFailure(step: string, res: { status: number; text: string }): Response {
  return new Response(res.text, {
    status: res.status,
    headers: { "Content-Type": "application/json", "X-Palai-Failed-Step": step },
  });
}
