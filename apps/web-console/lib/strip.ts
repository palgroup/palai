// THE LANE STRIP'S ARITHMETIC, with no React in it (E30 visual identity).
//
// The strip is this console's signature and it appears at three scales, so the placement of a mark is
// computed in exactly one place and driven by tests/unit.spec.ts with no browser. A percentage derived
// inline in JSX is a percentage nothing can check, and every defect this file guards against is a
// percentage: a NaN from an unparseable stamp, a divide-by-zero on a window with no width, a bar that runs
// off the end of its own track.
//
// WHAT IS NOT HERE IS THE POINT, again. There is no synthetic event, no interpolation between frames and no
// "current time" mark on a finished journal. Every tick below is one row the API served, at the timestamp
// that row carries.

import { LANE_LABEL } from "./sessions";
import { type Lane } from "./timeline";

/**
 * LANE_ORDER is the vertical order of the stave's channels, and it is a NARRATIVE rather than the alphabet.
 *
 * Reading down: the machine's own state, then what arrived, then the model thinking, then the model reaching
 * out, then a human deciding, then a repair, then what it cost, then how it ended. A run that goes well
 * draws a diagonal down this stave; a run that parks on an approval leaves a gap in the middle of it. That
 * is a shape an operator learns in one sitting and cannot learn from a list sorted by sequence number.
 *
 * It is exhaustive over `Lane` by construction — the type below fails to compile if a lane is added to
 * lib/timeline.ts and not placed here, which is the half of "a new lane must be given a channel" that a
 * comment cannot enforce.
 */
export const LANE_ORDER: readonly Lane[] = [
  "progress",
  "message",
  "model_step",
  "tool",
  "approval",
  "recovery",
  "usage",
  "terminal",
] as const;

/** A window in wall-clock milliseconds. `end` may equal `start`; positionAt is what handles that. */
export interface Window {
  start: number;
  end: number;
}

/** One mark on a track: where it starts, optionally how wide it is, and the two facts that change its shape. */
export interface Mark {
  key: string;
  /** 0-100, clamped. The mark's left edge as a percentage of the window. */
  at: number;
  /** 0-100, present only on a SPAN mark (the list scale's bar). Absent means a tick. */
  width?: number;
  failure?: boolean;
  latest?: boolean;
}

/** One channel of a stave: the attribute the stylesheet keys on, the word in the gutter, and its marks. */
export interface Channel {
  /** The `data-lane` value. For a journal stave this is a `Lane`; for the overview it is a lane REUSED. */
  lane: string;
  label: string;
  marks: Mark[];
  /** Set when every mark in the channel is a failure — the overview's "ended badly" row. */
  failure?: boolean;
}

/**
 * positionAt is a moment's place in a window as a 0-100 percentage.
 *
 * A ZERO-WIDTH WINDOW IS 0, NOT NaN — the same rule lib/sessions.ts's positionOf already keeps, and for the
 * same measured reason: a replayed fixture and a fast failure both produce a journal whose frames share one
 * timestamp, and `left: NaN%` puts every mark nowhere.
 */
export function positionAt(ms: number, window: Window): number {
  if (!Number.isFinite(ms) || window.end <= window.start) return 0;
  return Math.min(100, Math.max(0, ((ms - window.start) / (window.end - window.start)) * 100));
}

/** windowOf is the enclosing window of a set of stamps, or null when none of them parses. */
export function windowOf(isos: (string | null | undefined)[]): Window | null {
  const times = isos.map((v) => Date.parse(String(v ?? ""))).filter((t) => !Number.isNaN(t));
  if (times.length === 0) return null;
  return { start: Math.min(...times), end: Math.max(...times) };
}

/** A journal frame, reduced to what a strip needs. Kept structural so both session screens can pass theirs. */
export interface Frame {
  key: string;
  lane: Lane;
  time: string | null | undefined;
  failure: boolean;
}

/**
 * staveOf groups frames into channels, in LANE_ORDER, DROPPING THE LANES THIS JOURNAL DID NOT USE.
 *
 * The empty channels are dropped rather than drawn greyed, and that is a decision about what a reader
 * concludes from an empty row: a track with no marks on it says "this lane produced nothing", which is true
 * of six of the eight lanes on almost every run and is therefore six rows of noise saying nothing. A run
 * that DID use a lane is the interesting fact, so the stave's own height becomes a reading — a three-channel
 * run and a seven-channel run look different before a single mark is examined.
 */
export function staveOf(frames: Frame[], window: Window | null): Channel[] {
  if (window === null) return [];
  const out: Channel[] = [];
  for (const lane of LANE_ORDER) {
    const mine = frames.filter((f) => f.lane === lane);
    if (mine.length === 0) continue;
    out.push({
      lane,
      label: LANE_LABEL[lane],
      marks: mine.map((f) => ({
        key: f.key,
        at: positionAt(Date.parse(String(f.time ?? "")), window),
        failure: f.failure ? true : undefined,
      })),
    });
  }
  return out;
}

/**
 * spanMark is a session's activity bar inside the window the whole LIST covers.
 *
 * A SESSION THAT NEVER RAN HAS NO BAR AND RETURNS null, because `first_activity_at` is absent on exactly
 * those rows (lib/sessions.ts records the measurement) and a bar of width zero placed at the window's start
 * would claim it ran for an instant when it opened. The caller renders the em dash the rest of this console
 * uses for an absent value.
 *
 * The bar's width is allowed to be 0: a session whose whole life was inside one millisecond is a real thing,
 * and the stylesheet gives every bar a 3px floor so it is still visible as a POSITION when it is no longer
 * legible as a duration.
 */
export function spanMark(first: string | null | undefined, last: string | null | undefined, window: Window | null): Mark | null {
  if (window === null) return null;
  const a = Date.parse(String(first ?? ""));
  if (Number.isNaN(a)) return null;
  const b = Date.parse(String(last ?? ""));
  const at = positionAt(a, window);
  const to = positionAt(Number.isNaN(b) ? a : b, window);
  return { key: "span", at, width: Math.max(0, to - at) };
}
