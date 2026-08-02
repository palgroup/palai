import { type Event } from "@palai/sdk";

import { rawBaseURL, rawHeaders } from "@/lib/raw";
import { projectEvent } from "../route";

// SESSION HISTORY: everything that happened before this browser opened the page.
//
// WHY IT IS A SEPARATE ROUTE FROM THE STREAM. `GET /v1/sessions/{id}/events` is a resumable SSE
// endpoint: it replays the journal from a cursor and then TAILS it, so a request that wanted only the
// past would sit there holding a connection open forever waiting for a future that has not happened.
// The browser wants two different things — "catch me up" and "keep me posted" — and they are two
// requests, with the cursor of the last replayed event handing off from one to the other.
//
// ‼️ IT USES THE SAME projectEvent AS THE LIVE STREAM, imported rather than copied. A second projection
// would be a second definition of what an event LOOKS like, and the first time they disagreed the same
// event would render one way when it arrived live and another when the page was reloaded — which is
// exactly the kind of difference nobody reproduces on purpose.

// historyDeadlineMs bounds the replay, and it is DELIBERATELY SHORTER THAN THE SERVER'S HEARTBEAT.
//
// The endpoint sends `: heartbeat` after 15s idle (api/events.go SSEConfig.Heartbeat), so on a session
// that is still LIVE — no terminal event to close the stream — the drained signal is 15 seconds away and
// a page load must not wait for it. This deadline fires first, on purpose.
//
// ‼️ AND FIRING IS NOT A FAILURE. The events read before it ARE the history; returning 504 and throwing
// them away would make every live session's page load empty, which is the case a coding demo is in most
// of the time. So the deadline ends the READ and the route answers with what it has, saying whether the
// journal was actually drained.
const historyDeadlineMs = 4_000;

// maxHistoryEvents bounds what is sent to the browser. A long coding session journals thousands of
// events; a page that rendered all of them would spend its first seconds laying out a transcript nobody
// scrolled to. The NEWEST are what a reader wants, so an over-long history is truncated from the FRONT
// and says so, rather than silently dropping the recent end.
const maxHistoryEvents = 500;

export async function GET(request: Request): Promise<Response> {
  const sessionID = new URL(request.url).searchParams.get("sessionId")?.trim();
  if (!sessionID) {
    return Response.json({ error: "sessionId is required" }, { status: 400 });
  }

  const events: Record<string, unknown>[] = [];
  let lastEventID: string | null = null;
  let truncated = false;
  let drained = false;

  const controller = new AbortController();
  const deadline = setTimeout(() => controller.abort(), historyDeadlineMs);
  try {
    const upstream = await fetch(`${rawBaseURL()}/v1/sessions/${encodeURIComponent(sessionID)}/events`, {
      headers: { ...rawHeaders(), Accept: "text/event-stream" },
      signal: controller.signal,
      cache: "no-store",
    });
    if (!upstream.ok || !upstream.body) {
      return Response.json(
        { error: "session events unavailable", status: upstream.status },
        { status: upstream.status === 404 ? 404 : 502 },
      );
    }

    const reader = upstream.body.pipeThrough(new TextDecoderStream()).getReader();
    let buffer = "";
    read: while (true) {
      const { done, value } = await reader.read();
      // A terminal event closes the stream server-side, so `done` is the other drained signal — and the
      // common one, because a finished session never idles long enough to send a heartbeat.
      if (done) {
        drained = true;
        break;
      }
      buffer += value;
      // SSE frames are separated by a blank line. Anything without one is a partial frame and stays in
      // the buffer — splitting on newline alone would cut a JSON payload in half on a chunk boundary.
      const frames = buffer.split("\n\n");
      buffer = frames.pop() ?? "";
      for (const frame of frames) {
        // A HEARTBEAT IS THE DRAINED SIGNAL, AND ONE IS ENOUGH. The server writes `: heartbeat` only
        // when it has gone a full interval with nothing to send (api/events.go), so its arrival IS the
        // statement that the journal is caught up. Waiting for a second would cost another whole
        // interval for no more information — and on a session whose interval exceeds this route's
        // deadline, would turn every page load into a timeout.
        //
        // A terminal event closes the stream instead, so a finished session never reaches this at all:
        // it ends at `done`.
        if (!frame.trim()) continue;
        if (frame.startsWith(":")) {
          drained = true;
          break read;
        }
        const id = frame.match(/^id:\s*(.+)$/m)?.[1]?.trim();
        if (id) lastEventID = id;
        const payload = frame.match(/^data:\s*(.+)$/m)?.[1];
        if (!payload) continue;
        try {
          events.push(projectEvent(JSON.parse(payload) as Event));
        } catch {
          // A frame this app cannot parse is skipped rather than failing the whole replay: one bad
          // event must not cost a reader the other four hundred.
        }
        if (events.length > maxHistoryEvents) {
          events.shift();
          truncated = true;
        }
      }
    }
    await reader.cancel().catch(() => {});

    return Response.json({
      sessionId: sessionID,
      events,
      // WHETHER THE PAST IS ALL HERE. False means the deadline ended the read, not that the session has
      // no more history — a caller that treats the two the same will render a partial transcript as a
      // complete one, which is the failure this field exists to make impossible to miss.
      drained,
      // THE CURSOR IS THE HANDOFF. The browser opens the live stream with this as Last-Event-ID, so the
      // two requests join with no gap and no duplicate — without it a reload either loses whatever
      // happened between the two calls or renders it twice.
      lastEventId: lastEventID,
      truncated,
    });
  } catch (err) {
    if (controller.signal.aborted) {
      // The deadline, which is the EXPECTED end for a live session. What was read is real history and is
      // returned as such; `drained: false` is how the caller knows there may be more behind it.
      return Response.json({ sessionId: sessionID, events, lastEventId: lastEventID, truncated, drained: false });
    }
    return Response.json({ error: "history failed", detail: String(err) }, { status: 502 });
  } finally {
    clearTimeout(deadline);
  }
}
