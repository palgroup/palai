"use client";

import { useEffect, useState, type ReactNode } from "react";

import { apiGet } from "@/lib/api";

// THE OVERVIEW'S NUMBERS, AND EVERY ONE OF THEM IS HONEST ABOUT ITS OWN CEILING.
//
// A dashboard figure is the most quotable thing on a console and the easiest to make wrong. Three ways it
// goes wrong here, and each one is handled rather than hoped away:
//
//   1. A COLLECTION THAT WAS CUT. api/pagination.go's defaultPageLimit is 20, so `data.length` over a
//      paginated read is a FLOOR, not a count. When `has_more` is true the card renders "20+" and says so —
//      a flat "20" beside a collection of two hundred agents is a lie the operator has no way to detect.
//   2. A READ THAT FAILED. An unreachable list is an em dash and a sentence, never a zero: "0 agents" and
//      "the agent list did not load" are different facts, and a deployment where the second is rendered as
//      the first is a deployment nobody can triage from this screen.
//   3. A COLLECTION THAT IS GENUINELY EMPTY. That is a zero, and it is the answer — which is only readable
//      because the two cases above are not also zeros.

/** A tallied collection: how many rows were read, whether the server said more remain, and whether it read. */
export interface Tally {
  count: number;
  atLeast: boolean;
  state: "loading" | "ok" | "failed";
  /** The rows themselves, for a card whose meta line breaks the figure down (statuses, revocations). */
  rows: Record<string, unknown>[];
}

const PENDING: Tally = { count: 0, atLeast: false, state: "loading", rows: [] };

/**
 * useTally reads one /v1 collection through the relay and reports its size.
 *
 * `select` exists for the reads that are not `{data: [...]}` — GET /v1/usage carries its meters under
 * `meters` and GET /v1/capabilities answers a map — the same reason components/Panel.tsx takes one.
 */
export function useTally(path: string, select?: (body: Record<string, unknown>) => Record<string, unknown>[]): Tally {
  const [tally, setTally] = useState<Tally>(PENDING);
  useEffect(() => {
    let live = true;
    apiGet<Record<string, unknown>>(path)
      .then((body) => {
        if (!live) return;
        const rows = select?.(body) ?? ((body.data as Record<string, unknown>[] | undefined) ?? []);
        setTally({ count: rows.length, atLeast: body.has_more === true, state: "ok", rows });
      })
      .catch(() => {
        if (live) setTally({ count: 0, atLeast: false, state: "failed", rows: [] });
      });
    return () => {
      live = false;
    };
    // `select` is deliberately not a dependency: callers pass an inline arrow, and depending on it would
    // refetch on every render. The DATA input is the path.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path]);
  return tally;
}

/**
 * Stat renders one figure with what it counts above it and what it means below it.
 *
 * `tone="attention"` colours the NUMBER — never the card — and it is for the one figure on the screen that
 * is a reason to act. A dashboard where four cards are highlighted has highlighted nothing.
 */
export function Stat({
  label,
  tally,
  meta,
  href,
  tone,
  testId,
}: {
  label: string;
  tally: Tally;
  meta: ReactNode;
  href?: string;
  tone?: "attention";
  testId: string;
}) {
  const unread = tally.state !== "ok";
  const figure = tally.state === "loading" ? "…" : tally.state === "failed" ? "—" : `${String(tally.count)}${tally.atLeast ? "+" : ""}`;
  return (
    <div className="stat-card" data-tone={tone} data-testid={testId}>
      <p className="micro-label">{label}</p>
      <span className="stat-value" data-unread={unread ? "true" : undefined} data-testid={`${testId}-value`}>
        {figure}
      </span>
      <p className="stat-meta">
        {tally.state === "failed" ? (
          "This list could not be read. The figure is unknown, not zero."
        ) : tally.atLeast ? (
          <>
            The first page. The server has more{href === undefined ? "" : " — "}
            {href === undefined ? "." : <a href={href}>see the full list</a>}
          </>
        ) : (
          meta
        )}
      </p>
    </div>
  );
}
