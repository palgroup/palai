// server-only for the same reason lib/palai.ts and lib/raw.ts are: this module makes credentialed
// calls. Importing it from a Client Component is a build error.
import "server-only";

import { rawBaseURL, rawHeaders } from "@/lib/raw";

// =============================================================================================
// THE AGENT. The owner asked, in as many words: "bir agent mı tanımladık biz? ios agent'ı gibi bir
// şey? adminde agent üzerinden o agent'ı mı run ediyoruz? bu sistem promptu nereden alıyor şu an?"
//
// MEASURED ANSWER BEFORE THIS FILE EXISTED: no. `agent_id` was a dead branch in the chat route — the
// client never sent one — so every turn ran with no pinned config at all. The only steering was a
// string constant in a React file, glued onto the end of the operator's first message.
//
// And the tree already had the shape, unused. GET /v1/agents returned exactly one profile,
// `demo-coder` (aprof_afb45568…), with one PUBLISHED revision whose `instructions` was the empty
// string and whose `tools` was a non-nil list. That is this tree's most-named defect in miniature: a
// thing declared, published, carrying a capability CEILING, steering nothing, called by nobody.
//
// WHERE THE SYSTEM PROMPT LIVES NOW: on a published revision of this agent, which means it is
// versioned, auditable, visible on the console's agents screen, and — the part that matters for the
// bug this branch is about — carried by the RUN'S PINNED CONFIG rather than by a message that
// scrolls out of the window. A later turn cannot lose it.
//
// `tools` IS DELIBERATELY ABSENT FROM THE REVISION, and getting this wrong costs an hour every time.
// automation/agents.go:60, verbatim: "Tools nil imposes no capability ceiling (a non-nil set — even
// empty — is the ceiling the resolver intersects)". A revision's tools are a CEILING, not a grant:
// the project's default_tools is what grants, and the revision can only narrow it. An agent that
// names five tools on a project that grants them under other names resolves to the EMPTY set, and
// the model then says it cannot run shell commands while the run comes back green. So this revision
// names none, inherits the project grant whole, and was verified by running one:
//
//   MEASURED 2026-08-02, session ses_63274ed83d11ed252cb9eaf880142f19, three turns through this agent:
//     turn 1 "selam"             -> 0 tool calls, and it introduces itself as the iOS agent working
//                                   under `repo/` — knowledge only these instructions carry
//     turn 2 "naber"             -> 0 tool calls
//     turn 3 "build al projeyi"  -> 2 tool calls, the second `swift build --package-path repo`,
//                                   and the reply opens "Çalıştırdığım komut: swift build
//                                   --package-path repo"
//   Turn THREE is the assertion. The instruction is still steering the model at the point where the
//   old first-message-only placement had long since scrolled away.
// =============================================================================================

// The profile NAME is the identity this demo resolves on. An id is deliberately not hardcoded: a
// hardcoded id is wrong on every deployment except the one it was copied from, and the failure is
// silent — an unknown agent_id is a 404 at admission, on a stack where the agents screen shows an
// agent that looks right.
export const AGENT_NAME = "ios-coder";

// The system prompt. This is the text REPO_HINT used to carry in the browser, plus the identity
// sentence an agent needs and a request that makes the steering CHECKABLE from the outside.
//
// EVERY CLAUSE IS A MEASUREMENT, and they are recorded where they were made — app/api/chat/route.ts
// still carries the full findings for the clone layout, the `-C repo` path frame and the missing git
// identity, because that is where they were first paid for.
//
// THE LAST LINE EARNS ITS PLACE TWICE. "Name the exact command you ran" is what an operator wants
// from a coding agent anyway, and it is also the only way to tell FROM THE OUTSIDE that these
// instructions reached the model on a given turn. A system prompt whose arrival cannot be observed
// is one nobody can prove regressed.
export const AGENT_INSTRUCTIONS =
  "You are the iOS coding agent for this Palai deployment.\n" +
  "The repository is cloned at ./repo and shell commands start in the workspace root ABOVE it. " +
  "For the file tool, use paths beginning with `repo/`. For git, use `git -C repo …` and then give " +
  "paths RELATIVE TO the clone (`git -C repo add CONTRIBUTING.md`, not `repo/CONTRIBUTING.md`). " +
  "For a SwiftPM build or test, pass the package path explicitly: `swift build --package-path repo`, " +
  "`swift test --package-path repo`. " +
  "The clone has no git identity configured, so commit with " +
  "`git -C repo -c user.email=agent@palai.local -c user.name=Palai commit -m \"…\"` — do not run " +
  "`git config --global`. Do not run `git init`.\n" +
  "When you run a build, a test or any shell command, name the exact command you ran in your reply.";

// The model the revision pins. MEASURED on the live stack: a turn with no pinned model reported
// `model: "claude-sonnet-5"`, so this pins what the deployment was already choosing rather than
// moving it. `model_dispatch.go:620` reads `pinned.Model`, so this is the field that decides.
const AGENT_MODEL = "claude-sonnet-5";

