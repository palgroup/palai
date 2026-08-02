"use client";

import { Loader2, Send, Sparkles, Square } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { type AIStreamEvent, streamGenerate } from "@/components/ai-chat/api";
import { MessageResponse } from "@/components/ai-elements/message";
import { MediaPart, type MediaPartData } from "@/components/media-parts";
import { SubagentPart, type ChildPart } from "@/components/subagent-parts";
import { ApprovalPart, NoticePart, PublicationPart, ToolDetailPart, ToolPart } from "@/components/chat-parts";
import type { Binding } from "@/components/repository-picker";
import { fetchHistory, type HistoryEvent } from "@/lib/history";

// =============================================================================================
// COPIED FROM palcore — apps/web/src/ai-chat/AIChat.tsx — AND POINTED AT PALAI.
//
// The owner's directive, verbatim: "şu ui tarafını palcore'un chat'i ile birebir aynı yap direkt onu
// al kopyala sonra palai sdk kullanacak şekilde güncelle."
//
// WHAT IS palcore's, KEPT: the message model (`Message` / `ToolCall` with `status: running|done`), the
// `handleStreamEvent` switch, the per-turn AbortController and its Square stop button, the auto-scroll
// effect, Enter-to-send with Shift+Enter for a newline, the glass response surface, the rounded-full
// input pill, the Sparkles button, the conic-gradient border that spins while the model is working, and
// the three pulsing thinking dots.
//
// TWO THINGS palcore DOES THAT THIS DOES NOT, AND THEY ARE THE OWNER'S OWN BUG REPORT.
//
// The complaint was "tool call'ları da görmüyorum". palcore cannot show them, for two independent
// reasons, and copying it faithfully would have reproduced the complaint exactly:
//
//   1. THE RENDERING IS COMMENTED OUT. AIChat.tsx:626 says "Tool calls and thinking are hidden from
//      client" and :652 repeats it where the markup would go. Measured: `toolCalls` appears 13 times in
//      palcore's source and NOT ONCE inside JSX. The state is maintained and never drawn.
//   2. FINISHED CALLS ARE DELETED FROM STATE. AIChat.tsx:278-288 runs a timer that filters out every
//      call whose `doneAt` is more than 1500ms old. So even with the rendering restored, a tool call
//      would appear and then disappear a second and a half later.
//
// palcore is a design tool: its user wants the screens, not the machinery. This is a CODING chat, where
// the machinery is the product. So the calls are rendered, and they stay.
// =============================================================================================

// ── Message Model — palcore's, plus the parts Palai has and palcore does not ──────────────────

interface ToolCall {
  /** Palai's `tool_call_id`. palcore uses a monotonic counter and matches results by NAME; see api.ts. */
  id: string;
  name: string;
  status: "running" | "done";
  nameUnavailable: boolean;
  replayClass: string | null;
  /** The ledger join: what the call ran and what came back. Absent until it lands, or if it fails. */
  detail?: Record<string, unknown>;
}

interface Message {
  id: string;
  role: "user" | "ai";
  content: string;
  thinking?: string;
  timestamp: number;
  toolCalls?: ToolCall[];
  /** Palai-only: a decision the CONTROL PLANE is parked on, keyed by publication id. */
  approvals?: Record<string, unknown>[];
  publications?: Record<string, unknown>[];
  notices?: { level: string; text: string }[];
  /** Delegated runs, from main's child.* arms — "waiting on four children" must not look like "stuck". */
  subagents?: ChildPart[];
  /** What the agent chose to SHOW you: a screenshot or a recording, fetched through the relay. */
  media?: MediaPartData[];
  /**
   * The run's terminal state, rendered ONCE and only after the turn settles.
   *
   * It used to be two chat lines — "run queued" then "run completed" — sitting in the transcript above
   * the answer. That is the "bir sürü tool eventi geldi, sebeplerini anlamadım" half of the owner's
   * report: infrastructure states narrated at an operator who asked a question. A turn that ended is
   * worth one quiet line; a turn that is still queued is what the spinner is for.
   */
  runStatus?: string;
  isStreaming?: boolean;
  /**
   * Set when an error event arrives mid-stream. The message's content stays whatever the AI emitted
   * before the failure (legible markdown — colouring it red made the UI look like an error wall when
   * the actual prose was fine). The error itself is rendered as a small chip below the content.
   */
  isError?: boolean;
  errorText?: string;
}

