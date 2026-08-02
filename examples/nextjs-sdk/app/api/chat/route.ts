import { getPalaiClient } from "@/lib/palai";
import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE ADAPTER. This route is the whole answer to "can Palai be driven by a chat UI", and the answer is
// in the mapping below rather than in this sentence.
//
// IT USED TO SPEAK THE AI SDK'S UI MESSAGE STREAM, over `useChat`. The browser half is now palcore's
// chat — the owner's own, copied — which reads an AsyncGenerator of `{type, …}` frames, so this route
// emits those frames directly. That is a SMALLER protocol, not a bigger one: every part this handler
// ever wrote was a `data-*` custom part anyway, because the AI SDK has no concept for a control-plane
// approval, a replay class, or a push that was authorised and never confirmed.
//
// WHAT IS PROVEN AND WHAT IS NOT — read this before believing the demo:
//   - Palai's frames are journal events, not a token stream. `model_step.delta.v1` carries incremental
//     text when the engine emits it; a run that never deltas produces its text only at the terminal, so
//     the chat "types" in one go. That is Palai's shape and this adapter does not fake a stream it was
//     not given.
//   - Palai has no notion of an assistant message id the client can reconcile against. The browser mints
//     one per turn, which is fine for rendering and is NOT a persistence key.
//   - A session holds the conversation server-side, so only the newest user turn is forwarded. Sending
//     the history would double every prior turn into the context Palai already replays.
export async function POST(request: Request): Promise<Response> {
  let body: ChatRequest;
  try {
    body = (await request.json()) as ChatRequest;
  } catch {
    return problem(400, "invalid_request", "the request body must be JSON");
  }

  const prompt = typeof body.prompt === "string" ? body.prompt.trim() : "";
  if (prompt === "") {
    return problem(400, "invalid_request", "prompt is required");
  }

  const client = getPalaiClient();
  const encoder = new TextEncoder();

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      let closed = false;
      const emit: Emit = (event) => {
        if (closed) return;
        try {
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}\n\n`));
        } catch {
          // The browser navigated away mid-turn; the run keeps going server-side.
          closed = true;
        }
      };

      try {
        // ONE SESSION PER CHAT, carried by the client. A chat with no session opens one and announces
        // it, so the browser can send it back on the next turn — that is what makes this a CONVERSATION
        // rather than a series of unrelated single-shots.
        let sessionId = typeof body.sessionId === "string" && body.sessionId !== "" ? body.sessionId : "";
        if (sessionId === "") {
          const session = await client.sessions.create();
          sessionId = session.id;
        }
        emit({ type: "session", sessionId });

        const created = await client.responses.create({
          input: prompt,
          session_id: sessionId,
          // THE REPOSITORY HINT IS AN INSTRUCTION, NOT SOMETHING THE OPERATOR SAID.
          //
          // It used to be concatenated onto the user's own text in the browser, so somebody who typed
          // "selam" got a 470-character message in their own bubble about `git -C repo` and
          // `user.email=agent@palai.local` — measured on screen 2026-08-02, five characters typed and
          // 470 rendered. It also made the model go exploring a repository nobody had asked about.
          //
          // `instructions` is §25.12's layer 5 and the control plane inserts it as a SYSTEM message
          // after the engine's own kernel run (execution/instructions.go:94). Measured live on
          // 2026-08-02: resp_27f7e99aae66d2a9390549598b0a2f8b, instructions "end every answer with the
          // word KIRMIZI", output "Selam! Nasıl yardımcı olabilirim?\n\nKIRMIZI".
          //
          // AND IT RIDES EVERY TURN, which the old placement could not do — it was gated on
          // `messages.length === 0`, so by turn three the model no longer knew where the clone was.
          // That is not a theory about what would happen; it is what DID happen, measured on one run:
          // eight tool calls to do one build, six of them variations of `swift build` against a
          // directory that has no Package.swift, until it stumbled onto `--package-path repo`.
          ...(body.bindingId ? { instructions: REPO_HINT } : {}),
          ...(body.agentId ? { agent_id: body.agentId } : {}),
          ...(body.bindingId ? { repository: { binding_id: body.bindingId } } : {}),
        } as Parameters<typeof client.responses.create>[0]);

        const runId = str(created.run_id);
        emit({
          type: "run",
          responseId: created.id,
          runId: runId || null,
          status: str(created.status) || "queued",
        });

        const streamedText = await pumpPalaiFrames(emit, {
          sessionId,
          responseId: created.id,
          runId,
          signal: request.signal,
        });

        // THE TERMINAL ANSWER. A run that never emitted a delta frame has its text only in the
        // response's `output`, and a chat UI that renders everything except the reply is a demo that
        // proves the opposite of what it set out to. Guarded on `streamedText` so a run that DID delta
        // is not made to say everything twice.
        if (!streamedText) {
          const final = await settledOutput(client, created.id);
          if (final.text !== "") {
            emit({ type: "text_delta", text: final.text });
          } else {
            // SILENCE IS THE ONE ANSWER THAT MISLEADS. A completed run whose projection never filled
            // is a real state and the screen has to say so rather than render an empty bubble.
            emit({
              type: "notice",
              level: "warn",
              text:
                `This run reached a terminal state but its answer never became readable — ` +
                `GET /v1/responses/${created.id} still had an empty \`output\` after ${final.waitedMs}ms. ` +
                `The run itself is on the control plane and did not fail.`,
            });
          }
          if (final.usage !== null) emit({ type: "usage", ...final.usage });
        }

        emit({ type: "done" });
      } catch (error) {
        // A THROWN ERROR MUST REACH THE CHAT, not vanish into a closed stream.
        emit({ type: "error", message: error instanceof Error ? error.message : "the Palai stream failed" });
      } finally {
        closed = true;
        try {
          controller.close();
        } catch {
          // Already torn down.
        }
      }
    },
  });

  return new Response(stream, {
    headers: {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      // Next.js sits behind proxies that buffer by default; without this the whole point (watching a
      // build run) arrives in one lump at the end.
      "X-Accel-Buffering": "no",
    },
  });
}

