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
export function Timeline({ frames }: { frames: Frame[] }) {
  const events = frames.filter((f) => typeof f.type === "string" && f.lane !== undefined);
  return (
    <section className="panel" data-testid="timeline" aria-labelledby="timeline-h">
      <h2 id="timeline-h">Run timeline</h2>
      <ol>
        {events.map((f, i) => (
          <li key={i} className="lane" data-lane={f.lane} data-type={f.type}>
            <span className="muted">[{f.lane}]</span> {f.type}
          </li>
        ))}
      </ol>
    </section>
  );
}
