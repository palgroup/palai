"use client";

import { useChat } from "@ai-sdk/react";
import { DefaultChatTransport } from "ai";
import { useRef, useState } from "react";

import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from "@/components/ai-elements/conversation";
import { Message, MessageContent, MessageResponse } from "@/components/ai-elements/message";
import {
  PromptInput,
  PromptInputBody,
  PromptInputFooter,
  PromptInputSubmit,
  PromptInputTextarea,
  PromptInputTools,
} from "@/components/ai-elements/prompt-input";
import { ApprovalPart, NoticePart, PublicationPart, ToolPart } from "@/components/chat-parts";
import { type Binding, RepositoryPicker } from "@/components/repository-picker";
import { TooltipProvider } from "@/components/ui/tooltip";
import { WorkspacePanel } from "@/components/workspace-panel";

// =============================================================================================
// THE DEMO.
//
// The owner's ask, verbatim: "ben chat ederek ai'a kod yazdırmak istiyorum repo seçeceğim demo
// ekrandan o repoda kod yazdırmak istiyorum" — chat, pick a repository ON THE SCREEN, and have the
// AI write code in that repository.
//
// So the layout puts the REPOSITORY in the middle and the largest share of the page, the picker in
// the rail on the left, and the chat on the right as its control surface. The screen's subject is
// the repository changing; the chat is how you drive it.
//
// The chat itself is @ai-sdk/react's useChat, unmodified — the same hook a Vercel AI SDK app uses
// against an OpenAI route — and every visible component is Vercel's AI Elements, vendored. Only the
// adapter at /api/chat is ours. That is a much stronger answer to "can Palai be driven by the
// ecosystem" than a hand-built chat would be, and it is also what makes the gaps meaningful: where
// an AI Elements component cannot be filled, the gap is in Palai's stream, not in our rendering.
//
// NO KEY IN THE BROWSER. Every network call this component makes is to this app's own /api/* routes.
// The Palai credential is read server-side in lib/palai.ts and lib/raw.ts, both guarded by
// `server-only`, which makes importing them from a Client Component a build error.
// =============================================================================================

// MEASURED, AND THE REASON THIS STRING EXISTS. `palai.workspace.shell` runs with cwd = the
// ALLOCATION ROOT, not the clone: adapters/sandboxes/host/exec.go:152 sets c.Dir =
// cmd.WorkspaceRoot, and the clone is one level down at <root>/repo. The tool's JSON schema has no
// cwd/workdir field, AND it does not set additionalProperties:false — so a `cwd` argument invented
// by a hopeful caller is accepted and silently ignored (packages/tool-broker/conformance_math.go:50).
//
// Driven live 2026-08-02 without this hint, gpt-4o-mini ran `git status`, got "not a git
// repository", ran `git init` AT THE ROOT, and committed into a brand-new empty repository beside
// the clone. The clone stayed untouched at its real HEAD. Nothing errored. A push from that state
// would have proposed a write nobody meant.
//
// The only correct affordance is to tell the model where the repository is, so the demo does.
const REPO_HINT =
  "The repository is cloned at ./repo, and shell commands start in the workspace root ABOVE it — " +
  "always use `git -C repo …` or paths beginning with `repo/`. Do not run `git init`.";