// replayMessages turns a journal replay into THIS component's message model.
//
// main's lib/history.ts has `toUIMessages`, which builds the AI SDK's `UIMessage` — the shape this
// branch stopped using. The EVENTS are the shared contract, not the message type, so this reads the
// same HistoryEvent list rather than converting one message model into another. Tool calls replay as
// completed with their arguments already joined, because a replayed call is by definition finished.
function replayMessages(events: HistoryEvent[]): Message[] {
  const out: Message[] = [];
  let ai: Message | null = null;
  const startAI = (): Message => {
    if (ai === null) {
      ai = { id: `replay-ai-${out.length}`, role: "ai", content: "", timestamp: Date.now(), toolCalls: [] };
      out.push(ai);
    }
    return ai;
  };
  for (const e of events) {
    if (e.type === "user" && typeof e.text === "string") {
      ai = null;
      out.push({ id: `replay-user-${out.length}`, role: "user", content: e.text, timestamp: Date.now() });
      continue;
    }
    if (e.type === "text" && typeof e.text === "string") {
      const m = startAI();
      m.content += e.text;
      continue;
    }
    if (e.type === "tool" && e.tool) {
      const m = startAI();
      m.toolCalls = [
        ...(m.toolCalls ?? []),
        {
          id: e.tool.id ?? `replay-tool-${(m.toolCalls ?? []).length}`,
          name: e.tool.name ?? "",
          status: "done",
          nameUnavailable: (e.tool.name ?? "") === "",
          replayClass: null,
          detail:
            e.tool.arguments === undefined && e.tool.result === undefined
              ? undefined
              : { joined: true, arguments: e.tool.arguments ?? null, result: e.tool.result ?? null, hasResult: e.tool.result !== undefined },
        },
      ];
    }
  }
  return out;
}