// MEASURED, AND THE REASON THIS STRING EXISTS. `palai.workspace.shell` runs with cwd = the ALLOCATION
// ROOT, not the clone: adapters/sandboxes/host/exec.go:152 sets c.Dir = cmd.WorkspaceRoot, and the clone
// is one level down at <root>/repo. The tool's JSON schema has no cwd/workdir field, AND it does not set
// additionalProperties:false — so a `cwd` argument invented by a hopeful caller is accepted and silently
// ignored (packages/tool-broker/conformance_math.go:50).
//
// Driven live 2026-08-02 without this hint, gpt-4o-mini ran `git status`, got "not a git repository", ran
// `git init` AT THE ROOT, and committed into a brand-new empty repository beside the clone. The clone
// stayed untouched at its real HEAD. Nothing errored. A push from that state would have proposed a write
// nobody meant.
//
// THE SECOND CLAUSE IS THERE BECAUSE THE FIRST VERSION OF THIS HINT CAUSED A BUG. It said only "use
// `git -C repo …` or paths beginning with `repo/`", and the model dutifully ran
//     git -C repo add repo/CONTRIBUTING.md   -> exit 128, "pathspec did not match any files"
// because `-C repo` has ALREADY entered the clone, so the path must be relative to it.
//
// THE THIRD CLAUSE IS A PRODUCT GAP, NOT A MODEL ONE. The clone is prepared with `git init` + `git fetch`
// + `git checkout` (adapters/repositories/prepare.go:93-133) and NOTHING EVER SETS user.email OR
// user.name, so the first `git commit` of every coding session fails with "Author identity unknown".
// Measured twice on 2026-08-02, and both times the model recovered by running `git config --global …` —
// which writes the OPERATOR'S global git config, on an unsandboxed host, as a side effect of asking an
// agent to commit.
//
// THE FOURTH CLAUSE IS THE BUILD. Measured 2026-08-02: asked to build, the model ran `swift build` from
// the allocation root, got "Could not find Package.swift", and spent six more calls finding
// `--package-path repo`. The tool that runs it is the same one the first clause is about, so the frame
// has to be named for it too.
const REPO_HINT =
  "The repository is cloned at ./repo and shell commands start in the workspace root ABOVE it. " +
  "For the file tool, use paths beginning with `repo/`. For git, use `git -C repo …` and then give " +
  "paths RELATIVE TO the clone (`git -C repo add CONTRIBUTING.md`, not `repo/CONTRIBUTING.md`). " +
  "For a SwiftPM build or test, pass the package path explicitly: `swift build --package-path repo`, " +
  "`swift test --package-path repo`. " +
  "The clone has no git identity configured, so commit with " +
  "`git -C repo -c user.email=agent@palai.local -c user.name=Palai commit -m \"…\"` — do not run " +
  "`git config --global`. Do not run `git init`.";

