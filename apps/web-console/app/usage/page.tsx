"use client";

import { Panel } from "@/components/Panel";
import { shortId, Stamp } from "@/components/Session";
import { compactTokens } from "@/lib/sessions";

interface MeterRow extends Record<string, unknown> {
  meter: string;
  unit: string;
  quantity: number;
  entries: number;
}

interface LedgerRow extends Record<string, unknown> {
  id: string;
  meter: string;
  quantity: number;
  unit: string;
  session_id?: string;
  run_id?: string;
  occurred_at: string;
}

interface BudgetRow extends Record<string, unknown> {
  meter_prefix: string;
  limit_quantity: number;
  period_start: string;
  updated_at: string;
}

interface QuotaRow extends Record<string, unknown> {
  meter_prefix: string;
  limit_quantity: number;
  window_seconds: number;
  updated_at: string;
}

// O4 + O5 — WHAT HAS BEEN SPENT, AND WHAT CAPS IT (feature list §7, plan §T8). Four routes: GET /v1/usage
// (per-meter totals for the caller's scope), GET /v1/usage/ledger (the raw settled rows an external exporter
// reads), GET /v1/budgets and GET /v1/quotas (the limits admission enforces).
//
// WHAT THIS PASS CHANGED, AND IT WAS MEASURED FIRST (2026-07-31, `PROSE MEASURE /usage`): 1140 characters of
// paragraph over 1641 characters of page — the four notes were longer than the four tables. Each is now ONE
// sentence saying what the table IS; the paragraphs that explained the metering model moved to
// docs/operations/console.md §3d, which is where an operator reads about a subsystem rather than about a
// screen.
//
// THREE COLUMN FIXES, ALL THE SAME FIX. `Quantity` used to render `40 token` INSIDE the numeric cell, so a
// column of figures never shared a decimal position and could not be compared by eye — the one rule this
// console took verbatim from a published design system, defeated by putting a word in the cell. The unit is
// its own column now and the figure is `numeric`. `Occurred` and `Updated` used to print a raw RFC3339
// string; they are Stamps, which is the relative form a reader wants with the exact one in the title.
//
// THE LEDGER'S SESSION IS A LINK, and it is the only row-to-detail link this screen can honestly offer: a
// settled entry carries `session_id` and `run_id`, and only the first has a screen. There is no
// /history/{id} route — lib/routes.ts records why — so `run_id` stays a copyable value rather than becoming
// a link to nothing.
//
// NO FILTER BOX, AND THAT IS AN ASSERTION RATHER THAN AN OVERSIGHT. tests/observability.spec.ts requires
// this page to render ZERO input elements: it is the READ half of the metering surface and the absence of a
// control is the claim. components/Panel.tsx only renders its toolbar past eight rows or when a caller
// passes `tools`, and nothing here passes any.
//
// AND ON A FRESH STACK ALL FOUR ARE EMPTY. That is the honest shape of this screen on the day it matters
// most, so each empty state is a TITLE plus a sentence saying what the thing is FOR (DIV-UI-002 measured how
// thin the real surface is; a console that answers thinness with whitespace is unreadable).
export default function UsagePage() {
  return (
    <>
      <Panel<MeterRow>
        title="Usage by meter"
        testId="panel-usage-meters"
        fetchPath="/usage"
        note="Totals for the tenant this console's key is scoped to — the scope comes from the verified identity, never from a query parameter."
        // The summary is not a list envelope: metering/store.go's summaryView carries the per-meter totals
        // under `meters`, alongside the limits they are measured against (rendered by the two panels below,
        // from their OWN routes — the ones O5 names).
        selectRows={(body) => (Array.isArray(body.meters) ? (body.meters as MeterRow[]) : [])}
        columns={[
          { header: "Meter", sort: (row) => row.meter, render: (row) => <code>{row.meter}</code> },
          {
            header: "Quantity",
            sort: (row) => row.quantity,
            numeric: true,
            // compactTokens, and not only for tokens: it keeps a small count exact and abbreviates past a
            // thousand, which is what a total that runs into six figures needs to stay readable in a column.
            // The exact figure is in the title, so nothing is rounded away.
            render: (row) => <span title={String(row.quantity)}>{compactTokens(row.quantity)}</span>,
          },
          { header: "Unit", sort: (row) => row.unit, render: (row) => row.unit },
          { header: "Settled entries", sort: (row) => row.entries, numeric: true, render: (row) => String(row.entries) },
        ]}
        emptyNote={
          <>
            <p className="empty-title">Nothing metered yet</p>
            {/* THE WORDS "has metered nothing" ARE LOAD-BEARING: tests/observability.spec.ts pins this
                empty state by that phrase, and it is the arm that runs on a real bootstrap stack. */}
            <p className="empty-body">
              This project has metered nothing so far. A meter appears here once a run settles usage — a
              model step settles input and output tokens against the run that spent them — so an empty table
              means no run has completed in this scope, not that metering is off.
            </p>
          </>
        }
      />

      <Panel<LedgerRow>
        title="Settled ledger"
        testId="panel-usage-ledger"
        fetchPath="/usage/ledger"
        note="The raw settled rows, newest first — the same rows an external billing exporter reads. Each is settled exactly once, so a redelivery adds nothing."
        columns={[
          { header: "Meter", sort: (row) => row.meter, render: (row) => <code>{row.meter}</code> },
          {
            header: "Quantity",
            sort: (row) => row.quantity,
            numeric: true,
            render: (row) => <span title={String(row.quantity)}>{compactTokens(row.quantity)}</span>,
          },
          { header: "Unit", sort: (row) => row.unit, render: (row) => row.unit },
          {
            header: "Session",
            sort: (row) => String(row.session_id ?? ""),
            render: (row) =>
              row.session_id === undefined || row.session_id === "" ? (
                <span className="cell-none">—</span>
              ) : (
                <a className="cell-id" href={`/sessions/${encodeURIComponent(row.session_id)}`} title={row.session_id}>
                  {shortId(row.session_id)}
                </a>
              ),
          },
          {
            header: "Run",
            sort: (row) => String(row.run_id ?? ""),
            render: (row) =>
              row.run_id === undefined || row.run_id === "" ? (
                <span className="cell-none">—</span>
              ) : (
                <code title={row.run_id}>{shortId(row.run_id)}</code>
              ),
          },
          { header: "Occurred", sort: (row) => row.occurred_at, render: (row) => <Stamp iso={row.occurred_at} /> },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No settled entries yet</p>
            <p className="empty-body">
              A zero-quantity fact is never written — settleUsage skips it — so a run whose model step
              reported no tokens leaves no row here at all. That is the usual reason a completed run appears
              in the history and nowhere in this table.
            </p>
          </>
        }
      />

      <Panel<BudgetRow>
        title="Budgets"
        testId="panel-budgets"
        fetchPath="/budgets"
        note="A cumulative spend cap on a meter prefix: admission refuses a run once settled usage since the period start reaches the limit."
        columns={[
          { header: "Meter prefix", sort: (row) => row.meter_prefix, render: (row) => <code>{row.meter_prefix}</code> },
          {
            header: "Limit",
            sort: (row) => row.limit_quantity,
            numeric: true,
            render: (row) => <span title={String(row.limit_quantity)}>{compactTokens(row.limit_quantity)}</span>,
          },
          { header: "Period start", sort: (row) => row.period_start, render: (row) => <Stamp iso={row.period_start} /> },
          { header: "Updated", sort: (row) => row.updated_at, render: (row) => <Stamp iso={row.updated_at} /> },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No budget caps this scope</p>
            <p className="empty-body">
              Nothing here limits cumulative spend, so a run is admitted whatever this project has already
              used. Setting one is a write this console does not offer: <code>POST /v1/budgets</code> is the
              only way today, and there is no CLI verb for it either.
            </p>
          </>
        }
      />

      <Panel<QuotaRow>
        title="Quotas"
        testId="panel-quotas"
        fetchPath="/quotas"
        note="A rate cap: a limit on a meter prefix within a rolling window."
        columns={[
          { header: "Meter prefix", sort: (row) => row.meter_prefix, render: (row) => <code>{row.meter_prefix}</code> },
          {
            header: "Limit",
            sort: (row) => row.limit_quantity,
            numeric: true,
            render: (row) => <span title={String(row.limit_quantity)}>{compactTokens(row.limit_quantity)}</span>,
          },
          { header: "Window", sort: (row) => row.window_seconds, numeric: true, render: (row) => `${String(row.window_seconds)} s` },
          { header: "Updated", sort: (row) => row.updated_at, render: (row) => <Stamp iso={row.updated_at} /> },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No quota caps this scope</p>
            <p className="empty-body">
              Nothing here limits the RATE of spend. Like a budget it is a write this console does not offer:{" "}
              <code>POST /v1/quotas</code> is the only way today, and there is no CLI verb for it either.
            </p>
          </>
        }
      />

      {/* NOT A FIFTH PANEL. Four panels of data plus a fifth panel of prose reads as five things of equal
          importance, and the fifth is not data — it is what the four do not do. */}
      <details className="notes" data-testid="usage-ceiling-notes">
        <summary>What this screen cannot do</summary>
        <p className="muted" data-testid="usage-ceiling">
          These four panels are a <strong>read</strong> surface. Nothing here sets, raises or removes a limit,
          and nothing here is a bill: the metering surface reports consumption and caps it, and carries no
          price, no invoice and no adjustment entry. A limit is set today by calling{" "}
          <code>POST /v1/budgets</code> or <code>POST /v1/quotas</code> with a key holding the{" "}
          <code>provision</code> capability — there is no CLI verb for either. The write half is E26, and{" "}
          <code>docs/operations/console.md</code> §3d says what a meter is and where its rows come from.
        </p>
      </details>
    </>
  );
}