export interface ResolvedAgent {
  id: string;
  name: string;
  revisionId: string;
  /** True when this process had to create the profile or publish a revision to reach this state. */
  provisioned: boolean;
}

// The resolution is cached per server process, not per request: it is three GETs on a cold start and
// zero afterwards. The promise itself is cached rather than the value, so N concurrent first turns
// share one resolution instead of racing to create N profiles.
//
// A FAILED RESOLUTION IS NOT CACHED. Caching a rejected promise would make one transient 503 at boot
// permanent for the life of the process, and the chat would report "no agent" forever on a stack
// where the agent is fine.
let inflight: Promise<ResolvedAgent> | null = null;

export function resolveCodingAgent(): Promise<ResolvedAgent> {
  if (inflight === null) {
    inflight = resolve().catch((error: unknown) => {
      inflight = null;
      throw error;
    });
  }
  return inflight;
}

async function resolve(): Promise<ResolvedAgent> {
  let provisioned = false;

  // 1. The profile, BY NAME. `GET /v1/agents` is tenant-scoped, so this cannot see another tenant's
  //    agent and cannot collide with one.
  let agentId = await findProfileByName(AGENT_NAME);
  if (agentId === null) {
    agentId = await createProfile(AGENT_NAME);
    provisioned = true;
  }

  // 2. A PUBLISHED revision whose instructions are the ones this build intends. Matching on the text
  //    rather than on mere existence is what makes an edit to AGENT_INSTRUCTIONS take effect: change
  //    the constant, and the next cold start publishes a new revision instead of quietly running the
  //    old prompt forever. Revisions are immutable and numbered, so the previous one stays readable.
  const published = await findPublishedRevision(agentId, AGENT_INSTRUCTIONS);
  if (published !== null) {
    return { id: agentId, name: AGENT_NAME, revisionId: published, provisioned };
  }

  const revisionId = await createRevision(agentId);
  await publishRevision(agentId, revisionId);
  return { id: agentId, name: AGENT_NAME, revisionId, provisioned: true };
}

async function findProfileByName(name: string): Promise<string | null> {
  const page = await readJSON<{ data?: Record<string, unknown>[] }>(`/v1/agents?limit=100`);
  const rows = Array.isArray(page.data) ? page.data : [];
  // A deployment could hold two profiles with the same name — nothing makes the name unique. The
  // FIRST is taken and that choice is stated rather than left to whatever order the page arrived in;
  // the alternative (refusing on ambiguity) would break a chat over a duplicate somebody made by hand.
  const row = rows.find((r) => typeof r.name === "string" && r.name === name);
  return row === undefined ? null : String(row.id);
}

async function findPublishedRevision(agentId: string, instructions: string): Promise<string | null> {
  const page = await readJSON<{ data?: Record<string, unknown>[] }>(
    `/v1/agents/${encodeURIComponent(agentId)}/revisions?limit=100`,
  );
  const rows = Array.isArray(page.data) ? page.data : [];
  const match = rows.find(
    (r) => r.status === "published" && typeof r.instructions === "string" && r.instructions === instructions,
  );
  return match === undefined ? null : String(match.id);
}

async function createProfile(name: string): Promise<string> {
  const body = await writeJSON<{ id?: string }>("/v1/agents", { name });
  if (typeof body.id !== "string" || body.id === "") throw new Error("POST /v1/agents returned no id");
  return body.id;
}

async function createRevision(agentId: string): Promise<string> {
  // The body carries ONLY `model` and `instructions`. The endpoint strictly decodes with
  // DisallowUnknownFields (automation/agents.go:113), so a field outside
  // {model,tools,instructions,tool_sets,mcp_connections,skills,hooks,environment} is a 400 rather
  // than something quietly ignored — and `tools` is omitted on purpose, see the header.
  const body = await writeJSON<{ id?: string }>(`/v1/agents/${encodeURIComponent(agentId)}/revisions`, {
    model: AGENT_MODEL,
    instructions: AGENT_INSTRUCTIONS,
  });
  if (typeof body.id !== "string" || body.id === "") throw new Error("POST /v1/agents/{id}/revisions returned no id");
  return body.id;
}

async function publishRevision(agentId: string, revisionId: string): Promise<void> {
  await writeJSON(
    `/v1/agents/${encodeURIComponent(agentId)}/revisions/${encodeURIComponent(revisionId)}/publish`,
    {},
  );
}

async function readJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${rawBaseURL()}${path}`, { headers: rawHeaders(), cache: "no-store" });
  if (!res.ok) throw new Error(`GET ${path} answered HTTP ${res.status}`);
  return (await res.json()) as T;
}

async function writeJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(`${rawBaseURL()}${path}`, {
    method: "POST",
    headers: rawHeaders(),
    body: JSON.stringify(body),
    cache: "no-store",
  });
  if (!res.ok) {
    const detail = await res.text().catch(() => "");
    throw new Error(`POST ${path} answered HTTP ${res.status}${detail !== "" ? `: ${detail.slice(0, 200)}` : ""}`);
  }
  return (await res.json()) as T;
}