// BACKGROUND TASKS ARE DELIBERATELY NOT MENTIONED HERE, and the reason is worth writing down because
// the obvious move is to add a clause and I nearly did.
//
// They exist: `palai.workspace.shell` takes `"background": true` and answers {task_id, output_path,
// status} (execution/tools/shell.go:103-118), with palai.workspace.background_kill to stop one. And the
// model is ALREADY TOLD — that sentence is the parameter's own `description` in the tool's JSON schema
// (shell.go:36), which is part of the definition the model is given. Repeating it here would spend
// context re-teaching something the tool already says.
//
// The stronger reason is that it may not work on this stack and I have not measured whether it does.
// shell.go:104 refuses with ErrBackgroundUnsupported when `env.Background` is nil — "the deployment does
// not do background tasks" — and nothing here has established that this deployment wires it. Telling the
// model to use a capability that answers `unavailable` would spend a tool call to earn a refusal.
// Naming an exposure is not proving the mechanism works on the target, which is CLAUDE.md's second rule
// and the one this file's own history is full of.

// pumpPalaiFrames reads Palai's SSE journal for this session and emits the chat's frames.
//
// =============================================================================================
// THE DEFECT THIS FUNCTION EXISTS TO FIX, because it is the one that made the demo look broken.
// =============================================================================================
//
// MEASURED 2026-08-02 on the live stack, session ses_0438bd21b5b6562890fabbec865c10ea. Turn one
// ("selam") rendered fine. Turn two ("build al projeyi") rendered "run queued", "run completed", and
// NOTHING ELSE — no tool calls, no answer. The run itself was perfect:
//
//     GET /v1/responses/resp_47c7eb59f756f2351ec2d35d92c10a3e/tool-calls  -> 6 rows
//       palai.workspace.shell  {"argv":["ls","-la","repo"]}
//       palai.workspace.file   {"op":"read","path":"repo/Package.swift"}
//       … and `swift build --package-path repo`
//     GET /v1/responses/resp_47c7eb59…  -> "Build başarılı oldu ✅ … Build complete! (4.59s)"
//
// The whole turn happened and the screen showed none of it. The cause is one comment in the control
// plane naming its own assumption — api/events.go:50:
//
//     "an LP session carries a single run, so its terminal is the journal's end"
//
// A CHAT IS THE COUNTEREXAMPLE. It opens one session and posts many responses into it. The old pump
// opened the stream with no cursor, so the server replayed from sequence 0, hit the FIRST run's
// `run.completed.v1`, and — by design, events.go:149 — closed the connection. The pump read that
// terminal as its own and returned. Every turn after the first was blind.
//
// Reproduced directly, long after both turns had finished:
//     curl -N '…/v1/sessions/ses_0438…/events'  ->  6 frames, all run_43a1e033 (turn ONE), then idle.
//
// TWO THINGS FIX IT, and both are needed:
//   1. FILTER BY RUN. Frames carry `run_id` and the create returns ours — measured live: the 202 body
//      has run_id (`run_992abb2f…`), it is not a field that only sometimes appears. A frame naming a
//      different run is not this turn's. Frames carrying NO run_id are let through, because
//      `approval.requested.v1` is one of them (coordinator/publication.go emits {publication_id,
//      operation, branch, request_hash, display}) and filtering strictly on run_id would silently drop
//      every approval — the one frame on the whole stream a human has to answer.
//   2. RECONNECT PAST A FOREIGN TERMINAL. The server closes on ANY terminal frame, including one that
//      belongs to a run this turn did not start. `after_sequence` is the endpoint's own documented
//      resume (events.go:112), and the sequence rides every envelope, so the pump reopens exactly where
//      it stopped. This also makes a slow-consumer drop survivable, which the endpoint deliberately
//      does (events.go:144, "the journal keeps the events").
//
// A CURSOR PRE-READ WAS THE OTHER CANDIDATE AND IT IS WORSE. Taking the journal's end before creating
// the run would skip the replay altogether, but it costs a second request per turn and — because the
// endpoint TAILS rather than ending when it runs out — it has to be bounded by a timer, which is a
// number nothing measures. Reconnecting needs neither: it discovers the cursor from the frames it was
// going to read anyway.
async function pumpPalaiFrames(
  emit: Emit,
  opts: { sessionId: string; responseId: string; runId: string; signal: AbortSignal },
): Promise<boolean> {
  const { sessionId, responseId, runId, signal } = opts;
  // Replay from the beginning of the session. Everything older than this turn is filtered out by run
  // id, and the reconnect below steps over each previous run's terminal.
  let cursor = 0;
  let streamed = false;

  // Publications whose approval APPLIED but whose push has not been confirmed. See the
  // approval.approved.v1 arm for why the two are not the same event.
  const approvedPublications = new Set<string>();
  // THE REPLAY CLASS IS ON THE EXECUTING FRAME AND NOT ON THE COMPLETED ONE, so it has to be remembered
  // across the pair, or a card an operator was shown as "irreversible" loses that the moment it finishes.
  const replayClasses = new Map<string, string>();
  // Joins run alongside the pump rather than blocking it: a stalled join must not stop the run's own
  // frames from reaching the chat. They are awaited before the stream closes so nothing is left in
  // flight.
  const pending: Promise<void>[] = [];

  // A reconnect that reads nothing is a stall, not progress. Five of them in a row ends the turn with a
  // notice rather than looping forever against an endpoint that has stopped talking.
  let emptyReconnects = 0;

  for (;;) {
    if (signal.aborted) break;

    const res = await fetch(
      `${rawBaseURL()}/v1/sessions/${encodeURIComponent(sessionId)}/events?after_sequence=${cursor}`,
      { headers: { ...rawHeaders(), Accept: "text/event-stream" }, signal, cache: "no-store" },
    );
    if (!res.ok || res.body === null) {
      throw new Error(`Palai event stream answered HTTP ${res.status}`);
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let framesThisConnection = 0;
    let ours = false;

    try {
      outer: for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        // SSE frames are separated by a BLANK LINE, and a chunk boundary can fall anywhere — including
        // inside a JSON payload. Splitting on "\n" and parsing each line is the bug this loop exists
        // not to have.
        let sep: number;
        while ((sep = buffer.indexOf("\n\n")) !== -1) {
          const evt = parseFrame(buffer.slice(0, sep));
          buffer = buffer.slice(sep + 2);
          if (evt === null) continue;

          framesThisConnection += 1;
          if (evt.sequence > cursor) cursor = evt.sequence;

          // Rule 2 above: a frame that names a DIFFERENT run is not this turn's. A frame with no run_id
          // is this turn's by construction — nothing else is streaming into this session's journal on
          // behalf of this chat.
          const frameRun = str(evt.data.run_id);
          if (runId !== "" && frameRun !== "" && frameRun !== runId) {
            if (TERMINAL_FRAMES.has(evt.type)) break outer; // the server is about to close anyway
            continue;
          }

          const terminal = writeFrame(emit, evt, responseId, pending, approvedPublications, replayClasses);
          if (terminal) {
            ours = true;
            break outer;
          }
          if (evt.type === "model_step.delta.v1") streamed = true;
        }
      }
    } finally {
      try {
        await reader.cancel();
      } catch {
        // Already torn down.
      }
    }

    if (ours || signal.aborted) break;

    emptyReconnects = framesThisConnection === 0 ? emptyReconnects + 1 : 0;
    if (emptyReconnects >= 5) {
      emit({
        type: "notice",
        level: "error",
        text:
          "The event stream closed five times without delivering a frame, so this turn stopped " +
          "following it. The run is still on the control plane; reload to pick it up from the journal.",
      });
      break;
    }
  }

  await Promise.allSettled(pending);
  return streamed;
}

