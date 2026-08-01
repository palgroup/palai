"use client";

import { NameCell, Panel } from "@/components/Panel";
import { CopyButton, shortId } from "@/components/Session";
import { Stat, useTally } from "@/components/Stat";
import { compactTokens } from "@/lib/sessions";

// THE OVERVIEW — "what is this deployment doing right now", answered in figures, with the collections that
// identify it underneath.
//
// THE REFERENCE THIS IS DRAWN AGAINST IS A BILLING DASHBOARD and we are not one. Its four headline cards are
// credits remaining, spend today, prompt-caching savings and token volume; three of the four are money and
// this deployment has NO cost surface at all — no price, no invoice, no adjustment entry anywhere in the
// metering tables (see /usage, which says so on the screen). Inventing a currency figure here would be the
// exact defect this tree keeps finding in its own code, so what is on the cards is what /v1 actually
// answers, and the report that shipped with this pass names the reference cards that have no counterpart.
//
// SIX FIGURES, FROM FIVE ROUTES, EVERY ONE OF THEM READ AND NOT DERIVED:
//   GET /v1/responses  running work (the runs that have not reached a terminal state)
//   GET /v1/approvals  what is waiting on a human — the one `attention` card, because a dashboard where
//                      four cards are highlighted has highlighted nothing
//   GET /v1/sessions   the conversations those runs belong to
//   GET /v1/runners    fleet health, as STATES rather than as a health word the API does not carry
//   GET /v1/usage      token volume, summed over the two model meters coordinator/usage.go actually names
//   GET /v1/agents     the lineages a run can be pinned to
//
// "SPENT TODAY" IS ABSENT AND THAT IS A MEASUREMENT, NOT AN OMISSION. GET /v1/usage takes NO time window —
// metering/store.go's summaryView answers lifetime totals for the scope and the route parses no bounds — so
// a card labelled "today" would be a lifetime figure with a false label. The LEDGER is windowed
// (?created_after / ?created_before) and the totals are not, and until the summary gains a window the honest
// card is the lifetime one.
//
// FOUR COLLECTIONS STAY ON THIS PATH BECAUSE SHIPPED SPECS PIN THEM HERE, and that is recorded rather than
// quietly worked around: tests/public-api-only.spec.ts reads `panel-organizations` and `panel-secret-refs`
// on "/", tests/pagination.spec.ts drives `panel-agents` here, and tests/profile.ts's sign-in lands on
// `ceiling-note`. Three of those are files this pass must not weaken. They are not passengers either — the
// organisations and projects ARE the scope every other screen reads, the agent list is the deployment's unit
// of work, and secret refs are the one collection whose metadata-only projection is worth seeing beside the
// keys that use it. Model connections, model routes and knowledge bases, which no spec pins, are on
// /registry.
//
// EVERY READ RIDES THE RELAY (browser → /api/palai/v1/* → upstream /v1/*), so the public-API-only intercept
// still sees the whole admin surface ride it. Secret-ref VALUES are never rendered — metadata only.
//
// AND THERE IS NO "Created" COLUMN ON THE THREE COLLECTIONS BELOW, which was a column drafted and then
// deleted. `organizationView`, `projectView` and `apiKeyView` all declare `CreatedAt *time.Time` with
// `json:"created_at,omitempty"` (apps/control-plane/internal/identity/store.go), so the stamp is ABSENT from
// the wire whenever the pointer is nil — and it is nil on every row the deterministic fixture serves.
// Rendering three columns of em dashes teaches a reader that this screen is broken, which is a worse lie
// than a column that is not there. /registry keeps its Created column because those three projections write
// the field unconditionally.

/** IdCell is the reference console's identity cell: a readable prefix, the whole value in `title` and on the
 *  clipboard. A 36-character hash printed whole is a value an operator can neither read nor reliably select,
 *  and that is what every table on this page used to render. */
function IdCell({ id, label }: { id: string; label: string }) {
  if (id === "") return <span className="cell-none">— none</span>;
  return (
    <span className="cell-id-group">
      <code className="cell-id" title={id}>
        {shortId(id)}
      </code>
      <CopyButton value={id} label={label} testId={`copy-${id}`} />
    </span>
  );
}

/** RUNNING is the set of run states that are not an ending. api/responses.go's lifecycle words: a run is
 *  queued, then provisioning, then running, then one of completed / failed / cancelled / expired. Counting
 *  "not completed" would have called a FAILED run live, which is the opposite of what this card is for. */
