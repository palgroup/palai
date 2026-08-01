"use client";

import { useEffect, useState } from "react";

import { Picker } from "@/components/Picker";
import { apiGet, RelayError } from "@/lib/api";

interface Revision extends Record<string, unknown> {
  id: string;
}

interface AgentRow {
  id: string;
  name?: string;
}

// AgentDiff renders an agent's revision lineage and a diff between the two most-recent revisions (§47.1
// "agent revisions + diff"). It pulls the agents list + one agent's revisions through the relay. The diff is
// a naive per-line set difference of the two revisions' pretty-printed JSON.
// ponytail: naive line diff (added/removed by membership), not an LCS — enough to show what a revision
// changed; swap for an LCS diff if reviewers need move-aware hunks.
//
// IT SELECTS AN AGENT (E25 T6). It was pinned to `agents.data?.[0]` — the FIRST row of the first page — which
// was defensible while nothing in the console could create a second agent and stopped being defensible the
// moment /agents could. The list is also PAGINATED (api/pagination.go serves agents through renderPage, 20
// rows), so "the first row" was never even "the first agent" in any sense an operator would recognise; it was
// the first row of whichever page arrived. The default is still row zero, so the panel shows what it always
// showed on a single-agent deployment.
//
// AND IT TAKES ONE INSTEAD, WHEN THE PAGE ALREADY KNOWS (page-parity pass). On app/agents/[id]/page.tsx the
// agent is the SUBJECT of the route, so a picker there would ask the operator to re-choose the row they
// clicked to get here — and it would offer them the other twenty, on a page about one. With `agent` set the
// chooser is not rendered and the agents list is never fetched: this component's second read was only ever
// there to fill that control.
export function AgentDiff({ agent }: { agent?: string } = {}) {
  const pinned = agent !== undefined && agent !== "";
  const [agents, setAgents] = useState<AgentRow[] | null>(null);
  const [selected, setSelected] = useState(agent ?? "");
  const [revisions, setRevisions] = useState<Revision[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (pinned) {
      setSelected(agent);
      return;
    }
    let live = true;
    apiGet<{ data?: AgentRow[] }>("/agents")
      .then((body) => {
        if (!live) return;
        const rows = body.data ?? [];
        setAgents(rows);
        // Row zero by default — the pre-E25-T6 behaviour, so a single-agent deployment sees no change.
        if (rows[0] !== undefined) setSelected(rows[0].id);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof RelayError ? err.problem.detail : "failed to load");
      });
    return () => {
      live = false;
    };
  }, [pinned, agent]);

  useEffect(() => {
    if (selected === "") {
      setRevisions(null);
      return;
    }
    let live = true;
    setRevisions(null);
    apiGet<{ data?: Revision[] }>(`/agents/${encodeURIComponent(selected)}/revisions`)
      .then((body) => {
        if (live) setRevisions(body.data ?? []);
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof RelayError ? err.problem.detail : "failed to load");
      });
    return () => {
      live = false;
    };
  }, [selected]);

  return (
    <section className="panel" data-testid="panel-agent-diff" aria-labelledby="agent-diff-h">
      <h2 id="agent-diff-h">{pinned ? "Newest two revisions, diffed" : "Agent revisions & diff"}</h2>
      {pinned || agents === null ? null : (
        <Picker
          id="agent-diff-select"
          label="Agent"
          value={selected}
          onChange={setSelected}
          options={agents.map((a) => ({ value: a.id, label: a.name === undefined || a.name === "" ? a.id : `${a.name} (${a.id})` }))}
          testId="agent-diff-select"
          manage={{ href: "/agents", label: "Manage agents" }}
          emptyNote={
            <span data-testid="agent-diff-no-agents">
              No agents yet. <a href="/agents">Create one</a>.
            </span>
          }
        />
      )}
      {error ? (
        <p role="alert" className="form-error" data-testid="agent-diff-error">
          Error: {error}
        </p>
      ) : agents !== null && agents.length === 0 ? null : revisions === null ? (
        <p className="loading" data-testid="agent-diff-loading">Loading…</p>
      ) : revisions.length < 2 ? (
        // THE MEASURED EMPTY-STATE SHAPE, and the count is IN it: "Need at least two revisions to diff"
        // stated a rule and left the reader to go and count the rows themselves. A lineage with one
        // revision and a lineage with none are different situations and the second sentence says which.
        <div className="empty" data-testid="agent-diff-insufficient">
          <p className="empty-title">Nothing to compare yet</p>
          <p className="empty-body">
            A diff is between the two newest revisions of one lineage, and this one has{" "}
            {revisions.length === 0 ? "none" : "one"}. Create another on the Revisions tab.
          </p>
        </div>
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