const TERMINAL_FRAMES = new Set([
  "run.completed.v1",
  "run.failed.v1",
  "run.cancelled.v1",
  "run.canceled.v1",
  "run.timed_out.v1",
  "run.budget_exceeded.v1",
]);

// writeFrame maps ONE Palai journal frame onto chat frames. Returns true when THIS run reached a
// terminal state and the pump should stop.
//
// THE FRAMES THAT DO NOT MAP ARE THE FINDING, and they are listed here rather than silently dropped:
//   - attempt.recovering.v1 / recovery.proof.v1 — Palai recovered a crashed attempt and replayed from a
//     checkpoint. Rendered as a notice so the operator is not lied to by silence.
//   - tool_call.uncertain.v1 / tool_call.manual_resolution.v1 — a tool whose result was never committed
//     and which a human must resolve. Rendered as a notice.
//   - checkpoint.offer.v1, run.provisioning.v1, command.accepted.v1 — pure infrastructure with no UI
//     meaning. Dropped deliberately.
function writeFrame(
  emit: Emit,
  evt: PalaiFrame,
  responseId: string,
  pending: Promise<void>[],
  approvedPublications: Set<string>,
  replayClasses: Map<string, string>,
): boolean {
  const d = evt.data;
  switch (evt.type) {
    case "model_step.delta.v1": {
      if (typeof d.text === "string" && d.text !== "") {
        emit({ type: "text_delta", text: d.text });
      }
      return false;
    }

    // A TOOL CALL BECOMES A TOOL FRAME. The name rides the frame since E30 T2; the arguments and the
    // result do NOT, and that split is deliberate rather than an oversight to work around: an event
    // payload is POSTed to every registered webhook endpoint and stored immutably per delivery, and a
    // trivial `xcodebuild` build measures 51,422 bytes. So the bytes live on the `tool_calls` ledger and
    // are read once, when there is something to render.
    case "tool_call.executing.v1": {
      const id = str(d.tool_call_id);
      if (id !== "" && str(d.replay_class) !== "") replayClasses.set(id, str(d.replay_class));
      emit({
        type: "tool_call",
        id: id || "tool",
        toolName: str(d.tool_name),
        replayClass: str(d.replay_class) || null,
        // A run that started before this deployment took E30 T2 carries no name, and the card must not
        // be drawn as a tool named "". `nameUnavailable` is what lets it say so.
        nameUnavailable: str(d.tool_name) === "",
      });
      return false;
    }

    case "tool_call.completed.v1": {
      const id = str(d.tool_call_id);
      emit({
        type: "tool_result",
        id: id || "tool",
        toolName: str(d.tool_name),
        // Carried from the executing frame: the completed frame does not repeat it.
        replayClass: replayClasses.get(id) ?? null,
        nameUnavailable: str(d.tool_name) === "",
      });
      pending.push(joinToolCall(emit, responseId, id, str(d.tool_name)));
      return false;
    }

    // THE APPROVAL. THE FRAME IS A NOTIFICATION, NOT A PAYLOAD.
    //
    // MEASURED 2026-08-02 (coordinator/publication.go:269-272 — the emitter): a PUBLICATION approval
    // frame carries exactly {publication_id, operation, branch, request_hash, display}. It carries NO
    // approval_id, no remote, no base, no head_sha and no credential identity — so a screen that wants
    // to say WHERE the write goes and AS WHOM cannot get it from the stream at all. The coordinator says
    // so on purpose (approvals.go:162): "the genesis event carries the BINDING, never a rendered
    // screen." So the join happens HERE, server-side, where the credential already is.
    case "approval.requested.v1":
      pending.push(
        joinApproval(emit, {
          publicationId: str(d.publication_id),
          approvalId: str(d.approval_id),
          requestHash: str(d.request_hash),
          operation: str(d.operation),
          branch: str(d.branch),
          display: str(d.display),
        }),
      );
      return false;

    // AN APPROVAL THAT APPLIED IS NOT A PUSH THAT HAPPENED, and on this tree those two are routinely
    // confused because every surface a human sees says the first one.
    //
    // MEASURED 2026-08-02, end to end through this very screen: pressed Approve, the relay returned 200,
    // `approval.approved.v1` arrived carrying a command_id, the publications row moved
    // pending_approval -> approved, and the parked run woke and completed. NOTHING PUSHED. The row's
    // `receipt` stayed NULL and the git server received no git-receive-pack at all. Cause, cited:
    // repositoryPublisherFromEnv (main.go:1231) returns nil unless PALAI_GITHUB_APP_ID,
    // PALAI_GITHUB_APP_INSTALLATION_ID and PALAI_GITHUB_APP_PRIVATE_KEY_FILE are ALL set, and a nil
    // publisher makes pumpApprovedPublications a no-op — a failure its own comment calls
    // "INDISTINGUISHABLE from success on every surface a human looks at".
    case "approval.approved.v1":
      if (typeof d.publication_id === "string") approvedPublications.add(d.publication_id);
      return false;

    case "publication.published.v1":
      if (typeof d.publication_id === "string") approvedPublications.delete(d.publication_id);
      emit({
        type: "publication",
        id: str(d.publication_id) || "publication",
        publicationId: d.publication_id ?? null,
        operation: d.operation ?? null,
        receipt: d.receipt ?? null,
      });
      return false;

    case "usage.updated.v1":
      emit({ type: "usage", ...pickUsage(d) });
      return false;

    case "attempt.recovering.v1":
    case "recovery.proof.v1":
      emit({
        type: "notice",
        level: "warn",
        text: "Palai recovered a crashed attempt and replayed from a checkpoint.",
      });
      return false;

    case "tool_call.uncertain.v1":
    case "tool_call.manual_resolution.v1":
      emit({
        type: "notice",
        level: "error",
        text:
          "A tool call could not be resolved and this run is now parked waiting for a human. It will " +
          "not continue on its own.",
      });
      return false;

    case "run.failed.v1":
    case "run.cancelled.v1":
    case "run.canceled.v1":
    case "run.timed_out.v1":
    case "run.budget_exceeded.v1":
    case "run.completed.v1":
      // The unconfirmed pushes are reported BEFORE the terminal frame, so the notice sits with the
      // approval it belongs to rather than after the run's closing line.
      for (const publicationId of approvedPublications) {
        emit({
          type: "notice",
          level: "error",
          text:
            `The approval for ${publicationId} applied — the publication row moved to "approved" — but ` +
            "no publication.published.v1 arrived before this run ended, so THE PUSH IS NOT CONFIRMED. " +
            "On this stack that is a missing publisher: repositoryPublisherFromEnv returns nil unless " +
            "PALAI_GITHUB_APP_ID, PALAI_GITHUB_APP_INSTALLATION_ID and " +
            "PALAI_GITHUB_APP_PRIVATE_KEY_FILE are all set, and a nil publisher makes the approval pump " +
            "a no-op. Note this is true even when the binding carries its own connection_ref: the " +
            "credential-resolving publisher is built INSIDE the GitHub-App gate, so a binding " +
            "credential cannot publish on a stack with no App.",
        });
      }
      approvedPublications.clear();
      emit({ type: "run", responseId, status: evt.type.split(".")[1] });
      return true;

    default:
      return false;
  }
}

