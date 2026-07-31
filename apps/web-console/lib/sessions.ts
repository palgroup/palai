// The SESSION row and the pure functions the two session screens render it with (E29).
//
// It is a module rather than a handful of helpers inside the pages because every value on those screens is a
// DERIVATION — a duration from two timestamps, a relative stamp from a clock, a compact token figure, an
// elapsed offset from the journal's first frame — and a derivation written inline in JSX is a derivation no
// test can reach. tests/unit.spec.ts drives every function below with no browser.
//
// WHAT IS NOT HERE IS THE POINT. There is no `role`, no `speaker`, no `message` and no `content`: the shape
// below is exactly what GET /v1/sessions answers (protocols/schemas/execution/session.json) and what the
// journal frames carry, and a field the API does not send is a field these screens do not render.

import { type Lane } from "./timeline";

/** Where `name` came from. `derived` is a cut of the first prompt and is NOT something anyone chose. */
export type NameSource = "operator" | "derived" | "none";

/**
 * SessionRow is the /v1 session projection, field for field.
 *
 * `agents` IS PLURAL AND THAT IS LOAD-BEARING (session.json's own words): there is no session-to-agent
 * association in this system — a RUN pins an agent revision — so a session may have used none, one, or
 * several, and a column headed "Agent" singular asserts something the data does not say.
 *
 * The token counts are a FLOOR, not an invoice: a model step aborted mid-stream by an interrupt never reaches
 * the usage ledger, because the provider's counts ride a final chunk a canceled stream never receives.
 */
export interface SessionRow extends Record<string, unknown> {
  id: string;
  status: string;
  created_at: string;
  name: string;
  name_source: NameSource;
  agents: string[];
  input_tokens: number;
  output_tokens: number;
  /**
   * The span, and all three are OPTIONAL rather than nullable — measured against the running control plane on
   * 2026-07-31 rather than read off the schema.
   *
   * A session that has never run answers ELEVEN keys and these three are ABSENT, because the Go projection
   * carries them omitempty; a session that HAS run carries all three with values. `string | null` alone would
   * be a type saying "this key is always here", which is how a console comes to render `undefined`. The
   * functions below take `null | undefined` for exactly this reason and both answer the em dash.
   */
  first_activity_at?: string | null;
  last_activity_at?: string | null;
  duration_ms?: number | null;
  organization_id?: string;
  project_id?: string;
}

/** A journal frame as the SSE stream serves it, with the fields these screens actually read. */
export interface JournalEvent extends Record<string, unknown> {
  id?: string;
  type: string;
  sequence: number;
  time?: string;
  data?: Record<string, unknown>;
}

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

/**
 * relativeTime renders a timestamp the way the eye reads a list: "10 hours ago", never an ISO string.
 *
 * `now` is a PARAMETER rather than a call to the clock inside, which is what makes it testable at all — and
 * it is also why nothing on these screens renders during server-side render: both pages fetch from the
 * browser, so a relative stamp is only ever computed on the client and cannot produce a hydration mismatch.
 *
 * An unparseable or empty timestamp returns the em dash rather than "Invalid Date". The API sends
 * `first_activity_at: null` for a session that never ran, and a screen that printed NaN for it would be
 * reporting a rendering fault as a fact about the record.
 */
export function relativeTime(iso: string | null | undefined, now: number = Date.now()): string {
  if (iso === null || iso === undefined || iso === "") return "—";
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "—";
  const deltaMs = then - now;
  const abs = Math.abs(deltaMs);
  if (abs < 45_000) return "just now";
  if (abs < 3_600_000) return relative.format(Math.round(deltaMs / 60_000), "minute");
  if (abs < 86_400_000) return relative.format(Math.round(deltaMs / 3_600_000), "hour");
  if (abs < 2_592_000_000) return relative.format(Math.round(deltaMs / 86_400_000), "day");
  return relative.format(Math.round(deltaMs / 2_592_000_000), "month");
}

/** absoluteTime is the full stamp a relative one hides — the `title`/`dateTime` behind every "10 hours ago". */
export function absoluteTime(iso: string | null | undefined): string {
  if (iso === null || iso === undefined || iso === "") return "";
  const t = Date.parse(iso);
  return Number.isNaN(t) ? "" : new Date(t).toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}

/**
 * compactTokens renders a metered count the way the reference does: 22500 -> "22.5k", 111 -> "111".
 *
 * The cut is at a THOUSAND and not lower, because the numbers this console shows are frequently two and three
 * digits (a fake-provider run meters 44 in and 161 out) and "0.0k" is not a smaller way of writing 44.
 */