const RUNNING = new Set(["queued", "provisioning", "running", "in_progress", "requires_action"]);

export default function OverviewPage() {
  const approvals = useTally("/approvals");
  const runs = useTally("/responses");
  const sessions = useTally("/sessions");
  const runners = useTally("/runners");
  const agents = useTally("/agents");
  // The meters are not a list envelope — metering/store.go's summaryView carries them under `meters` — which
  // is the reason useTally takes a selector at all.
  const usage = useTally("/usage", (body) => (Array.isArray(body.meters) ? (body.meters as Record<string, unknown>[]) : []));

  const live = runs.rows.filter((r) => RUNNING.has(String(r.status ?? r.state ?? ""))).length;
  const failed = runs.rows.filter((r) => /fail|cancel|expire/.test(String(r.status ?? r.state ?? ""))).length;
  const byState = (state: string) => runners.rows.filter((r) => String(r.state ?? "") === state).length;
  const pending = byState("pending");
  const cordoned = byState("cordoned");
  const activeRunners = byState("active");
  // The two model meters, summed. coordinator/usage.go names them `model.input_tokens` and
  // `model.output_tokens`; `run.admitted` is a COUNT OF RUNS in the same list and adding it to a token total
  // would be adding two units together.
  const meter = (name: string) => usage.rows.filter((r) => String(r.meter ?? "") === name).reduce((n, r) => n + Number(r.quantity ?? 0), 0);
  const tokensIn = meter("model.input_tokens");
  const tokensOut = meter("model.output_tokens");

  return (
    <>
      {/* THE FIGURES FIRST, at the weight the reference gives them. Each card states the ceiling of its own
          read rather than assuming it away — a paginated collection is a FLOOR, an unreachable one is an em
          dash and never a zero. components/Stat.tsx is where those three cases live. */}
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
          {/* THE FIGURE IS WHAT THE LABEL SAYS. This card counted every run ever recorded and called itself
              "Runs" — a number that only ever rises and answers nothing an operator opens a console to ask.
              `atLeast` is carried through unchanged: over a collection the server cut at twenty, the runs
              this page can see are a FLOOR, and Stat says so in place of the meta rather than printing a
              flat figure beside a list that was truncated. */}
          <Stat
            label="Runs in flight"
            testId="stat-runs"
            tally={{ ...runs, count: live }}
            href="/history"
            meta={
              runs.count === 0
                ? "This project has never started a run."
                : live === 0
                  ? `Nothing is running. ${String(runs.count)} recorded${failed === 0 ? "" : `, ${String(failed)} of them ended badly`}.`
                  : `${String(runs.count)} recorded in this project.`
            }
          />
          <Stat
            label="Sessions"
            testId="stat-sessions"
            tally={sessions}
            href="/sessions"
            meta={sessions.count === 0 ? "Nothing has been asked of this deployment yet." : "Conversations every run belongs to."}
          />
          <Stat
            label="Machines enrolled"
            testId="stat-runners"
            tally={runners}
            href="/fleet"
            meta={
              runners.count === 0
                ? "No machine has enrolled. Runs are placed by the control plane's own workers."
                : // THE STATES, NOT A HEALTH WORD. api/runners.go carries no `healthy` field and nothing
                  // expires a row, so the only honest breakdown is the lifecycle the rows actually hold.
                  `${String(activeRunners)} active, ${String(cordoned)} cordoned, ${String(pending)} awaiting admission.`
            }
          />
          <Stat
            label="Agents"
            testId="stat-agents"
            tally={agents}
            href="/agents"
            meta={agents.count === 0 ? "Nothing can be pinned to a run yet." : "Lineages a run can be pinned to."}
          />
          {/* THE TOKEN CARD IS NOT A useTally COUNT — it is a SUM over the rows, so the figure is written
              here rather than taken from `tally.count`. Stat renders `count`, so the tally handed to it is
              the read's own state (loading / failed / ok) with the summed figure in `count`. */}
          <Stat
            label="Tokens in / out"
            testId="stat-tokens"
            tally={{ ...usage, count: tokensIn + tokensOut, atLeast: false }}
            href="/usage"
            meta={
              tokensIn + tokensOut === 0
                ? "No model step has settled a token in this scope, for all time."
                : `${compactTokens(tokensIn)} in, ${compactTokens(tokensOut)} out — all time, because GET /v1/usage takes no window.`
            }
          />
        </div>
      </section>

      {/* THE COLLECTIONS THAT IDENTIFY THE DEPLOYMENT, under the figures and at a lower weight. Each is a
          real table now: an id you can read and copy, the attribute that names the row, and the stamp that
          says how old it is. */}
      <Panel
        title="Organizations"
        testId="panel-organizations"
        fetchPath="/organizations"
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="organization ID" /> },
          {
            header: "Name",
            sort: (r) => String(r.display_name ?? ""),
            render: (r) => <NameCell name={String(r.display_name ?? "")} id="" />,
          },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No organization</p>
            <p className="empty-body">
              An organization is the outermost scope this deployment holds. A bootstrap seeds one, so an
              empty list here means the control plane has not finished starting.
            </p>
          </>
        }
      />

      <Panel
        title="Projects"
        testId="panel-projects"
        fetchPath="/projects"
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="project ID" /> },
          {
            header: "Name",
            sort: (r) => String(r.display_name ?? ""),
            // THE NAME IS A LINK, and /policy is where it goes because that is the one screen in this
            // console that takes a project as a PARAMETER — it opens on whichever project is named in the
            // query string, which is the pattern app/sessions/[id]/page.tsx landed.
            render: (r) => (
              <a className="cell-name-link" href={`/policy?project=${encodeURIComponent(String(r.id ?? ""))}`}>
                <NameCell name={String(r.display_name ?? "")} id="" />
              </a>
            ),
          },
          { header: "Organization", sort: (r) => String(r.organization_id ?? ""), render: (r) => <code>{String(r.organization_id ?? "")}</code> },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No project</p>
            <p className="empty-body">
              A project is the scope a key reaches and a policy applies to. Create one from the CLI or the
              provisioning API — nothing on this console reaches a scope that does not exist.
            </p>
          </>
        }
      />

      <Panel
        title="Secret refs"
        testId="panel-secret-refs"
        fetchPath="/secret-refs"
        // THE ONE NOTE ON THIS PAGE, and it is the only claim here a column cannot carry. It also pins the
        // shape tests/public-api-only.spec.ts asserts on the real profile: the header row must be exactly
        // Name and Version, because a third column on THIS collection would be a value.
        note="Metadata only. A secret VALUE is write-only server-side and appears in no response, this console's included."
        columns={[
          { header: "Name", sort: (r) => String(r.name ?? ""), render: (r) => <code>{String(r.name ?? "")}</code> },
          { header: "Version", sort: (r) => Number(r.version ?? 0), numeric: true, render: (r) => String(r.version ?? "") },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No secret refs yet</p>
            <p className="empty-body">
              A secret ref is a NAME a credential is stored under. One is created by binding a provider key
              or by writing an environment value — never by this screen.
            </p>
          </>
        }
      />

      {/* The one PAGINATED collection on this surface: api/pagination.go serves agents through renderPage, so
          this panel is where truncation is visible rather than assumed (E25 T2, plan §3.6 D18). */}
      <Panel
        title="Agents"
        testId="panel-agents"
        fetchPath="/agents"
        columns={[
          // FIRST CELL, AND IT MUST STAY UNIQUE PER ROW: tests/pagination.spec.ts reads
          // `tbody tr td:first-child` and asserts the twenty are distinct and that a continuation appends
          // rows the first page did not have. The id is what carries that; the copy button rides beside it.
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="agent ID" /> },
          {
            header: "Name",
            sort: (r) => String(r.name ?? ""),
            render: (r) => (
              <a className="cell-name-link" href="/agents">
                <NameCell name={String(r.name ?? "")} id="" />
              </a>
            ),
          },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No agents yet</p>
            <p className="empty-body">
              An agent is a name with a lineage of immutable revisions. A run with no pinned revision still
              works — an agent is what makes one reproducible.
            </p>
          </>
        }
      />

      {/* THE STANDING CAVEAT, AT THE BOTTOM, WHICH IS WHERE A STANDING CAVEAT BELONGS. It was the first thing
          on this page for three epics — three sentences of permanent background above every number and every
          list. It has not been softened or shortened; it is where a reader meets it after the screen has
          answered something. tests/profile.ts's sign-in waits for this testid, so it stays VISIBLE rather
          than collapsing into a details element. */}
      <p className="muted" data-testid="ceiling-note">
        Open-core console — <strong>public API only</strong>. This is not a commercial SaaS UI: no billing
        or team management (§5). The operator console (cordon/drain/queue-inspect, §47.4) lives in the E15
        ops CLIs, and config explainability shows the effective value only (layer-by-layer attribution is a
        later iteration, §47.3).
      </p>
    </>
  );
}