// settledOutput waits for the response projection to become readable and returns its text.
//
// `run.completed.v1` is journalled BEFORE the response's `output` is queryable. Retrieving the instant
// the terminal frame lands returned an EMPTY output and the chat rendered a tool call, a "completed",
// and no answer. A terminal frame is not a promise that the projection is queryable.
//
// THE BOUND IS REPORTED RATHER THAN SWALLOWED. The caller turns an expired wait into a notice on screen,
// because a chat that silently renders nothing is the defect this whole branch is about.
async function settledOutput(
  client: ReturnType<typeof getPalaiClient>,
  responseId: string,
): Promise<{ text: string; usage: Record<string, unknown> | null; waitedMs: number }> {
  const startedAt = Date.now();
  let final = await client.responses.retrieve(responseId);
  for (let i = 0; i < 40 && (final.output ?? []).length === 0; i += 1) {
    await new Promise((r) => setTimeout(r, 250));
    final = await client.responses.retrieve(responseId);
  }
  const text = (final.output ?? [])
    .map((item) => (item as { content?: unknown }).content)
    .filter((c): c is string => typeof c === "string")
    .join("\n");
  return {
    text,
    usage: final.usage ? (final.usage as unknown as Record<string, unknown>) : null,
    waitedMs: Date.now() - startedAt,
  };
}

