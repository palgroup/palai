"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState, type ReactNode } from "react";

import { Button } from "@/components/ui/Button";
import { Menu } from "@/components/ui/Menu";
import { AgentTemplates } from "@/components/AgentTemplates";
import { FormDialog } from "@/components/FormDialog";
import { Panel, type Column } from "@/components/Panel";
import { ResourceForm } from "@/components/ResourceForm";
import { CopyButton, shortId } from "@/components/Session";
import { Status } from "@/components/Status";
import { apiGet, apiSend, RelayError } from "@/lib/api";
import { lineageOf, modelOf, revisionLabel, type AgentRevision, type AgentRow, type Lineage } from "@/lib/agents";

// THE AGENTS LIST — one row per lineage, and every column is a field (page-parity pass).
//
// WHAT THIS SCREEN WAS, measured on the built console 2026-07-31 with
// `node scripts/…/parity-prose.mjs http://127.0.0.1:3310 /agents`: 3501 characters of <main>, 1158 of them
// grey prose, ONE table column, and five <h2>s — "Agents", "Create an agent", "Choose an agent", "Create a
// revision", "Agent revisions & diff". Four forms stacked down one page, a table whose only column was Name
// with the id underneath it, and a row that went nowhere. A page with four forms has no primary subject, and
// the operator paid for three paragraphs of explanation on every visit to a list they read in two seconds.
//
// WHAT IT IS NOW: the collection, its own create action, and a row that opens. Everything that acts on ONE
// agent — the revision draft, the publish, the diff — moved to app/agents/[id]/page.tsx, which is that
// agent's page. The rules that used to be prose here are in docs/operations/console.md §4c and on the
// controls they govern; nothing was deleted (see that section's opening line).
//
// THE COLUMNS ARE NOT ALL FROM THE LIST, AND THAT IS STATED RATHER THAN HIDDEN. GET /v1/agents answers
// {id, object, name} per row and nothing else — lib/agents.ts carries the grep that measures it, including
// why there is no Created column — so Model / Revisions / Status come from ONE EXTRA READ PER ROW of
// GET /v1/agents/{id}/revisions. That is an N+1 and it is deliberate:
//
//   * It is bounded by what is ON SCREEN. The fetch runs over the rows Panel actually rendered, so a
//     truncated collection costs a page of reads and not a collection of them (21 on the fixture, which is
//     one more than a page — DIV-UI-005).
//   * The alternative is a two-column table. "Can a run be pinned to this agent" is the question this screen
//     exists to answer and the list endpoint does not answer it; a console that shows only what one endpoint
//     returns is a console that shows the id twice.
//   * Every cell says which state it is in. A lineage still loading renders "…" rather than a zero, because
//     a zero is a claim.
//
// ponytail: reads are per-row, concurrency-capped, and never re-read once a row's lineage has landed (a
// created agent bumps `reloadKey`, which starts the list — and therefore the lineages — again). No cache
// across navigations. Upgrade path: if /v1/agents ever projects the lineage summary itself, delete `useLineages`
// and read the fields off the row.

/** How many revision reads are in flight at once. A page of 21 rows through one relay is not a fan-out worth
 *  making unbounded; six keeps the list responsive without queueing behind itself. */
const CONCURRENCY = 6;

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/**
 * useLineages reads one revision page per agent id and returns what each one says.
 *
 * A FAILED READ IS `null` AND RENDERS AS SUCH. An agent whose revisions cannot be read is not an agent with
 * no revisions, and collapsing the two would put "no revisions" on a row whose lineage the operator cannot
 * see — the same conflation components/Panel.tsx refuses between an empty collection and a filter that
 * matched nothing.
 */