export function compactTokens(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${trimZero(n / 1000)}k`;
  return `${trimZero(n / 1_000_000)}M`;
}

function trimZero(v: number): string {
  return v.toFixed(1).replace(/\.0$/, "");
}

/**
 * formatDuration renders a span in the largest unit that still says something: "19.9s", "2m 04s", "1h 12m".
 *
 * NULL IS "—" AND IT MEANS SOMETHING DIFFERENT FROM ZERO. session.json: duration_ms is null when the session
 * has never run — "a session with no duration, not one lasting zero" — and a screen that rendered 0ms there
 * would be claiming a run happened instantly.
 */
export function formatDuration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${String(Math.round(ms))}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms < 3_600_000) {
    const m = Math.floor(ms / 60_000);
    return `${String(m)}m ${String(Math.floor((ms % 60_000) / 1000)).padStart(2, "0")}s`;
  }
  const h = Math.floor(ms / 3_600_000);
  return `${String(h)}h ${String(Math.floor((ms % 3_600_000) / 60_000)).padStart(2, "0")}m`;
}

/** elapsedStamp is the transcript's per-row offset from the journal's first frame: "0:00:04". */
export function elapsedStamp(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return `${String(h)}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

/**
 * LANE_LABEL is the word a transcript row wears, and it is the §47.2 LANE rather than a speaker.
 *
 * THE REFERENCE SCREEN WANTS "User / Tool / Agent" AND THIS DEPLOYMENT HAS NO SUCH THING. A palai session's
 * journal is a RUN LIFECYCLE — run.queued, run.provisioning, run.running, model_step.created,
 * model_step.completed, run.completed — with no author, no addressee and no message body. lib/timeline's
 * laneFor already sorts every canonical type into the eight lanes the system actually has; this names them
 * for a reader and adds nothing. Inventing a role column would mean deciding, in the console, who "said" a
 * `run.provisioning.v1` — which is a claim no code anywhere makes.
 */
export const LANE_LABEL: Record<Lane, string> = {
  message: "Message",
  model_step: "Model step",
  tool: "Tool",
  approval: "Approval",
  recovery: "Recovery",
  usage: "Usage",
  progress: "Lifecycle",
  terminal: "Outcome",
};

/**
 * isFailureEvent is the error pill's predicate — the frames that record a run ending badly or a decision
 * refusing. It is deliberately NOT "anything terminal": run.completed.v1 is terminal and is the good outcome.
 */
export function isFailureEvent(type: string): boolean {
  return (
    /^(?:run|response)\.(?:failed|canceled|timed_out|budget_exceeded)\.v1$/.test(type) ||
    type === "approval.denied.v1" ||
    type === "approval.expired.v1" ||
    type === "host.quarantined.v1" ||
    type === "workspace.host_lost.v1"
  );
}

/**
 * eventSubject is the one identifier a journal frame carries that says WHICH thing it is about.
 *
 * THIS IS WHAT THE REFERENCE CALLS "the content preview", AND IT IS NOT ONE. Measured against the live stack
 * on 2026-07-31: `run.*` frames carry `{run_id, state}` and `model_step.*` frames carry
 * `{model_request_id, run_id}` — no text, no arguments, no result, nothing to preview. So the column shows
 * the identifier the frame actually holds, in monospace, and is headed for what it is.
 *
 * The order is most-specific first: a model step's own request id says more than the run it belongs to.
 */
export function eventSubject(data: Record<string, unknown> | undefined): string {
  const d = data ?? {};
  for (const key of ["model_request_id", "tool_call_id", "publication_id", "checkpoint_id", "artifact_id", "child_id", "run_id"]) {
    const value = d[key];
    if (typeof value === "string" && value !== "") return value;
  }
  return "";
}

/** eventState is the lifecycle word a run.* frame carries in `data.state`, or "" for a frame with none. */
export function eventState(data: Record<string, unknown> | undefined): string {
  const value = (data ?? {}).state;
  return typeof value === "string" ? value : "";
}

/**
 * spanOf reports the journal's own first and last frame times, in milliseconds.
 *
 * IT IS READ OFF THE FRAMES AND NOT OFF THE SESSION ROW, and the difference is measurable: `duration_ms` on
 * the session spans its RUNS (created_at of the earliest to the last state change of the latest), while the
 * scrubber places EVENTS. On a session with one run they are within a few milliseconds; on a session with
 * two they are not, and a scrubber scaled to the wrong span puts marks outside its own track.
 */
export function spanOf(events: JournalEvent[]): { start: number; end: number } | null {
  const times = events.map((e) => Date.parse(String(e.time ?? ""))).filter((t) => !Number.isNaN(t));
  if (times.length === 0) return null;
  return { start: Math.min(...times), end: Math.max(...times) };
}

/**
 * positionOf is a frame's place on the scrubber as a 0-100 percentage of the journal's span.
 *
 * A ZERO-LENGTH SPAN IS 0, NOT NaN. Every frame of a session whose events all share one timestamp — which is
 * what a replayed fixture and a fast failure both produce — would otherwise divide by zero and place every
 * mark at `left: NaN%`, i.e. nowhere.
 */
export function positionOf(time: string | undefined, span: { start: number; end: number } | null): number {
  if (span === null) return 0;
  const t = Date.parse(String(time ?? ""));
  if (Number.isNaN(t) || span.end <= span.start) return 0;
  return Math.min(100, Math.max(0, ((t - span.start) / (span.end - span.start)) * 100));
}