// joinApproval turns a publication approval NOTIFICATION into the row a human can decide on.
//
// The join key is `publication_id`, not `id`: GET /v1/approvals returns a row whose own `id` is the
// approval id (apr_…) and whose `publication_id` field is what the frame carried (api/approvals.go:79).
// Matching on `id` would never hit.
//
// WHAT THE ROW ADDS OVER THE FRAME, all measured on api/approvals.go:52-95 —
//   remote, branch, base, head_sha       WHERE the write lands
//   credential_ref, credential           AS WHOM it is made
// `credential` is one of two fixed sentences the control plane chooses with the SAME condition the
// publisher branches on (store/approvals.go:269): an empty credential_ref means the deployment's GitHub
// App, NOT an unknown identity. The screen must not render an empty ref as a blank, because blank reads
// as "nobody checked" when it actually means "a different, named identity".
//
// IF THE JOIN FAILS the frame is still emitted, with joined:false — the operator gets a decidable button
// carrying the hash the frame did give, and the screen says the destination could not be read. Dropping
// it would hide an approval that is genuinely pending and leave the run parked with nothing on screen.
async function joinApproval(
  emit: Emit,
  frame: { publicationId: string; approvalId: string; requestHash: string; operation: string; branch: string; display: string },
): Promise<void> {
  const base = {
    type: "approval" as const,
    id: frame.publicationId !== "" ? frame.publicationId : frame.approvalId !== "" ? frame.approvalId : "approval",
    approvalId: frame.approvalId !== "" ? frame.approvalId : null,
    requestHash: frame.requestHash !== "" ? frame.requestHash : null,
    operation: frame.operation !== "" ? frame.operation : null,
    branch: frame.branch !== "" ? frame.branch : null,
    display: frame.display !== "" ? frame.display : null,
    publicationId: frame.publicationId !== "" ? frame.publicationId : null,
  };

  try {
    const res = await fetch(`${rawBaseURL()}/v1/approvals`, { headers: rawHeaders(), cache: "no-store" });
    if (!res.ok) throw new Error(`GET /v1/approvals answered HTTP ${res.status}`);
    const page = (await res.json()) as { data?: Record<string, unknown>[] };
    const rows = Array.isArray(page.data) ? page.data : [];
    const row =
      rows.find((r) => frame.publicationId !== "" && str(r.publication_id) === frame.publicationId) ??
      rows.find((r) => frame.approvalId !== "" && str(r.id) === frame.approvalId) ??
      null;

    if (row === null) {
      emit({
        ...base,
        joined: false,
        joinNote: "this publication is not on GET /v1/approvals — it may already have been decided",
      });
      return;
    }

    emit({
      ...base,
      joined: true,
      // The row's own id is the one POST /v1/approvals/{id}/approve wants. The frame never had it.
      approvalId: str(row.id) !== "" ? str(row.id) : base.approvalId,
      requestHash: str(row.request_hash) !== "" ? str(row.request_hash) : base.requestHash,
      operation: str(row.operation) !== "" ? str(row.operation) : base.operation,
      remote: str(row.remote) || null,
      branch: str(row.branch) || base.branch,
      baseBranch: str(row.base) || null,
      headSha: str(row.head_sha) || null,
      credentialRef: str(row.credential_ref) || null,
      credential: str(row.credential) || null,
      expiresAt: str(row.expires_at) || null,
      operatorLabel: str(row.operator_label) || null,
    });
  } catch (error) {
    emit({
      ...base,
      joined: false,
      joinNote: error instanceof Error ? error.message : "the approval row could not be read",
    });
  }
}

