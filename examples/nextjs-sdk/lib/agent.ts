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
// THE ANTI-FABRICATION CLAUSE IS THERE BECAUSE THIS FILE CAUSED THE FABRICATION IT NOW FORBIDS.
//
// "Name the exact command you ran" was added so instruction-reach could be observed from outside. On a
// deployment that grants NO tools it became an incentive to invent one. MEASURED 2026-08-02 on
// :60351, same prompt ("repo dizinini listele"), two agents, both with ZERO tool calls:
//
//   with a tools CEILING  -> "Bu ortamda gerçek bir shell/dosya sistemi aracına erişimim yok"
//                            (an honest refusal — it says it cannot)
//   with NO ceiling, ours -> "**Çalıştırdığım komut:** `ls repo`" + a fabricated JSON result
//
// The difference was not the ceiling. It was that ours ASKED for a command line and the model produced
// one from nothing. A self-verification clause that can be satisfied by invention verifies nothing, and
// on a screen that renders assistant text as markdown a fabricated transcript is indistinguishable from
// a real build. So the clause now carries its own bound.
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
//
// AND THE BUILD CLAUSE NAMES xcodebuild, WHICH IT DID NOT AT FIRST. The first version of this agent
// said `swift build --package-path repo`, the model obeyed, and the demo looked right — but
// `swift build` builds for the HOST. Package.swift declares `platforms: [.iOS(.v17)]` and the model
// even said so on screen ("Package.swift içinde platforms: [.iOS(.v17)] tanımlı olmasına rağmen,
// burada macOS SDK'sı ile derleme yapıldı"), which is the demo reporting its own defect while
// everyone read it as success. The owner asked to watch an iOS project build; the iOS half was being
// satisfied by a macOS build.
//
// MEASURED before changing the text, on the real clone rather than assumed:
//   git clone http://127.0.0.1:8177/ios-demo.git
//   xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build
//     -> ** BUILD SUCCEEDED **, compiling for `arm64-apple-ios17.0-simulator`
//        against iPhoneSimulator26.5.sdk
// So the correct command exists and works on this package; the instruction was simply naming the
// wrong one. xcodebuild has no `-C`, so it has to run from inside the clone — `cd` works in this
// shell, which was measured separately.
//
// THE LAST CLAUSE IS THE OWNER'S FIRST COMPLAINT, AND IT TOOK TWO ROUNDS TO FIND ITS REAL CAUSE.
// They said: "bir sürü tool eventi geldi, sebeplerini anlamadım." Moving the hint off their message
// took turn one from FIVE tool calls to zero — but UAT measured turn two, "naber", still running two:
// `git -C repo log --oneline -10` and `ls repo`. For a greeting.
//
// The cause is this very file. Instructions that describe the repository layout in detail read, to a
// model, as an invitation to go and look at it — the more precisely you explain where things are, the
// more it wants to confirm. Describing the workspace and never saying when NOT to act is a one-sided
// instruction, so the last clause is the other side. It is cheap to state and it is the difference
// between an agent that answers "naber" and one that runs `git log` at somebody saying hello.
//
// A SIDE EFFECT WORTH KNOWING: xcodebuild writes to DerivedData under ~/Library, NOT into the clone,
// so it does not bury the changed-files panel the way `swift build` did by writing `.build/` inside
// the package. The panel collapses those either way (workspace-panel.tsx) — one fix should not be
// load-bearing for the other.
export const AGENT_INSTRUCTIONS =
  "You are the iOS coding agent for this Palai deployment.\n" +
  "The repository is cloned at ./repo and shell commands start in the workspace root ABOVE it. " +
  "For the file tool, use paths beginning with `repo/`. For git, use `git -C repo …` and then give " +
  "paths RELATIVE TO the clone (`git -C repo add CONTRIBUTING.md`, not `repo/CONTRIBUTING.md`). " +
  "This is an iOS package. To build or test it FOR iOS, run xcodebuild from inside the clone against " +
  "a Simulator destination — `cd` works in this shell:\n" +
  "  bash -c \"cd repo && xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build\"\n" +
  "  bash -c \"cd repo && xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' test\"\n" +
  "Use `xcrun simctl list devices available` to see the destinations that exist before naming one. " +
  "`swift build --package-path repo` also works but it builds for macOS, NOT for iOS, so do not use it " +
  "when the request is about the iOS app — and say which of the two you ran. " +
  "The clone has no git identity configured, so commit with " +
  "`git -C repo -c user.email=agent@palai.local -c user.name=Palai commit -m \"…\"` — do not run " +
  "`git config --global`. Do not run `git init`.\n" +
  "When you run a build, a test or any shell command, name the exact command you ran in your reply.\n" +
  "NEVER WRITE A COMMAND, A TERMINAL TRANSCRIPT OR A RESULT YOU DID NOT ACTUALLY GET FROM A TOOL. If " +
  "you have no tool available for what was asked, say exactly that and stop — do not describe what the " +
  "command would have printed, and do not format an imagined result as though a tool returned it. An " +
  "invented build log is worse than no answer, because the person reading it cannot tell.\n" +
  // CARRIED OVER from app/api/palai/agents/route.ts when the two agent definitions were merged into
  // this one. These three rules were that route's and are not in this file's history — folding them in
  // is what makes "one creator" a merge rather than a deletion. The screenshot rule is the one that
  // matters most for a demo whose output is a picture: without being told to SHOW the result, a model
  // that finishes a build reports that it finished a build.
  "Read before you edit — never guess a file's contents.\n" +
  "After a visible change, take a screenshot, show it with palai.workspace.show_media, say what " +
  "changed, and ask whether it looks right.\n" +
  "Prefer the smallest change that does the job. Do not refactor code you were not asked about.\n" +
  "DO NOT USE TOOLS UNLESS THE REQUEST NEEDS THEM. A greeting, a thank-you, a question about what you " +
  "can do, or small talk is answered in words alone — do not list files, do not read git history, do " +
  "not look around the repository first. Reach for a tool only when the operator has asked for a " +
  "change, a build, a test, or a fact that can only be obtained by looking. When you are unsure what " +
  "they want, ASK rather than explore.";

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
