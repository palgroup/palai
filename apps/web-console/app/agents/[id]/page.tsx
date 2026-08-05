"use client";

import { usePathname, useParams, useRouter, useSearchParams } from "next/navigation";
import { useEffect, useState } from "react";

import { Button } from "@/components/ui/Button";
import { AgentDiff } from "@/components/AgentDiff";
import { RevisePublish } from "@/components/RevisePublish";
import { CopyButton, shortId } from "@/components/Session";
import { Status } from "@/components/Status";
import { apiGet, RelayError } from "@/lib/api";
import { lineageOf, modelOf, revisionLabel, type AgentRevision, type AgentRow } from "@/lib/agents";

// ONE AGENT — its lineage, the draft that supersedes it, and the diff between the two newest.
//
// THIS PAGE IS WHERE THE /agents FORMS WENT (page-parity pass). That screen stacked four of them — create an
// agent, choose an agent, create a revision, and a diff with its own agent picker — down one column, so a
// list of twenty-one agents was the first of five things on it and the page had no primary subject. Three of
// the four act on ONE agent, and an agent is what this page is; the fourth (create) is a dialog behind the
// list's own button.
//
// THE "CHOOSE AN AGENT" PICKER IS GONE RATHER THAN MOVED, and that is the point of a detail route: the row
// the operator clicked IS the selection, so a control that re-asks which agent they meant is a control that
// exists because the page had no subject. The same is true of the diff's own picker — components/AgentDiff.tsx
// now takes an `agent` prop and renders no chooser when it is given one.
//
// THE URL CARRIES THE OPEN TAB, exactly as app/sessions/[id]/page.tsx does and for the reasons written there:
// the back button undoes a tab change, a compare view can be sent to somebody as a link, and a reload lands
// where the reader was. `?segment=compare`; the default tab writes no parameter at all, because `?segment=`
// with the default value in it is a URL that says a choice was made when none was.
//
// WHAT IS NOT ON THIS PAGE, and why: a Created stamp. GET /v1/agents answers {id, object, name} and
// GET /v1/agents/{id} answers the same three fields — the timestamp storage/queries/agents.sql selects is
// used to mint a pagination cursor and never serialised (lib/agents.ts carries the measurement). A "created"
// chip here would be a field this console invented.

type Tab = "revisions" | "compare";
const TABS: { id: Tab; label: string }[] = [
  { id: "revisions", label: "Revisions" },
  { id: "compare", label: "Compare" },
];

interface EnvironmentRow {
  id?: string;
  name?: string;
}

interface ToolSetRow {
  id?: string;
  set?: string;
  revision_number?: number;
  status?: string;
}

interface MCPConnectionRow {
  id?: string;
  name?: string;
}

const detail = (err: unknown, fallback: string) => (err instanceof RelayError ? err.problem.detail : fallback);

/** csv splits a comma-separated field into a trimmed list. `tools` is an array on the wire. */
const csv = (raw: string): string[] =>
  raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");

