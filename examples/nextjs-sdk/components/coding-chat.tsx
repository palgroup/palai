"use client";

import { useCallback, useState } from "react";

import { AIChat } from "@/components/ai-chat/AIChat";
import { type AutoApproveState, AutoApproveControls } from "@/components/auto-approve";
import { type Binding, RepositoryPicker } from "@/components/repository-picker";
import { TooltipProvider } from "@/components/ui/tooltip";
import { WorkspacePanel } from "@/components/workspace-panel";

// =============================================================================================
// THE DEMO.
//
// The owner's ask, verbatim: "ben chat ederek ai'a kod yazdırmak istiyorum repo seçeceğim demo
// ekrandan o repoda kod yazdırmak istiyorum" — chat, pick a repository ON THE SCREEN, and have the AI
// write code in that repository.
//
// So the layout puts the REPOSITORY in the middle and the largest share of the page, the picker in the
// rail on the left, and the chat on the right as its control surface. The screen's subject is the
// repository changing; the chat is how you drive it.
//
// THE CHAT ITSELF IS palcore's, COPIED. The owner's second directive was "şu ui tarafını palcore'un
// chat'i ile birebir aynı yap direkt onu al kopyala sonra palai sdk kullanacak şekilde güncelle", so
// components/ai-chat/ is palcore's apps/web/src/ai-chat/ with its data layer replaced. What that copy
// kept and what it deliberately did not is documented at the top of AIChat.tsx — the short version is
// that palcore hides tool calls in two independent ways, and this is the screen whose whole subject is
// tool calls.
//
// IT NO LONGER USES @ai-sdk/react's useChat. That hook was the previous answer to "can Palai be driven
// by the ecosystem", and the answer it gave was real but it is not what the owner asked for. The
// vendored AI Elements components are still what draws a tool call, an approval and a code block, so
// the ecosystem half of the claim survives where it does the work.
//
// NO KEY IN THE BROWSER. Every network call this component makes is to this app's own /api/* routes.
// The Palai credential is read server-side in lib/palai.ts and lib/raw.ts, both guarded by
// `server-only`, which makes importing them from a Client Component a build error.
//
// MERGED WITH main 2026-08-02, AND THIS FILE IS WHERE THE TWO DESIGNS MET. main's version is the
// `useChat` host this branch replaced, so a conflict "resolved" by taking either side whole would have
// deleted a working chat or three shipped features. What was carried across instead:
//   * SESSION RESUME (?session=<id>) — now owned by AIChat, which owns the message list here. Passing
//     it down rather than reimplementing it keeps main's route, its lib/history.ts and its two
//     partial-transcript warnings exactly as they were.
//   * SUBAGENTS and SHOW_MEDIA — rendered inside AIChat's turn, from main's own components.
// =============================================================================================

export function CodingChat() {
  const [binding, setBinding] = useState<Binding | null>(null);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [responseId, setResponseId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // The standing authorization as THE SERVER records it. It starts off for both halves, which is the
  // state every session is born in, and only ever moves to what a PATCH came back saying.
  const [autoApprove, setAutoApprove] = useState<AutoApproveState>({ tools: false, publications: false, setBy: "" });

  // These three are handed to the chat as callbacks. They are `useCallback`ed because AIChat reports
  // its busy state from an effect, and an unstable callback there would loop.
  const handleSession = useCallback((id: string) => setSessionId(id), []);
  const handleResponse = useCallback((id: string) => setResponseId(id), []);
  const handleBusy = useCallback((value: boolean) => setBusy(value), []);

  return (
    <TooltipProvider>
      <div className="flex h-dvh flex-col bg-background text-foreground xl:flex-row">
        {/* ------------------------------------------------------------------------ the rail */}
        {/* MEASURED (design-reference §1): 256px, and rgb(13,13,13) — DARKER than the page, which is
            the reverse of what this tree assumed before it measured. */}
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

          <AutoApproveControls
            sessionId={sessionId}
            state={autoApprove}
            onChange={setAutoApprove}
            disabled={busy}
          />

          <AIChat
            binding={binding}
            onSession={handleSession}
            onResponse={handleResponse}
            onBusyChange={handleBusy}
          />
        </section>
      </div>
    </TooltipProvider>
  );
}