function useLineages(ids: string[]): Map<string, Lineage | null> {
  const [lineages, setLineages] = useState<Map<string, Lineage | null>>(new Map());

  // The ids as ONE string, so an inline array literal from the caller cannot re-trigger this on every render.
  const key = ids.join(",");

  useEffect(() => {
    const wanted = key === "" ? [] : key.split(",");
    if (wanted.length === 0) return;
    let live = true;
    let next = 0;
    const worker = async () => {
      while (live) {
        const index = next;
        next += 1;
        if (index >= wanted.length) return;
        const id = wanted[index];
        try {
          const body = await apiGet<{ data?: AgentRevision[]; has_more?: boolean }>(`/agents/${encodeURIComponent(id)}/revisions`);
          if (!live) return;
          const lineage = lineageOf(body.data ?? [], body.has_more === true);
          setLineages((prev) => new Map(prev).set(id, lineage));
        } catch {
          if (!live) return;
          setLineages((prev) => new Map(prev).set(id, null));
        }
      }
    };
    void Promise.all(Array.from({ length: Math.min(CONCURRENCY, wanted.length) }, worker));
    return () => {
      live = false;
    };
  }, [key]);

  return lineages;
}

export default function AgentsPage() {
  const router = useRouter();
  const [rows, setRows] = useState<AgentRow[]>([]);
  const [reloadKey, setReloadKey] = useState(0);

  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [createError, setCreateError] = useState("");
  const [busy, setBusy] = useState(false);

  const ids = rows.map((r) => String(r.id ?? "")).filter((id) => id !== "");
  const lineages = useLineages(ids);

  async function createAgent() {
    setBusy(true);
    setCreateError("");
    try {
      const body = await apiSend<AgentRow>("POST", "/agents", { name: newName });
      setNewName("");
      setCreating(false);
      // STRAIGHT TO THE AGENT THAT WAS JUST MADE. The old screen selected it in a dropdown and left the
      // operator on a page of forms; the thing they came here to do next is write its first revision, and
      // that is on its own page. A created id the server did not return leaves them on a refreshed list.
      if (typeof body.id === "string" && body.id !== "") router.push(`/agents/${encodeURIComponent(body.id)}`);
      else setReloadKey((n) => n + 1);
    } catch (err: unknown) {
      setCreateError(detail(err, "the agent could not be created"));
    } finally {
      setBusy(false);
    }
  }

  /** cell renders a lineage-derived value, saying which of the three states the row is in. */
  const cell = (id: string, of: (lineage: Lineage) => ReactNode) => {
    if (!lineages.has(id)) return <span className="cell-none">…</span>;
    const lineage = lineages.get(id);
    if (lineage === null || lineage === undefined) {
      return (
        <span className="cell-none" title="This agent's revisions could not be read. It is not a claim that it has none.">
          — unreadable
        </span>
      );
    }
    return of(lineage);
  };

  const columns: Column<AgentRow>[] = [
    {
      header: "ID",
      sort: (r) => String(r.id ?? ""),
      // A SHORT FORM, A COPY BUTTON AND A LINK — the shape app/sessions/page.tsx measured and the correction
      // to what this table did before: a 36-character id stacked under a name, which an operator can neither
      // read nor reliably select, and which was not a link to anything.
      render: (r) => (
        <span className="cell-id-group">
          <a className="cell-id" href={`/agents/${encodeURIComponent(String(r.id ?? ""))}`} title={String(r.id ?? "")} data-testid="agent-link">
            {shortId(String(r.id ?? ""))}
          </a>
          <CopyButton value={String(r.id ?? "")} label="agent ID" testId="agent-copy-id" />
        </span>
      ),
    },
    {
      header: "Name",
      sort: (r) => String(r.name ?? ""),
      render: (r) => (
        <a className="cell-name-link" href={`/agents/${encodeURIComponent(String(r.id ?? ""))}`} data-testid="agent-name-link">
          {String(r.name ?? "").trim() === "" ? <span className="cell-none">— unnamed</span> : String(r.name)}
        </a>
      ),
    },
    {
      header: "Model",
      // The NEWEST revision's model, not the published one, and the header would be lying if it were the
      // other way round: this column answers "what does the next publish run", and a lineage whose newest
      // revision is a draft is exactly the case where the two differ.
      render: (r) => cell(String(r.id ?? ""), (l) => <span data-testid="agent-model">{modelOf(l.newest)}</span>),
    },
    {
      header: "Revisions",
      numeric: true,
      render: (r) =>
        cell(String(r.id ?? ""), (l) => (
          <span className="num" data-testid="agent-revision-count">
            {l.count}
            {l.truncated ? "+" : ""}
          </span>
        )),
    },
    {
      header: "Latest published",
      // The revision a RUN can be pinned to. An em dash here is the difference between an agent that is
      // configured and one that is only named, which is the single most useful fact on the row.
      render: (r) =>
        cell(String(r.id ?? ""), (l) =>
          l.published === null ? (
            <span className="cell-none" data-testid="agent-published">
              — none
            </span>
          ) : (
            <span data-testid="agent-published">{revisionLabel(l.published)}</span>
          ),
        ),
    },
    {
      header: "Status",
      render: (r) => cell(String(r.id ?? ""), (l) => <Status value={l.state} testId="agent-status" />),
    },
    {
      header: "",
      // THE ROW-END MENU, AND ITS TWO ITEMS ARE THE TWO THINGS A LINEAGE CAN BE ASKED FOR. There is no
      // rename, no delete and no duplicate here because api/router.go:52-59 mounts no PATCH and no DELETE on
      // /v1/agents — a third entry would have to be a control that refuses. Both items are LINKS to this
      // agent's own page, which is where every write on this surface lives.
      render: (r) => {
        const id = String(r.id ?? "");
        return (
          <div className="row-menu">
            <Menu
              label={`Actions for agent ${id}`}
              trigger={<span aria-hidden="true">⋯</span>}
              triggerClassName="row-menu-toggle"
              triggerTestId="agent-menu"
              items={[
                { label: "Revisions", href: `/agents/${encodeURIComponent(id)}`, testId: "agent-menu-revisions" },
                { label: "Compare revisions", href: `/agents/${encodeURIComponent(id)}?segment=compare`, testId: "agent-menu-compare" },
              ]}
            />
          </div>
        );
      },
    },
  ];

  return (
    <>
      {/* BELOW THE LIST, NOT ABOVE IT. The list is what this screen IS and what an operator with agents
          comes back for; the gallery is for the first day. It routes to the new agent's own page for the
          same reason createAgent does — the thing you do next is write or publish a revision, and that
          lives there. */}
      <Panel<AgentRow>
        title="Agents"
        testId="panel-agent-profiles"
        fetchPath="/agents"
        reloadKey={reloadKey}
        onRows={setRows}
        columns={columns}
        matchOn={(r) => `${String(r.id ?? "")} ${String(r.name ?? "")}`}
        filterLabel="Search agents by name or ID"
        filterPlaceholder="Name or ID…"
        action={
          <Button variant="primary" testId="agent-create-open" onClick={() => setCreating(true)}>
            + New agent
          </Button>
        }
        emptyNote={
          <>
            <p className="empty-title" data-testid="agent-empty-title">
              No agents yet
            </p>
            <p className="empty-body">
              An agent is a name with a lineage of immutable revisions — a run is pinned to one of them, which
              is what makes it reproducible.
            </p>
            <Button variant="primary" testId="agent-create-open-empty" onClick={() => setCreating(true)}>
              Create one
            </Button>
          </>
        }
      />

      <AgentTemplates onCreated={(id) => router.push(`/agents/${encodeURIComponent(id)}`)} />

      {creating ? (
        <FormDialog
          label="Create an agent"
          testId="agent-create-dialog"
          onClose={() => {
            setCreating(false);
            setCreateError("");
          }}
        >
          <ResourceForm
            title="Create an agent"
            testId="agent-create"
            fields={[
              {
                name: "agent-name",
                label: "Name",
                required: true,
                value: newName,
                onChange: setNewName,
                hint: "For example: deployer, docs-writer, release-bot. A name is all it takes — the executable configuration is a revision, and this agent's page is where you write one.",
                testId: "agent-name-input",
              },
            ]}
            submitLabel="Create agent"
            submitTestId="agent-create-button"
            submitting={busy}
            error={createError}
            onSubmit={createAgent}
            actions={
              <Button
                testId="agent-create-cancel"
                onClick={() => {
                  setCreating(false);
                  setCreateError("");
                }}
              >
                Cancel
              </Button>
            }
          />
        </FormDialog>
      ) : null}
    </>
  );
}
