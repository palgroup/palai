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

// iosInstructions is the RULE SET. It is here rather than in a config file because the demo's whole
// claim is that the rules are visible: a reader can see exactly what the agent was told, and change it.
//
// The last sentence is the one that matters for a demo whose output is a screenshot: without being told
// to SHOW the result, a model that finishes a build reports that it finished a build.
const iosInstructions = `You are an iOS engineer working in a cloned repository on a Mac.

Rules:
- Read before you edit. Never guess a file's contents.
- Build with xcodebuild and run on a simulator to check your work.
- NEVER claim a build passed without showing its output. If it failed, name the file and the line.
- After a visible change, take a screenshot, show it, say what changed, and ask whether it looks right.
- Prefer the smallest change that does the job. Do not refactor code you were not asked about.`;

// iosTools is the least a coding agent needs to do this job: read and write files, run commands, and
// record the result. Publish tools are deliberately ABSENT — a demo that could push without being asked
// is a demo that pushes while nobody is watching, and the approval story is the point of the exercise.
const iosTools = ["palai.workspace.file", "palai.workspace.shell", "palai.workspace.commit"];

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
export async function POST(request: Request): Promise<Response> {
  let name = "ios-agent";
  try {
    const body = (await request.json()) as { name?: string };
    if (typeof body?.name === "string" && body.name.trim() !== "") name = body.name.trim();
  } catch {
    // An empty body is the ordinary case — the default name is fine.
  }

  try {
    const profile = await post(`/v1/agents`, { name });
    if (!profile.ok) return relayFailure("agent profile", profile);
    const agentID = (profile.json as { id?: string }).id;
    if (!agentID) return problem(502, "agent_malformed", "the control plane created a profile with no id");

    const revision = await post(`/v1/agents/${encodeURIComponent(agentID)}/revisions`, {
      model: process.env.PALAI_MODEL?.trim() || "gpt-4o-mini",
      instructions: iosInstructions,
      tools: iosTools,
    });
    if (!revision.ok) return relayFailure("agent revision", revision);
    const revisionID = (revision.json as { id?: string }).id;
    if (!revisionID) return problem(502, "agent_malformed", "the control plane created a revision with no id");

    // THE STEP THAT MAKES IT REAL. Skipping it leaves a draft that no run will ever be given.
    const published = await post(`/v1/agents/${encodeURIComponent(agentID)}/revisions/${encodeURIComponent(revisionID)}/publish`, {});
    if (!published.ok) return relayFailure("agent publish", published);

    return Response.json({
      agentId: agentID,
      revisionId: revisionID,
      name,
      tools: iosTools,
      instructions: iosInstructions,
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
