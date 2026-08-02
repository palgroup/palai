import type { UIMessage } from "ai";

// REPLAYING A SESSION INTO THE CHAT: turning what the journal recorded back into what a reader saw.
//
// A session's transcript is not stored as messages anywhere — it is stored as EVENTS, which is the
// stronger record (every tool call, every state transition, ordered and resumable) and the wrong shape
// for a chat window. This module is the projection between them, and it exists so a reload shows the
// conversation rather than an empty box with a session id in the corner.
//
// ‼️ ONE MESSAGE PER MODEL STEP, NOT ONE PER DELTA. A model step's text arrives as many coalesced
// `model_step.delta.v1` events — the control plane journals a window at a time — and rendering each as
// its own bubble would turn one answer into five. The step's created/completed events are the
// boundaries, so the deltas between them concatenate into one assistant message, which is what the live
// stream also produces.

export interface HistoryEvent {
  type: string;
  sequence?: number;
  text?: string;
  tool?: { id?: string; name?: string; arguments?: unknown; result?: unknown };
}

export interface HistoryPayload {
  sessionId: string;
  events: HistoryEvent[];
  lastEventId: string | null;
  truncated: boolean;
  drained: boolean;
}

// toUIMessages turns a replayed journal into the messages a chat renders.
//
// IT IS DELIBERATELY LOSSY AND SAYS SO. The journal carries far more than a chat can show — attempt
// transitions, workspace states, usage. Those are not dropped because they do not matter; they are
// dropped because this is the CHAT view, and the same events feed the workspace panel beside it. A
// projection that tried to render everything would be a log viewer wearing a chat's clothes.
export function toUIMessages(events: HistoryEvent[]): UIMessage[] {
  const messages: UIMessage[] = [];
  let pending: string[] = [];
  let step = 0;

  const flush = () => {
    const text = pending.join("");
    pending = [];
    if (text.trim() === "") return; // a step that produced only tool calls has no bubble to show
    messages.push({
      id: `history-step-${step++}`,
      role: "assistant",
      parts: [{ type: "text", text }],
    } as UIMessage);
  };

  for (const event of events) {
    switch (event.type) {
      case "model_step.delta.v1":
        if (typeof event.text === "string") pending.push(event.text);
        break;
      case "model_step.completed.v1":
      case "model_step.interrupted.v1":
      case "model_step.failed.v1":
        // THE STEP'S END IS THE MESSAGE BOUNDARY. Flushing on the next step's CREATED event instead
        // would lose the last message of a session that ended cleanly — there is no step after it.
        flush();
        break;
      default:
        break;
    }
  }
  flush();
  return messages;
}

// fetchHistory reads a session's past from the server relay. A failure is not thrown: a page that could
// not load history should still open a working chat on that session, because the alternative — a blank
// error screen — loses the user the thing they came back for.
export async function fetchHistory(sessionID: string): Promise<HistoryPayload | null> {
  try {
    const res = await fetch(`/api/palai/history?sessionId=${encodeURIComponent(sessionID)}`);
    if (!res.ok) return null;
    return (await res.json()) as HistoryPayload;
  } catch {
    return null;
  }
}