// joinToolCall reads ONE completed tool call's arguments and result off the ledger.
//
// WHY IT IS A READ AND NOT A WIDER FRAME, stated here because this is where a future reader will be
// tempted to "just put it on the event": automation/webhook_pump.go:328 puts an event's whole payload in
// the body it POSTs to every registered endpoint and stores that envelope immutably so a redelivery
// replays it byte-for-byte. A tool's arguments and result are model-authored and unbounded — a trivial
// `xcodebuild` build measured 51,422 bytes — so widening the frame ships megabytes off-box, once per
// endpoint, permanently, to save this one request.
//
// IT MATCHES ON THE CALL ID rather than taking the last row: a run makes many tool calls and the list is
// the whole response's. Matching by position would attach one build's output to another's card.
//
// AND IT RETRIES, WHICH IS THE SECOND DEFECT THIS BRANCH FIXES. `tool_call.completed.v1` is journalled
// before the ledger row is committed, so the first read routinely misses — the screen showed five rows
// of "This call ran, but its output could not be read" for a run whose ledger, read one minute later,
// had all eight rows with full stdout, stderr and exit_code. The join is the same one either way; the
// only thing wrong was WHEN. Measured 2026-08-02 against the live stack: see the retry bound below.
//
// A FAILED JOIN IS STILL REPORTED, NOT DROPPED. The tool frame has already been emitted, so the call is
// on screen either way; writing `joined: false` lets the screen say "the output could not be read"
// instead of rendering an empty successful build.
async function joinToolCall(emit: Emit, responseID: string, toolCallID: string, toolName: string): Promise<void> {
  if (toolCallID === "") return;
  let lastError = "";
  // TEN ATTEMPTS OVER ~3s, AND THE BOUND IS CHOSEN, NOT MEASURED — said plainly because this tree has
  // been burned by numbers that read as measured and were not. What IS measured is the failure it
  // fixes: a run whose eight ledger rows were all readable a minute later showed five "could not be
  // read" rows on screen, so the gap is sub-minute and the first read is too early. If a row still has
  // not landed after ~3s the screen SAYS the join failed, and the attempt count rides that sentence, so
  // a wrong bound is visible on the surface rather than hidden behind a longer wait.
  for (let attempt = 0; attempt < 10; attempt += 1) {
    if (attempt > 0) await new Promise((r) => setTimeout(r, 300));
    try {
      const res = await fetch(`${rawBaseURL()}/v1/responses/${encodeURIComponent(responseID)}/tool-calls`, {
        headers: rawHeaders(),
        cache: "no-store",
      });
      if (!res.ok) throw new Error(`GET /v1/responses/{id}/tool-calls answered HTTP ${res.status}`);
      const page = (await res.json()) as { data?: Record<string, unknown>[] };
      const rows = Array.isArray(page.data) ? page.data : [];
      const row = rows.find((r) => str(r.id) === toolCallID) ?? null;
      if (row === null) {
        lastError = "this call is not on the ledger read";
        continue;
      }
      emit({
        type: "tool_detail",
        id: toolCallID,
        name: str(row.name) || toolName || null,
        state: str(row.state) || null,
        replayClass: str(row.replay_class) || null,
        arguments: row.arguments ?? null,
        // `result` is ABSENT rather than null when a call has none (parked, still running, or an
        // unretained tool whose output is deliberately not stored). The screen must not draw an absent
        // result as an empty one — "nothing is known yet" and "the tool returned nothing" are different
        // sentences.
        result: Object.hasOwn(row, "result") ? row.result : null,
        hasResult: Object.hasOwn(row, "result"),
        joined: true,
      });
      return;
    } catch (error) {
      lastError = error instanceof Error ? error.message : "the tool call could not be read";
    }
  }
  emit({
    type: "tool_detail",
    id: toolCallID,
    name: toolName || null,
    joined: false,
    joinNote: `${lastError} (10 attempts over ~3s)`,
  });
}