export function AIChat({
  binding,
  onSession,
  onResponse,
  onBusyChange,
}: {
  binding: Binding | null;
  onSession: (sessionId: string) => void;
  onResponse: (responseId: string) => void;
  onBusyChange: (busy: boolean) => void;
}) {
  const [messages, setMessages] = useState<Message[]>([]);
  const [input, setInput] = useState("");
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [streaming, setStreaming] = useState(false);
  const [status, setStatus] = useState<"thinking" | "working">("thinking");
  // Which agent the session runs as. Null until the first turn resolves it — a session does not exist
  // before then either, and claiming an agent before anything has been pinned would be the same class
  // of lie this screen keeps finding.
  const [agent, setAgent] = useState<{ id: string | null; name: string | null; note: string } | null>(null);
  // A replayed transcript that is INCOMPLETE must not read as a complete one; see the chips below.
  const [resumed, setResumed] = useState<{ truncated: boolean; drained: boolean } | null>(null);

  const inputRef = useRef<HTMLTextAreaElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  // The transport reads the current session and binding WITHOUT re-creating the send callback on every
  // keystroke, so both ride refs alongside the state that renders them.
  const sessionRef = useRef<string | null>(null);
  const bindingRef = useRef<string>("");
  bindingRef.current = binding?.id ?? "";

  useEffect(() => {
    onBusyChange(streaming);
  }, [streaming, onBusyChange]);

  // RESUMING A SESSION: ?session=<id> opens the chat on a conversation that already exists, with its
  // transcript replayed from the journal. PORTED FROM main's coding-chat at the merge — it lived beside
  // `useChat`'s setMessages, and this component owns the message list now, so it moved rather than
  // being reimplemented. main's route, lib/history.ts and its two partial-transcript warnings are
  // untouched.
  //
  // WHY THE URL AND NOT LOCAL STORAGE (main's reasoning, kept): a session id in the address bar is a
  // link — it survives a reload and can be pasted to a colleague. Local storage would make "the session
  // I was in" a property of one browser profile.
  //
  // It runs ONCE and only when there is no session yet: a resume firing after the chat had started its
  // own session would replace live messages with an older transcript.
  useEffect(() => {
    if (typeof window === "undefined" || sessionRef.current !== null) return;
    const resume = new URLSearchParams(window.location.search).get("session")?.trim();
    if (!resume) return;
    let cancelled = false;
    void (async () => {
      const history = await fetchHistory(resume);
      if (cancelled || history === null) return;
      sessionRef.current = history.sessionId;
      setSessionId(history.sessionId);
      onSession(history.sessionId);
      setMessages(replayMessages(history.events));
      setResumed({ truncated: history.truncated, drained: history.drained });
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Auto-scroll — palcore's, unchanged.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: "smooth" });
  }, [messages]);

  const patchAI = useCallback((aiMsgId: string, fn: (m: Message) => Message) => {
    setMessages((prev) => prev.map((m) => (m.id === aiMsgId ? fn(m) : m)));
  }, []);

  const handleStreamEvent = useCallback(
    (aiMsgId: string, event: AIStreamEvent) => {
      switch (event.type) {
        case "session":
          sessionRef.current = event.sessionId;
          setSessionId(event.sessionId);
          onSession(event.sessionId);
          break;

        case "agent":
          setAgent({ id: event.agentId, name: event.agentName, note: event.note });
          break;

        case "thinking_delta":
          patchAI(aiMsgId, (m) => ({ ...m, thinking: (m.thinking ?? "") + event.text }));
          break;

        case "text_delta":
          patchAI(aiMsgId, (m) => ({ ...m, content: m.content + event.text }));
          break;

        case "tool_call":
          setStatus("working");
          patchAI(aiMsgId, (m) => {
            const calls = m.toolCalls ?? [];
            // A frame may repeat on a journal replay; keying by id makes that idempotent, which the
            // counter palcore increments cannot be.
            if (calls.some((c) => c.id === event.id)) return m;
            return {
              ...m,
              toolCalls: [
                ...calls,
                {
                  id: event.id,
                  name: event.toolName,
                  status: "running",
                  nameUnavailable: event.nameUnavailable,
                  replayClass: event.replayClass,
                },
              ],
            };
          });
          break;

        case "tool_result":
          patchAI(aiMsgId, (m) => ({
            ...m,
            toolCalls: (m.toolCalls ?? []).map((c) =>
              c.id === event.id
                ? {
                    ...c,
                    status: "done" as const,
                    name: event.toolName !== "" ? event.toolName : c.name,
                    nameUnavailable: event.nameUnavailable && c.nameUnavailable,
                    // The completed frame does not repeat the replay class; keep what the executing
                    // frame said rather than blanking a field the operator was already shown.
                    replayClass: event.replayClass ?? c.replayClass,
                  }
                : c,
            ),
          }));
          break;

        case "tool_detail":
          patchAI(aiMsgId, (m) => ({
            ...m,
            toolCalls: (m.toolCalls ?? []).map((c) =>
              c.id === event.id ? { ...c, detail: event as unknown as Record<string, unknown> } : c,
            ),
          }));
          break;

        case "approval":
          patchAI(aiMsgId, (m) => {
            const rows = m.approvals ?? [];
            const data = event as unknown as Record<string, unknown>;
            const idx = rows.findIndex((r) => r.id === event.id);
            return {
              ...m,
              approvals: idx === -1 ? [...rows, data] : rows.map((r, i) => (i === idx ? data : r)),
            };
          });
          break;

        case "publication":
          patchAI(aiMsgId, (m) => ({
            ...m,
            publications: [...(m.publications ?? []), event as unknown as Record<string, unknown>],
          }));
          break;

        case "subagent":
          patchAI(aiMsgId, (m) => {
            const rows = m.subagents ?? [];
            const next = {
              id: event.id,
              requestId: event.requestId,
              state: event.state,
              status: event.status,
              reason: event.reason,
            } as ChildPart;
            // Keyed by id so the `requested` row BECOMES the `completed` one rather than stacking two
            // rows for one child — the terminal arrives on the same id.
            const idx = rows.findIndex((r) => r.id === event.id);
            return { ...m, subagents: idx === -1 ? [...rows, next] : rows.map((r, i) => (i === idx ? next : r)) };
          });
          break;

        case "media":
          patchAI(aiMsgId, (m) => {
            const rows = m.media ?? [];
            if (rows.some((r) => r.artifactId === event.artifactId)) return m;
            return { ...m, media: [...rows, event as unknown as MediaPartData] };
          });
          break;

        case "notice":
          patchAI(aiMsgId, (m) => ({
            ...m,
            notices: [...(m.notices ?? []), { level: event.level, text: event.text }],
          }));
          break;

        case "run":
          if (event.responseId) onResponse(event.responseId);
          patchAI(aiMsgId, (m) => ({ ...m, runStatus: event.status }));
          break;

        case "done":
          patchAI(aiMsgId, (m) => ({ ...m, isStreaming: false }));
          break;

        case "error":
          // Content already streamed before the error stays as-is; the error itself goes into errorText
          // so the chip can be rendered below it rather than recolouring legitimate prose.
          patchAI(aiMsgId, (m) => ({ ...m, isStreaming: false, isError: true, errorText: event.message }));
          break;
      }
    },
    [patchAI, onSession, onResponse],
  );

  const handleCancel = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
  }, []);

  const handleSend = useCallback(async () => {
    const text = input.trim();
    if (text === "" || streaming) return;

    const aiMsgId = `ai-${Date.now()}`;
    // THE OPERATOR'S MESSAGE IS WHAT THE OPERATOR TYPED. Nothing is appended to it. The repository hint
    // that used to be concatenated here is an `instructions` layer now, sent server-side on every turn;
    // see app/api/chat/route.ts. Measured before the change: five characters typed, 470 rendered.
    setMessages((prev) => [
      ...prev,
      { id: `user-${Date.now()}`, role: "user", content: text, timestamp: Date.now() },
      { id: aiMsgId, role: "ai", content: "", timestamp: Date.now(), isStreaming: true, toolCalls: [] },
    ]);
    setInput("");
    setStreaming(true);
    setStatus("thinking");

    const controller = new AbortController();
    abortRef.current = controller;

    try {
      for await (const event of streamGenerate({
        prompt: text,
        sessionId: sessionRef.current ?? undefined,
        bindingId: bindingRef.current !== "" ? bindingRef.current : undefined,
        signal: controller.signal,
      })) {
        handleStreamEvent(aiMsgId, event);
      }
    } catch (err) {
      if (err instanceof Error && err.name === "AbortError") {
        patchAI(aiMsgId, (m) => ({
          ...m,
          content: `${m.content}\n\n[Cancelled]`,
          isStreaming: false,
        }));
      } else {
        // Keep whatever content arrived before the throw — overwriting it with the error string would
        // erase legitimate AI prose. Surface the error separately via errorText.
        patchAI(aiMsgId, (m) => ({
          ...m,
          isStreaming: false,
          isError: true,
          errorText: err instanceof Error ? err.message : String(err),
        }));
      }
    } finally {
      setStreaming(false);
      abortRef.current = null;
    }
  }, [input, streaming, handleStreamEvent, patchAI]);

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      void handleSend();
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {/* ── The conversation ─────────────────────────────────────────────────────────
          palcore's response card is a glass surface with its own scroll region. Here it holds the whole
          turn rather than only the last AI sentence, because the machinery IS the product on this
          screen. */}
      <div ref={scrollRef} className="min-h-0 flex-1 overflow-y-auto px-3 py-4" data-testid="chat-scroll">
        {messages.length === 0 ? (
          <div className="px-2 py-8 text-center" data-testid="chat-empty">
            <Sparkles size={18} className="mx-auto mb-2 text-ai" aria-hidden />
            <p className="text-[13px] text-text-primary">
              {binding ? `Ready on ${binding.repository_identity}` : "Pick a repository first"}
            </p>
            <p className="mt-1 text-[12px] text-text-tertiary leading-4">
              {binding
                ? "Ask for a change. The clone, the shell and any publication happen inside that repository."
                : "Choose one in the rail — that is what gives the session a clone to work in."}
            </p>
          </div>
        ) : null}

        <div className="space-y-4">
          {messages.map((m) =>
            m.role === "user" ? (
              <div key={m.id} className="flex justify-end" data-testid="chat-message-user">
                <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-white/10 px-3 py-2 text-[13px] text-text-primary">
                  {m.content}
                </div>
              </div>
            ) : (
              <div key={m.id} className="glass-elevated rounded-2xl p-3" data-testid="chat-message-ai">
                {/* TOOL CALLS COME FIRST AND THEY STAY. This is the half palcore hides and deletes. */}
                {(m.toolCalls ?? []).map((tc) => (
                  <div key={tc.id}>
                    <ToolPart
                      data={{
                        id: tc.id,
                        name: tc.nameUnavailable ? "" : tc.name,
                        state: tc.status === "running" ? "running" : "done",
                        replayClass: tc.replayClass ?? "",
                        // The ledger join's arguments, so opening the card shows what the call RAN
                        // rather than the id of the frame that announced it.
                        arguments: tc.detail?.joined === true ? (tc.detail.arguments ?? null) : null,
                      }}
                    />
                    {tc.detail ? <ToolDetailPart data={tc.detail} /> : null}
                  </div>
                ))}

                {(m.approvals ?? []).map((a) => (
                  <ApprovalPart key={String(a.id)} data={a} />
                ))}
                {(m.publications ?? []).map((p, i) => (
                  <PublicationPart key={`${String(p.id)}-${i}`} data={p} />
                ))}
                {(m.subagents ?? []).map((c) => (
                  <SubagentPart key={c.id} child={c} />
                ))}
                {(m.media ?? []).map((md) => (
                  <MediaPart key={md.artifactId} data={md} />
                ))}
                {(m.notices ?? []).map((n, i) => (
                  <NoticePart key={i} data={n} />
                ))}

                {m.content !== "" ? (
                  <div className="text-[13px] text-text-primary leading-relaxed" data-testid="chat-ai-text">
                    <MessageResponse>{m.content}</MessageResponse>
                  </div>
                ) : m.isStreaming ? (
                  // palcore's three pulsing dots, plus the word for what the run is doing.
                  <div className="flex items-center gap-1.5 text-[13px] text-text-secondary" data-testid="chat-thinking">
                    <span className="thinking-dot inline-block size-1.5 rounded-full bg-current" />
                    <span className="thinking-dot inline-block size-1.5 rounded-full bg-current" />
                    <span className="thinking-dot inline-block size-1.5 rounded-full bg-current" />
                    <span className="ml-1.5 opacity-70">{status === "thinking" ? "Thinking…" : "Working…"}</span>
                  </div>
                ) : null}

                {/* Error chip — sits BELOW the content and does not recolour it. Streaming errors often
                    arrive after a chunk of perfectly valid prose; painting that prose red made the whole
                    UI look broken. */}
                {/* PROSE THAT PERFORMS A TOOL CALL MUST NOT PASS AS ONE.
                    MEASURED on the live stack 2026-08-02: a turn with ZERO tool calls and ZERO ledger
                    rows answered with "**Tool Call:** … Status: Completed … Terminal: ```** BUILD
                    SUCCEEDED **```" and an invented simctl listing — one device UUID was literally
                    `…CC24CFXX-XXXX`. Rendered as markdown that is indistinguishable from a real build.
                    Every other defect on this screen made the demo look broken; this one makes it look
                    like it WORKED, which is the only kind an operator cannot catch.

                    THE SENTENCE IS UNCONDITIONALLY TRUE WHENEVER IT RENDERS, and that is the whole
                    design. It fires only when the turn recorded NO tool calls, so it can never
                    contradict a real one — the tool rows above come from the ledger and are evidence;
                    this is the absence of evidence, said out loud. The code-fence test only decides
                    whether the caption is WORTH showing, never whether it is true: a turn with no
                    fenced block is not claiming to have run anything, so the note would be noise. */}
                {!m.isStreaming && (m.toolCalls ?? []).length === 0 && m.content.includes("```") ? (
                  <p
                    data-testid="chat-unverified-output"
                    className="mt-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-[12px] text-amber-200 leading-4"
                  >
                    No tool call was recorded for this turn. Anything above that reads as a command or
                    its output was written by the model — it was not executed, and this screen has no
                    evidence that it ran.
                  </p>
                ) : null}

                {/* One quiet line when the turn settles, and nothing at all while it runs. */}
                {!m.isStreaming && m.runStatus ? (
                  <p data-testid="chat-run" className="mt-2 text-[11px] text-text-tertiary">
                    run {m.runStatus}
                  </p>
                ) : null}

                {m.isError && m.errorText ? (
                  <div
                    data-testid="chat-error"
                    className="mt-3 flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-300"
                  >
                    <span className="mt-0.5" aria-hidden>
                      ⚠
                    </span>
                    <span className="min-w-0 flex-1 break-words">{m.errorText}</span>
                  </div>
                ) : null}
              </div>
            ),
          )}
        </div>
      </div>

      {/* ── The input pill — palcore's, geometry and motion included ─────────────────── */}
      <div className="p-3">
        <form
          data-testid="chat-form"
          method="post"
          action="/api/chat"
          onSubmit={(e) => {
            e.preventDefault();
            void handleSend();
          }}
          className={`relative ${streaming ? "p-[2px]" : ""}`}
        >
          {/* The conic-gradient border that spins while the model works. */}
          {streaming ? (
            <div
              aria-hidden
              className="palai-gradient-spin absolute inset-0 rounded-full"
              style={{
                background:
                  "conic-gradient(from var(--gradient-angle, 0deg), #6366f1, #8b5cf6, #3b82f6, #a855f7, #6366f1)",
                mask: "linear-gradient(#fff 0 0) content-box, linear-gradient(#fff 0 0)",
                maskComposite: "exclude",
                WebkitMaskComposite: "xor",
                padding: "2px",
              }}
            />
          ) : null}

          <div className={`glass-elevated rounded-full px-4 py-2 ${streaming ? "relative z-10" : ""}`}>
            <div className="flex items-center gap-2.5">
              <Sparkles size={16} className={streaming ? "shrink-0 animate-pulse text-ai" : "shrink-0 text-ai"} aria-hidden />

              <textarea
                ref={inputRef}
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                data-testid="chat-input"
                aria-label="Message"
                rows={1}
                placeholder={
                  binding ? `Ask for a change in ${binding.repository_identity}…` : "Pick a repository to start…"
                }
                className="h-[20px] flex-1 resize-none bg-transparent text-[13px] text-text-primary outline-none placeholder:text-text-tertiary"
              />

              {streaming ? (
                <button
                  type="button"
                  onClick={handleCancel}
                  data-testid="chat-stop"
                  aria-label="Stop"
                  className="flex size-7 shrink-0 items-center justify-center rounded-full bg-red-500/20 text-red-400 transition-colors hover:bg-red-500/30"
                >
                  <Square size={10} fill="currentColor" aria-hidden />
                </button>
              ) : (
                <button
                  type="submit"
                  data-testid="chat-send"
                  aria-label="Send"
                  disabled={input.trim() === ""}
                  className={`flex size-7 shrink-0 items-center justify-center rounded-full transition-colors ${
                    input.trim() !== ""
                      ? "bg-ai/20 text-ai hover:bg-ai/30"
                      : "cursor-not-allowed text-text-disabled"
                  }`}
                >
                  <Send size={13} strokeWidth={2} aria-hidden />
                </button>
              )}
            </div>
          </div>
        </form>

        {/* WHICH AGENT IS RUNNING. The owner had to ask this, which means the screen was not saying
            it. An unresolved agent says so rather than showing nothing — "no agent" and "I did not
            look" are different sentences, and only one of them is a bug report. */}
        {agent !== null ? (
          <p className="mt-2 px-1 text-[11px] text-text-tertiary" data-testid="chat-agent">
            {agent.id !== null ? (
              <>
                running as agent <span className="text-text-secondary">{agent.name}</span>{" "}
                <span className="font-mono">{agent.id.slice(0, 14)}…</span>
              </>
            ) : (
              <span className="text-red-300">
                no agent pinned — this turn ran unsteered{agent.note !== "" ? `: ${agent.note}` : "."}
              </span>
            )}
          </p>
        ) : null}

        <p className="mt-1 px-1 text-[11px] text-text-tertiary" data-testid="chat-session">
          {sessionId ? `session ${sessionId.slice(0, 16)}…` : "no session yet"}
          {/* A PARTIAL TRANSCRIPT MUST NOT READ AS A COMPLETE ONE — main's sentences, kept verbatim in
              meaning. `drained: false` means the replay hit its deadline with the journal still going,
              and `truncated` means the oldest events were dropped to bound the page. Either way the
              reader is looking at less than happened, and saying so is why the route reports it. */}
          {resumed !== null && !resumed.drained ? (
            <span className="ml-2 text-amber-600" data-testid="history-partial">
              · history partial (still catching up)
            </span>
          ) : null}
          {resumed?.truncated === true ? (
            <span className="ml-2 text-amber-600" data-testid="history-truncated">
              · oldest messages trimmed
            </span>
          ) : null}
          {streaming ? (
            <span className="ml-2 inline-flex items-center gap-1">
              <Loader2 size={10} className="animate-spin" aria-hidden />
              {status === "thinking" ? "thinking" : "working"}
            </span>
          ) : null}
        </p>
      </div>
    </div>
  );
}