export function CodingChat() {
  const [binding, setBinding] = useState<Binding | null>(null);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [responseId, setResponseId] = useState<string | null>(null);

  // The transport's body callback must read the current session and binding WITHOUT re-creating the
  // transport on every turn, so both ride refs alongside the state that renders them.
  const sessionRef = useRef<string | null>(null);
  const bindingRef = useRef<string>("");
  bindingRef.current = binding?.id ?? "";

  const { messages, sendMessage, status, error, stop } = useChat({
    transport: new DefaultChatTransport({
      api: "/api/chat",
      body: () => ({
        sessionId: sessionRef.current,
        ...(bindingRef.current !== "" ? { bindingId: bindingRef.current } : {}),
      }),
    }),
    onData: (part) => {
      if (part.type === "data-session") {
        const id = (part.data as { sessionId?: string })?.sessionId;
        if (typeof id === "string") {
          sessionRef.current = id;
          setSessionId(id);
        }
      }
      if (part.type === "data-run") {
        const id = (part.data as { responseId?: string })?.responseId;
        if (typeof id === "string") setResponseId(id);
      }
    },
  });

  const busy = status === "streaming" || status === "submitted";

  return (
    <TooltipProvider>
      <div className="flex h-dvh flex-col bg-background text-foreground xl:flex-row">
        {/* ------------------------------------------------------------------------ the rail */}
        {/* MEASURED (design-reference §1): 256px, and rgb(13,13,13) — DARKER than the page, which
            is the reverse of what this tree assumed before it measured. */}
        <aside className="w-full shrink-0 border-border border-b bg-rail xl:h-full xl:w-64 xl:border-r xl:border-b-0">
          <RepositoryPicker selectedId={binding?.id ?? ""} onSelect={setBinding} disabled={busy} />
        </aside>

        {/* ------------------------------------------------------- the subject: the repository */}
        <main className="order-last min-h-0 min-w-0 flex-1 xl:order-none">
          <WorkspacePanel binding={binding} responseId={responseId} running={busy} sessionId={sessionId} />
        </main>

        {/* ------------------------------------------------- the control surface: the chat */}
        <section
          className="flex min-h-0 w-full shrink-0 flex-col border-border border-t xl:h-full xl:w-[420px] xl:border-t-0 xl:border-l"
          aria-label="Chat"
        >
          <header className="border-border border-b px-4 py-3">
            <h2 className="font-medium text-[15px]">Coding session</h2>
            <p data-testid="chat-key-note" className="mt-0.5 text-[12px] text-muted-foreground leading-4">
              No key in this page — the browser talks only to <code>/api/*</code>, and the Palai
              credential is server-side.
            </p>
          </header>

          <Conversation className="min-h-0 flex-1">
            <ConversationContent className="gap-6 p-4">
              {messages.length === 0 ? (
                <ConversationEmptyState
                  title={binding ? `Ready on ${binding.repository_identity}` : "Pick a repository first"}
                  description={
                    binding
                      ? "Ask for a change. The clone, the shell and any publication happen inside that repository."
                      : "Choose one in the rail — that is what gives the session a clone to work in."
                  }
                />
              ) : null}

              {messages.map((m) => (
                <Message from={m.role} key={m.id} data-testid={`chat-message-${m.role}`}>
                  <MessageContent>
                    {m.parts.map((part, i) => (
                      <Part key={i} part={part as UIPart} />
                    ))}
                  </MessageContent>
                </Message>
              ))}
            </ConversationContent>
            <ConversationScrollButton />
          </Conversation>

          {error ? (
            <p data-testid="chat-error" className="mx-4 mb-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-[13px] text-destructive">
              {error.message}
            </p>
          ) : null}

          <div className="p-3">
            <PromptInput
              data-testid="chat-form"
              onSubmit={(message) => {
                const text = message.text.trim();
                if (text === "" || busy) return;
                // The repository hint rides with the FIRST turn of a session only. Palai holds the
                // conversation server-side and replays it, so repeating the hint every turn would
                // spend context re-teaching something already in the transcript.
                const withHint = messages.length === 0 && binding !== null ? `${text}\n\n(${REPO_HINT})` : text;
                sendMessage({ text: withHint });
              }}
            >
              <PromptInputBody>
                <PromptInputTextarea
                  data-testid="chat-input"
                  placeholder={binding ? `Ask for a change in ${binding.repository_identity}…` : "Pick a repository to start…"}
                  aria-label="Message"
                />
              </PromptInputBody>
              <PromptInputFooter>
                <PromptInputTools>
                  <span className="px-1 text-[12px] text-muted-foreground" data-testid="chat-session">
                    {sessionId ? `session ${sessionId.slice(0, 16)}…` : "no session yet"}
                  </span>
                </PromptInputTools>
                <PromptInputSubmit status={status} onStop={stop} data-testid="chat-send" />
              </PromptInputFooter>
            </PromptInput>
          </div>
        </section>
      </div>
    </TooltipProvider>
  );
}

type UIPart = { type: string; text?: string; data?: Record<string, unknown>; id?: string };

// Part renders ONE UI-message-stream part. The `text` arm is the ecosystem's own and needs no
// translation at all, which is itself part of the answer; the `data-*` arms are the Palai half.
function Part({ part }: { part: UIPart }) {
  if (part.type === "text") {
    return <MessageResponse>{part.text ?? ""}</MessageResponse>;
  }
  const d = part.data ?? {};
  switch (part.type) {
    case "data-tool":
      return <ToolPart data={d} />;
    case "data-approval":
      return <ApprovalPart data={d} />;
    case "data-publication":
      return <PublicationPart data={d} />;
    case "data-notice":
      return <NoticePart data={d} />;
    case "data-run":
      return (
        <p data-testid="chat-run" className="text-[12px] text-muted-foreground">
          run {String(d.status ?? "")}
        </p>
      );
    default:
      return null;
  }
}