type Emit = (event: Record<string, unknown>) => void;

interface PalaiFrame {
  type: string;
  data: Record<string, unknown>;
  sequence: number;
}

function str(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function parseFrame(frame: string): PalaiFrame | null {
  let type = "";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) type = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
  }
  if (type === "" || dataLines.length === 0) return null;
  try {
    // Palai's SSE payload is a CloudEvent envelope; the frame's own fields are under `data`, and the
    // journal sequence — the cursor the pump resumes from — is on the envelope itself.
    const envelope = JSON.parse(dataLines.join("\n")) as Record<string, unknown>;
    const inner = (envelope.data ?? {}) as Record<string, unknown>;
    const sequence = typeof envelope.sequence === "number" ? envelope.sequence : 0;
    return { type, data: inner, sequence };
  } catch {
    return null;
  }
}

function pickUsage(d: Record<string, unknown>): Record<string, unknown> {
  return {
    input_tokens: typeof d.input_tokens === "number" ? d.input_tokens : null,
    output_tokens: typeof d.output_tokens === "number" ? d.output_tokens : null,
    total_tokens: typeof d.total_tokens === "number" ? d.total_tokens : null,
  };
}

interface ChatRequest {
  prompt?: unknown;
  sessionId?: unknown;
  agentId?: string;
  bindingId?: string;
}

function problem(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}
