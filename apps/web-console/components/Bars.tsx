"use client";

import type { Tally } from "@/components/Stat";

// A CHART, DRAWN HERE, WITH NO LIBRARY (E31 shell parity).
//
// "muz gibiii uzun sayfalar var aq, ne grafik var ne başka bir şey." The overview was six stacked tables and
// a strip of numbers, and a distribution — which of twenty runs failed, which of six machines is cordoned —
// was a sentence an operator had to parse rather than a shape they could see.
//
// IT IS INLINE SVG AND THE BUNDLE COST IS ZERO, which is the brief's own condition ("no chart library may be
// added without checking bundle impact; inline SVG is fine and probably better"). A bar chart of a
// categorical breakdown needs a scale, a rectangle and a label; every charting library in this size class
// ships an axis engine, a tooltip layer and a colour ramp for the cases this console does not have.
//
// WHAT IT IS NOT, AND THIS IS THE HONEST HALF: it is NOT a time series. A run-volume-over-time chart is the
// one an operator actually wants and it needs `GET /v1/usage/series` (bucket=hour|day plus the
// created_after/created_before window, apps/control-plane/api/router.go:296). That route EXISTS on the
// control plane and tests/fake-control-plane.mjs does not serve it — measured 2026-08-01,
// `grep -n '"/v1/usage' tests/fake-control-plane.mjs` → 2 hits, `/v1/usage` and `/v1/usage/ledger`, no
// series — so a time chart here would render an error on the profile the whole suite runs on. The fixture's
// ledger is two rows at one identical timestamp, which would draw one bar and call it a trend. That work
// belongs with whoever teaches the fake to serve the series; this file charts the distributions that ARE
// non-degenerate on both profiles.
//
// EVERY BAR IS LABELLED IN TEXT AND CARRIES ITS FIGURE. Colour is a third carrier here for the same reason
// it is on components/Status.tsx's pills: remove the hue and the row still reads `failed 3`. A chart whose
// only channel is colour is a chart a colourblind operator cannot read, and axe has no rule that would say
// so.

export interface Slice {
  label: string;
  value: number;
  /** The status band this slice belongs to, so a bar takes the same colour its pill does. */
  tone?: "ok" | "warn" | "danger" | "info" | "live" | "neutral";
}

/**
 * Bars is a horizontal bar chart of a categorical breakdown.
 *
 * THE SCALE IS THE LARGEST SLICE, NOT THE TOTAL, and that is the choice that makes a distribution readable:
 * against a total, a category holding 5% of the rows is three pixels wide and indistinguishable from one
 * holding 1%. The figures are printed, so nothing is lost by not being able to eyeball a percentage.
 *
 * A ZERO SLICE IS DRAWN AND NOT DROPPED. "no run failed" is an answer; a chart that silently omits the
 * category has answered nothing, and an operator cannot tell the difference between a state with no rows and
 * a state this console does not know about.
 */
export function Bars({ slices, tally, testId, empty }: { slices: Slice[]; tally: Tally; testId: string; empty: string }) {
  if (tally.state === "loading") {
    return (
      <p className="loading" data-testid={`${testId}-loading`}>
        Loading…
      </p>
    );
  }
  // AN UNREAD COLLECTION IS NOT AN EMPTY ONE, which is components/Stat.tsx's first rule and would be a
  // chart's easiest lie: every bar at zero renders exactly like a deployment doing nothing.
  if (tally.state === "failed") {
    return (
      <p className="muted" data-testid={`${testId}-unread`}>
        This collection could not be read, so there is no distribution to draw. The bars are unknown, not
        zero.
      </p>
    );
  }
  const total = slices.reduce((n, s) => n + s.value, 0);
  if (total === 0) {
    return (
      <p className="muted" data-testid={`${testId}-empty`}>
        {empty}
      </p>
    );
  }
  const peak = Math.max(...slices.map((s) => s.value), 1);
  return (
    // A LIST, NOT A <figure> OF RECTANGLES. The rows are the data; the bar is a second, visual rendering of a
    // figure that is already in the text. A screen reader therefore reads a labelled list of numbers and
    // needs no chart description, and the <svg> is aria-hidden because it says nothing the row does not.
    <ul className="bars" data-testid={testId}>
      {slices.map((slice) => (
        <li key={slice.label} data-tone={slice.tone ?? "neutral"}>
          <span className="bars-label">{slice.label}</span>
          <span className="bars-track">
            <svg viewBox="0 0 100 8" preserveAspectRatio="none" aria-hidden="true" focusable="false">
              <rect x="0" y="0" width={(slice.value / peak) * 100} height="8" rx="1.5" />
            </svg>
          </span>
          <span className="bars-value">{slice.value.toLocaleString()}</span>
        </li>
      ))}
    </ul>
  );
}