export default function AgentPage() {
  const params = useParams<{ id: string }>();
  const agentID = Array.isArray(params.id) ? (params.id[0] ?? "") : (params.id ?? "");

  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();
  const tab: Tab = search.get("segment") === "compare" ? "compare" : "revisions";

  const [agent, setAgent] = useState<AgentRow | null>(null);
  const [agentError, setAgentError] = useState("");
  const [revisions, setRevisions] = useState<AgentRevision[] | null>(null);
  // A FAILED READ IS ITS OWN STATE, never the pending one. `revisions === null` is what the chip row calls
  // "still loading" and it is also what a rejected fetch would leave behind — so without this a closed door
  // renders "Loading…" forever, and the page's declared readiness signal (lib/routes.ts names `agent-chips`)
  // would never appear at all. That is the same conflation components/Panel.tsx refuses between a spinner
  // and a settled panel.
  const [revisionsError, setRevisionsError] = useState("");
  const [reloadKey, setReloadKey] = useState(0);

  const [model, setModel] = useState("");
  const [instructions, setInstructions] = useState("");
  const [tools, setTools] = useState("");
  const [environment, setEnvironment] = useState("");
  const [toolSet, setToolSet] = useState("");
  const [mcpConnection, setMcpConnection] = useState("");

  const [environments, setEnvironments] = useState<EnvironmentRow[]>([]);
  const [toolSets, setToolSets] = useState<ToolSetRow[]>([]);
  const [mcpConnections, setMcpConnections] = useState<MCPConnectionRow[]>([]);

  useEffect(() => {
    if (agentID === "") return;
    let live = true;
    setAgentError("");
    apiGet<AgentRow>(`/agents/${encodeURIComponent(agentID)}`)
      .then((row) => {
        if (live) setAgent(row);
      })
      .catch((err: unknown) => {
        // The SERVER's sentence. A 404 for an id that was never here and a 401 for a closed door are
        // different facts, and "the agent could not be loaded" is neither of them.
        if (live) setAgentError(detail(err, "the agent could not be read"));
      });
    return () => {
      live = false;
    };
  }, [agentID]);

  // The lineage, for the CHIPS. RevisePublish reads the same collection for its own table; this second read
  // is what lets the summary above the tabs be a summary rather than a duplicate of the table below it, and
  // `reloadKey` is bumped by a create or a publish so the two can never disagree about what is published.
  useEffect(() => {
    if (agentID === "") return;
    let live = true;
    apiGet<{ data?: AgentRevision[]; has_more?: boolean }>(`/agents/${encodeURIComponent(agentID)}/revisions`)
      .then((body) => {
        if (!live) return;
        setRevisions(body.data ?? []);
        setRevisionsError("");
      })
      .catch((err: unknown) => {
        // NOT a role="alert": the revisions panel below reads the same collection and announces the server's
        // own refusal already, and one failure announced twice is one failure a screen reader hears twice.
        // What the chip row owes is to stop claiming to be loading.
        if (live) setRevisionsError(detail(err, "the revisions could not be read"));
      });
    return () => {
      live = false;
    };
  }, [agentID, reloadKey]);

  useEffect(() => {
    let live = true;
    apiGet<{ data?: EnvironmentRow[] }>("/environments")
      .then((body) => {
        if (live) setEnvironments(body.data ?? []);
      })
      .catch(() => {
        // An unreadable list leaves the picker absent, which renders the "create one first" note. An operator
        // who cannot read environments cannot bind one either, so that is the truthful screen.
      });
    apiGet<{ data?: ToolSetRow[] }>("/tool-sets")
      .then((body) => {
        if (live) setToolSets(body.data ?? []);
      })
      .catch(() => {
        /* same rule: an unreadable list is an absent control with a note, never a text box */
      });
    apiGet<{ data?: MCPConnectionRow[] }>("/mcp-connections")
      .then((body) => {
        if (live) setMcpConnections(body.data ?? []);
      })
      .catch(() => {
        /* same rule */
      });
    return () => {
      live = false;
    };
  }, []);

  const lineage = revisions === null ? null : lineageOf(revisions);

  const environmentOptions = environments
    .filter((e) => typeof e.id === "string" && e.id !== "")
    .map((e) => ({ value: String(e.id), label: `${String(e.name ?? e.id)} (${String(e.id)})` }));
  // PUBLISHED ONLY. A draft set revision is accepted by every route here and then advertises nothing — a
  // `tool_sets` id is not reference-checked at create or at publish (automation/agents.go, deliberately: "a
  // typo'd/draft/foreign id fails CLOSED"), so offering one would be offering a choice that fails silently.
  const toolSetOptions = toolSets
    .filter((s) => typeof s.id === "string" && s.id !== "" && s.status === "published")
    .map((s) => ({ value: String(s.id), label: `${String(s.set ?? "?")} #${String(s.revision_number ?? "?")} (${String(s.id)})` }));
  const mcpConnectionOptions = mcpConnections
    .filter((c) => typeof c.id === "string" && c.id !== "")
    .map((c) => ({ value: String(c.id), label: `${String(c.name ?? c.id)} (${String(c.id)})` }));

  function chooseTab(next: Tab) {
    const params = new URLSearchParams(search.toString());
    if (next === "revisions") params.delete("segment");
    else params.set("segment", next);
    const query = params.toString();
    // push, not replace: the back button undoes a tab change, which is the browser's own affordance doing
    // what an operator expects. scroll:false — switching tabs must not jump the reader to the top.
    router.push(query === "" ? pathname : `${pathname}?${query}`, { scroll: false });
  }

  function moveTab(from: Tab, delta: number) {
    const index = TABS.findIndex((t) => t.id === from);
    const next = TABS[(index + delta + TABS.length) % TABS.length];
    chooseTab(next.id);
    document.getElementById(`tab-${next.id}`)?.focus();
  }

  const name = String(agent?.name ?? "").trim();

  return (
    <>
      {/* THE BREADCRUMB CARRIES THE NAME, NOT THE ID, AND THAT IS THE REFERENCE'S OWN SPLIT (E30).
          Measured: the AGENT detail reads `Agents / palcore Mac Spike` and the SESSION detail reads
          `Sessions / ⧉sesn_…3c2jein`. It is not an inconsistency in the reference — an agent HAS a name a
          person chose and a session usually does not, so each trail shows the thing that identifies the
          record to a reader. The id has not left the page; it moved to the metadata line under the title,
          with the same copy button, which is where the reference puts it too. */}
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>
            <a href="/agents">Agents</a>
          </li>
          <li aria-current="page" data-testid="breadcrumb-agent">
            {name === "" ? <code>{shortId(agentID)}</code> : name}
          </li>
        </ol>
      </nav>

      {agentError === "" ? null : (
        <p role="alert" className="form-error" data-testid="agent-error">
          <span className="glyph" aria-hidden="true">
            ✖︎
          </span>{" "}
          {agentError}
        </p>
      )}

      {/* THE TITLE WITH ITS STATUS PILL INLINE — measured on the reference's agent page, 2026-08-01. The pill
          is the ONE fact about this record that changes what an operator can do with it, so it sits beside
          the name; everything else is a reading and goes in the line below. */}
      <div className="detail-head-row">
        <h1 className="page-title" data-testid="agent-title">
          {agent === null ? <code>{shortId(agentID)}</code> : name === "" ? <span className="cell-none">— unnamed</span> : name}
        </h1>
        {revisionsError === "" && lineage !== null ? <Status value={lineage.state} testId="chip-state" /> : null}
      </div>

      {/* THE METADATA LINE REPLACES FOUR CHIPS, AND THE REFERENCE IS EXPLICIT ABOUT THE SHAPE: under the
          title it puts `agent_0144c…` and `Last updated 6 days ago` in the secondary colour, with no box
          around either. Ours had the id in the breadcrumb and four bordered chips here — four boxes for
          facts nobody clicks. A chip is for a value you can act on; a reading is a reading, and a reading in
          a box is a box.

          THE TESTID STAYS `agent-chips` AND SO DO THE FOUR PER-FACT ONES. lib/routes.ts names this element
          as the route's readiness signal and tests/config-journey.spec.ts reads `chip-state` and
          `chip-model` through a whole publish journey; renaming them would have been a rewrite of the
          evidence to match a layout, which is the one direction a change like this must never go. What
          changed is what a reader sees, not what a test can find. */}
      {revisionsError !== "" ? (
        <p className="detail-meta" data-testid="agent-chips">
          <span data-testid="chip-state" className="cell-none">
            — the lineage could not be read
          </span>
        </p>
      ) : lineage === null ? (
        <p className="loading">Loading…</p>
      ) : (
        <p className="detail-meta" data-testid="agent-chips">
          <span className="cell-id-group">
            <code title={agentID}>{shortId(agentID)}</code>
            <CopyButton value={agentID} label="agent ID" testId="breadcrumb-agent" />
          </span>
          <span data-testid="chip-revisions">
            {lineage.count} {lineage.count === 1 ? "revision" : "revisions"}
          </span>
          <span data-testid="chip-published">
            {lineage.published === null ? "No published revision" : <>Latest published {revisionLabel(lineage.published)}</>}
          </span>
          <span data-testid="chip-model">{modelOf(lineage.newest)}</span>
        </p>
      )}

      <div className="tabs" role="tablist" aria-label="Agent views">
        {TABS.map((t) => (
          <Button
            key={t.id}
            id={`tab-${t.id}`}
            role="tab"
            className="tab"
            aria-selected={tab === t.id}
            aria-controls={`panel-${t.id}`}
            tabIndex={tab === t.id ? 0 : -1}
            testId={`tab-${t.id}`}
            onClick={() => chooseTab(t.id)}
            onKeyDown={(e) => {
              if (e.key === "ArrowRight") moveTab(t.id, 1);
              if (e.key === "ArrowLeft") moveTab(t.id, -1);
            }}
          >
            {t.label}
          </Button>
        ))}
      </div>

      <div className="tab-panel" role="tabpanel" id="panel-revisions" aria-labelledby="tab-revisions" tabIndex={0} hidden={tab !== "revisions"}>
        <RevisePublish
          title="New revision"
          testId="agent-revision"
          note={
            <>
              A draft is created first and <strong>published as a separate step</strong>, because a run can
              only be pinned to a published revision.
            </>
          }
          createPath={`/agents/${encodeURIComponent(agentID)}/revisions`}
          listPath={`/agents/${encodeURIComponent(agentID)}/revisions`}
          publishPath={(rev) => `/agents/${encodeURIComponent(agentID)}/revisions/${encodeURIComponent(String(rev.id ?? ""))}/publish`}
          emptyNote="This page is keyed by an agent id and the address bar carries none."
          onCreated={() => {
            setModel("");
            setInstructions("");
            setTools("");
          }}
          // A CREATE **AND** A PUBLISH both move what the chip row says. Bumping only on create is the bug
          // this prop was added for — RevisePublish's own header records it.
          onChanged={() => setReloadKey((n) => n + 1)}
          fields={[
            {
              name: "revision-model",
              label: "Model",
              value: model,
              onChange: setModel,
              hint: "Overrides the project route and the deployment default for runs pinned to this revision. Leave empty to inherit.",
              testId: "revision-model-input",
            },
            {
              name: "revision-instructions",
              label: "Instructions",
              kind: "textarea",
              value: instructions,
              onChange: setInstructions,
              hint: "The system instructions this agent runs with.",
              testId: "revision-instructions-input",
            },
            {
              name: "revision-tools",
              label: "Tools",
              value: tools,
              onChange: setTools,
              hint: "Comma-separated tool names. This is a CEILING on what a run pinned to this revision may call, not a grant — a name here that the project does not grant stays unavailable.",
              testId: "revision-tools-input",
            },
            {
              name: "revision-environment",
              label: "Environment",
              kind: "select",
              value: environment,
              onChange: setEnvironment,
              options: environmentOptions,
              placeholder: "None — this agent's shell gets no extra keys",
              testId: "revision-environment-select",
              hint: "Its KEY=value pairs reach every shell command of every run pinned to this revision — never something the model is shown, and never part of a prompt. A value written into an environment can never be read back.",
              // NO FREE-TEXT FALLBACK. Publishing a revision that names a missing environment is refused with
              // a 400 (store/agents.go, automation.ErrEnvironmentNotFound), which is a refusal about a field
              // the operator typed several steps earlier.
              emptyNote: (
                <>
                  <strong>Create an environment first.</strong> There is nothing to bind, and an id cannot be
                  typed here on purpose. <a href="/environments">Go to Environments</a>.
                </>
              ),
            },
            {
              name: "revision-tool-set",
              label: "Tool set",
              kind: "select",
              value: toolSet,
              onChange: setToolSet,
              options: toolSetOptions,
              placeholder: "None — this agent reaches no registered tool",
              testId: "revision-tool-set-select",
              hint: "The GRANT: a published set revision, pinning exact tool revisions. Published sets only — a draft pinned here would be accepted and then advertise nothing.",
              emptyNote: (
                <>
                  <strong>Publish a tool set first.</strong> Register an MCP connection, approve its tools and
                  pin them into a set on the <a href="/tools">Tools</a> page. An id cannot be typed here on
                  purpose: a wrong one is accepted by every route and then grants nothing, silently.
                </>
              ),
            },
            {
              name: "revision-mcp-connection",
              label: "MCP connection",
              kind: "select",
              value: mcpConnection,
              onChange: setMcpConnection,
              options: mcpConnectionOptions,
              placeholder: "None — this agent reaches no MCP server",
              testId: "revision-mcp-connection-select",
              hint: "The CEILING, not the grant: a run may only reach connections its revision names. An MCP tool needs BOTH this and the tool set above — each one missing fails quietly, and differently.",
              emptyNote: (
                <>
                  {/* IT POINTED AT /tools AND THE CONTROL IS NOT THERE. An MCP connection is created on
                      /mcp — that screen did not exist when this note was written, so the sentence sent an
                      operator to a page with no such control and left them hunting for it. */}
                  <strong>Register an MCP connection first.</strong> <a href="/mcp">Go to MCP</a>. Without one
                  an MCP tool resolves to nothing even when it is advertised.
                </>
              ),
            },
          ]}
          buildBody={() => ({
            // Only what was FILLED IN. An empty string on `model` would pin the revision to a model named "",
            // and an empty `environment` is the no-environment case the store reads as "inherit nothing".
            ...(model === "" ? {} : { model }),
            ...(instructions === "" ? {} : { instructions }),
            ...(tools === "" ? {} : { tools: csv(tools) }),
            ...(environment === "" ? {} : { environment }),
            // One-element arrays: the field is a list and this control offers one. An empty list and an
            // ABSENT field are not the same thing here — nil means "no ceiling declared" — so neither is sent
            // when nothing was chosen.
            ...(toolSet === "" ? {} : { tool_sets: [toolSet] }),
            ...(mcpConnection === "" ? {} : { mcp_connections: [mcpConnection] }),
          })}
          columns={[
            { header: "Model", render: (rev) => String(rev.model ?? "— inherited") },
            {
              header: "Environment",
              render: (rev) => (
                <span data-testid={`revision-environment-${String(rev.id ?? "")}`}>{rev.environment ? String(rev.environment) : "— none"}</span>
              ),
            },
            { header: "Tools", render: (rev) => (Array.isArray(rev.tools) ? rev.tools.join(", ") : "— inherited") },
            {
              header: "Tool sets",
              render: (rev) => (
                <span data-testid={`revision-tool-sets-${String(rev.id ?? "")}`}>
                  {Array.isArray(rev.tool_sets) && rev.tool_sets.length > 0 ? rev.tool_sets.join(", ") : "— none"}
                </span>
              ),
            },
            {
              header: "MCP connections",
              render: (rev) => (
                <span data-testid={`revision-mcp-connections-${String(rev.id ?? "")}`}>
                  {Array.isArray(rev.mcp_connections) && rev.mcp_connections.length > 0 ? rev.mcp_connections.join(", ") : "— none"}
                </span>
              ),
            },
          ]}
        />
      </div>

      <div className="tab-panel" role="tabpanel" id="panel-compare" aria-labelledby="tab-compare" tabIndex={0} hidden={tab !== "compare"}>
        <AgentDiff agent={agentID} />
      </div>
    </>
  );
}
