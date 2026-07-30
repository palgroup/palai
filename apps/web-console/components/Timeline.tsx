"use client";

import type { Lane } from "@/lib/timeline";

export interface Frame extends Record<string, unknown> {
  lane?: Lane;
  type?: string;
  sequence?: number;
}

// Timeline renders the ordered canonical event stream, each row tagged with its §47.2 LANE so the
// separation is visible: a model step, a tool call, an approval, a recovery transition, usage, and the
// terminal result each read as a distinct category rather than one undifferentiated log. The lane label
// is a data attribute (machine-checkable) and visible text (human + screen reader).
//
// `title`/`testId` are overridable (E25 T8) because this component now renders in TWO places: the live
// stream on /runs and the REPLAY of a finished run's journal on /history. They never coexist on one page,
// but they are asserted on separately, and two pages answering one testid is how a spec ends up asserting
// against whichever page it happened to be on (the same reason lib/routes.ts spells out panel-agent-profiles).
export function Timeline({ frames, title = "Run timeline", testId = "timeline" }: { frames: Frame[]; title?: string; testId?: string }) {
  const events = frames.filter((f) => typeof f.type === "string" && f.lane !== undefined);
  return (
    <section className="panel" data-testid={testId} aria-labelledby={`${testId}-h`}>
      <h2 id={`${testId}-h`}>{title}</h2>
      <ol className="timeline">
        {events.map((f, i) => (
          <li key={i} className="lane" data-lane={f.lane} data-type={f.type}>
            <span className="muted">[{f.lane}]</span> {f.type}
          </li>
        ))}
      </ol>
    </section>
  );
}
