"use client";

import { NameCell, Panel } from "@/components/Panel";
import { Stat, useTally } from "@/components/Stat";

// THE OVERVIEW — what this deployment is doing, in one screen (console design pass).
//
// WHAT THIS PAGE WAS: every registry the public API answers, stacked. Organizations, projects, keys, model
// connections, model routes, secret refs, knowledge bases, agents and an agent diff — nine panels, twenty
// rows each, about twelve screens of scrolling, in the order the epics happened to add them. Nothing on it
// answered the question an operator opens a console to ask, and the first thing on it was a paragraph of
// standing caveats. A console reads as monotone when everything is shown at equal weight; this page was the
// proof.
//
// WHAT IT IS NOW: a band of figures with their ceilings stated, then the collections that identify the
// deployment — name-first, counted, and short. Model connections, model routes and knowledge bases moved to
// /registry, and the agent-revision diff moved to /agents where the lineage it diffs already lives.
//
// FOUR COLLECTIONS STAY HERE BECAUSE SHIPPED SPECS PIN THEM TO THIS PATH, and that is recorded rather than
// quietly worked around — tests/public-api-only.spec.ts reads `panel-organizations` and `panel-secret-refs`
// on "/", tests/pagination.spec.ts drives `panel-agents` here, and tests/profile.ts's sign-in lands on
// `ceiling-note`. Three of those files are ones this pass must not edit. They are not passengers, though:
// the organisations and projects ARE the scope every other screen reads, the agent list is the deployment's
// unit of work, and secret refs are the one collection whose metadata-only projection is worth seeing beside
// the keys that use it.
//
// EVERY PANEL FETCHES THROUGH THE RELAY (browser → /api/palai/v1/* → upstream /v1/*), so the public-API-only
// intercept still sees the whole admin surface ride it. Secret-ref VALUES are never rendered — the read
// surface is metadata only.
export default function OverviewPage() {
  const approvals = useTally("/approvals");
  const runs = useTally("/responses");
  const agents = useTally("/agents");
  const projects = useTally("/projects");
  const keys = useTally("/api-keys");

  const completed = runs.rows.filter((r) => String(r.status ?? "") === "completed").length;
  const live = keys.rows.filter((r) => r.revoked_at === null || r.revoked_at === undefined).length;

  return (
    <>
      {/* THE FIGURES FIRST. Five questions an operator has on arrival, each with the ceiling of its own read
          stated on the card rather than assumed away. */}
      <section className="panel" data-testid="panel-deployment" aria-labelledby="panel-deployment-h">
        <div className="panel-head">
          <h2 id="panel-deployment-h">Right now</h2>
        </div>
        <div className="stat-grid">
          <Stat
            label="Waiting on you"
            testId="stat-approvals"
            tally={approvals}
            tone="attention"
            href="/approvals"
            meta={
              approvals.count === 0 ? (
                "No gated tool call is parked. Nothing is blocked on a decision."
              ) : (
                <a href="/approvals">Decide them on the Approvals screen</a>
              )
            }
          />
          <Stat
            label="Runs recorded"
            testId="stat-runs"
            tally={runs}
            href="/history"
            meta={
              runs.count === 0
                ? "This project has never started a run."
                : `${String(completed)} completed, ${String(runs.count - completed)} otherwise.`
            }
          />
          <Stat
            label="Agents"
            testId="stat-agents"
            tally={agents}
            href="/agents"
            meta={agents.count === 0 ? "Nothing can be pinned to a run yet." : "Lineages a run can be pinned to."}
          />
          <Stat
            label="Projects"
            testId="stat-projects"
            tally={projects}
            meta={projects.count === 1 ? "One project — the scope every screen here reads." : "Scopes this deployment holds."}
          />
          <Stat
            label="API keys"
            testId="stat-keys"
            tally={keys}
            href="/policy"
            meta={`${String(live)} live, ${String(keys.count - live)} revoked.`}
          />
        </div>
      </section>

      <Panel
        title="Organizations"
        testId="panel-organizations"
        fetchPath="/organizations"
        emptyNote="No organization. A bootstrap seeds one, so an empty list here means the control plane has not finished starting."
        columns={[
          {
            header: "Name",
            sort: (r) => String(r.display_name ?? r.id ?? ""),
            render: (r) => <NameCell name={String(r.display_name ?? "")} id={String(r.id ?? "")} />,
          },
        ]}
      />

      <Panel
        title="Projects"
        testId="panel-projects"
        fetchPath="/projects"
        note={
          <>
            A project is the scope a key reaches and a policy applies to. Its configuration is written on the{" "}
            <a href="/policy">Policy &amp; keys</a> screen.
          </>
        }
        emptyNote="No project. Create one from the CLI or the provisioning API — nothing on this console reaches a scope that does not exist."
        columns={[
          {
            header: "Name",
            sort: (r) => String(r.display_name ?? r.id ?? ""),
            render: (r) => <NameCell name={String(r.display_name ?? "")} id={String(r.id ?? "")} />,
          },
          { header: "Organization", sort: (r) => String(r.organization_id ?? ""), render: (r) => <code>{String(r.organization_id ?? "")}</code> },
        ]}
      />

      <Panel
        title="Secret refs"
        testId="panel-secret-refs"
        fetchPath="/secret-refs"
        note="METADATA ONLY. A secret VALUE is write-only server-side and never appears in any response."
        emptyNote="No secret ref. One is created by binding a provider key or by writing an environment value — never by this panel."
        columns={[
          { header: "Name", sort: (r) => String(r.name ?? ""), render: (r) => <code>{String(r.name ?? "")}</code> },
          { header: "Version", sort: (r) => Number(r.version ?? 0), render: (r) => <span className="num">{String(r.version ?? "")}</span> },
        ]}
      />

      {/* The one PAGINATED collection on this surface: api/pagination.go serves agents through renderPage, so
          this panel is where truncation is visible rather than assumed (E25 T2, plan §3.6 D18). */}
      <Panel
        title="Agents"
        testId="panel-agents"
        fetchPath="/agents"
        note={
          <>
            One row per lineage. Revisions, drafts and publishing are on the <a href="/agents">Agents</a> screen.
          </>
        }
        emptyNote="No agent. A run with no pinned revision still works — an agent is what makes one reproducible."
        columns={[
          {
            header: "Name",
            sort: (r) => String(r.name ?? r.id ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id={String(r.id ?? "")} />,
          },
        ]}
      />

      {/* THE STANDING CAVEAT, AT THE BOTTOM, WHICH IS WHERE A STANDING CAVEAT BELONGS. It was the first thing
          on this page — three sentences of permanent background above every number and every list. It has not
          been softened or shortened; it has been moved to where a reader meets it after the screen has
          answered something. */}
      <p className="muted" data-testid="ceiling-note">
        Open-core console — <strong>public API only</strong>. This is not a commercial SaaS UI: no billing
        or team management (§5). The operator console (cordon/drain/queue-inspect, §47.4) lives in the E15
        ops CLIs, and config explainability shows the effective value only (layer-by-layer attribution is a
        later iteration, §47.3).
      </p>
    </>
  );
}
