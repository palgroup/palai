"use client";

import { useEffect, useState } from "react";

import { apiGet, RelayError } from "@/lib/api";

interface Revision extends Record<string, unknown> {
  id: string;
}

// AgentDiff renders an agent's revision lineage and a diff between the two most-recent revisions (§47.1
// "agent revisions + diff"). It pulls the first agent + its revisions through the relay. The diff is a
// naive per-line set difference of the two revisions' pretty-printed JSON.
// ponytail: naive line diff (added/removed by membership), not an LCS — enough to show what a revision
// changed; swap for an LCS diff if reviewers need move-aware hunks.
export function AgentDiff() {
  const [revisions, setRevisions] = useState<Revision[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    (async () => {
      const agents = await apiGet<{ data?: { id: string }[] }>("/agents");
      const first = agents.data?.[0];
      if (!first) {
        if (live) setRevisions([]);
        return;
      }
      const revs = await apiGet<{ data?: Revision[] }>(`/agents/${encodeURIComponent(first.id)}/revisions`);
      if (live) setRevisions(revs.data ?? []);
    })().catch((err: unknown) => {
      if (live) setError(err instanceof RelayError ? err.problem.detail : "failed to load");
    });
    return () => {
      live = false;
    };
  }, []);

  return (
    <section className="panel" data-testid="panel-agent-diff" aria-labelledby="agent-diff-h">
      <h2 id="agent-diff-h">Agent revisions &amp; diff</h2>
      {error ? (
        <p role="alert" data-testid="agent-diff-error">
          Error: {error}
        </p>
      ) : revisions === null ? (
        <p data-testid="agent-diff-loading">Loading…</p>
      ) : revisions.length < 2 ? (
        <p data-testid="agent-diff-insufficient">Need at least two revisions to diff.</p>
      ) : (
        <>
          <p className="muted">
            Diff of <code>{revisions[0].id}</code> → <code>{revisions[1].id}</code>
          </p>
          <pre className="code" data-testid="agent-diff-output">
            {diffLines(revisions[1], revisions[0])
              .map((l) => l.text)
              .join("\n")}
          </pre>
        </>
      )}
    </section>
  );
}

function diffLines(older: Revision, newer: Revision): { text: string }[] {
  const oldLines = JSON.stringify(older, null, 2).split("\n");
  const newLines = JSON.stringify(newer, null, 2).split("\n");
  const oldSet = new Set(oldLines);
  const newSet = new Set(newLines);
  const out: { text: string }[] = [];
  for (const l of oldLines) if (!newSet.has(l)) out.push({ text: `- ${l}` });
  for (const l of newLines) out.push({ text: `${oldSet.has(l) ? "  " : "+ "}${l}` });
  return out;
}
