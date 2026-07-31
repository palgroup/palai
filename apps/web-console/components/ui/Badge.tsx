import type { ReactNode } from "react";

// THE BADGE WRAPS WHAT IS ALREADY THERE AND REDESIGNS NOTHING (E29 component layer).
//
// `.status` in app/globals.css §9 already measures exactly as the reference console's pill does — 12px/15px
// at weight 500, padding 0 8, radius 5, height 20 — and it already carries the property this console will not
// give up: the meaning is in the GLYPH and the WORD, and the colour is a redundant third layer. So this file
// adds no rule, changes no token, and moves no pixel. What it adds is a NAME: `<Badge tone="ok">` instead of
// a bare `<span className="status" data-glyph="ok">` written out at each call site.
//
// components/Status.tsx keeps the one thing a primitive must not own — the CLASSIFIER that maps a status word
// to its glyph and its band. That mapping is domain knowledge ("restored" is an ok, "lost" is a danger), and
// pulling it in here would make this component know about run lifecycles.

export type BadgeTone = "ok" | "danger" | "info" | "warn" | "neutral";

export function Badge({
  tone,
  glyph,
  children,
  title,
  testId,
}: {
  tone: BadgeTone;
  /** Rendered aria-hidden: it is the redundant visual carrier, and a screen reader gets `children`. */
  glyph: string;
  children: ReactNode;
  /** The raw value the tone was classified FROM, kept as a data attribute for the stylesheet and for tests. */
  title?: string;
  testId?: string;
}) {
  return (
    <span className="status" data-testid={testId} data-status={title} data-glyph={tone}>
      <span className="glyph" aria-hidden="true">
        {glyph}
      </span>
      <span>{children}</span>
    </span>
  );
}
