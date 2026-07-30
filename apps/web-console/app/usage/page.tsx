"use client";

import { Panel } from "@/components/Panel";

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

// O4 + O5 — WHAT HAS BEEN SPENT, AND WHAT CAPS IT (feature list §7, plan §T8). Four routes, all of them
// already mounted and none of them shown by this console before: GET /v1/usage (per-meter totals for the
// caller's scope), GET /v1/usage/ledger (the raw settled rows an external exporter reads), GET /v1/budgets
// and GET /v1/quotas (the limits admission enforces).
//
// THE READ HALF ONLY, AND THAT IS A SCOPE DECISION RATHER THAN AN OVERSIGHT. POST /v1/budgets and
// POST /v1/quotas exist, are gated on the `provision` capability this console's key holds, and the relay
// would forward either — so the absence of a form here is a choice, it is stated on the screen, and
// tests/observability.spec.ts asserts the absence rather than trusting it. The write half is E26.
//
// AND ON A FRESH STACK ALL FOUR ARE EMPTY. That is the honest shape of this screen on the day it matters
// most — the first one — so each panel says what its emptiness MEANS instead of rendering "None yet." over
// a region an operator cannot tell apart from a broken fetch (DIV-UI-002 measured how thin the real surface
// is; a console that answers thinness with whitespace is unreadable).
export default function UsagePage() {
  return (
    <>
      <Panel<MeterRow>
        title="Usage by meter"
        testId="panel-usage-meters"
        fetchPath="/usage"
        note="Totals for the tenant this console's key is scoped to. There is no organization or project selector, and that is deliberate: the scope comes from the verified identity, never from a query parameter that could name someone else's tenant."
        // The summary is not a list envelope: metering/store.go's summaryView carries the per-meter totals
        // under `meters`, alongside the limits they are measured against (rendered by the two panels below,
        // from their OWN routes — the ones O5 names).
        selectRows={(body) => (Array.isArray(body.meters) ? (body.meters as MeterRow[]) : [])}
        columns={[
          { header: "Meter", render: (row) => <code>{row.meter}</code> },
          { header: "Quantity", render: (row) => <span className="num">{`${String(row.quantity)} ${row.unit}`}</span> },
          { header: "Settled entries", render: (row) => <span className="num">{String(row.entries)}</span> },
        ]}
        emptyNote="This project has metered nothing yet. A meter appears here once a run settles usage — a model step settles input and output tokens against the run that spent them — so an empty table means no run has completed in this scope, not that metering is off."
      />

      <Panel<LedgerRow>
        title="Settled ledger"
        testId="panel-usage-ledger"
        fetchPath="/usage/ledger"
        note="The raw settled rows, newest first — the same rows an external billing exporter would read. Each is settled exactly once against the model request or run that produced it, so a redelivery adds nothing."
        columns={[
          { header: "Meter", render: (row) => <code>{row.meter}</code> },
          { header: "Quantity", render: (row) => <span className="num">{`${String(row.quantity)} ${row.unit}`}</span> },
          { header: "Run", render: (row) => (row.run_id === undefined || row.run_id === "" ? "—" : <code>{row.run_id}</code>) },
          { header: "Occurred", render: (row) => row.occurred_at },
        ]}
        emptyNote="The ledger holds no settled entries for this scope. A zero-quantity fact is never written — settleUsage skips it — so a run whose model step reported no tokens leaves no row here at all, which is the usual reason a completed run appears in the history and nowhere in this table."
      />

      <Panel<BudgetRow>
        title="Budgets"
        testId="panel-budgets"
        fetchPath="/budgets"
        note="A budget is a cumulative spend cap on a meter prefix. Admission refuses a run once settled usage since the period start reaches the limit."
        columns={[
          { header: "Meter prefix", render: (row) => <code>{row.meter_prefix}</code> },
          { header: "Limit", render: (row) => <span className="num">{String(row.limit_quantity)}</span> },
          { header: "Period start", render: (row) => row.period_start },
          { header: "Updated", render: (row) => row.updated_at },
        ]}
        emptyNote="No budget is set in this scope, so nothing here caps cumulative spend — a run is admitted whatever this project has already used. Setting one is a write, and this console has no control for it; POST /v1/budgets is the only way today, and there is no CLI verb for it either."
      />

      <Panel<QuotaRow>
        title="Quotas"
        testId="panel-quotas"
        fetchPath="/quotas"
        note="A quota is a rate cap: a limit on a meter prefix within a rolling window."
        columns={[
          { header: "Meter prefix", render: (row) => <code>{row.meter_prefix}</code> },
          { header: "Limit", render: (row) => <span className="num">{String(row.limit_quantity)}</span> },
          { header: "Window", render: (row) => <span className="num">{`${String(row.window_seconds)} s`}</span> },
          { header: "Updated", render: (row) => row.updated_at },
        ]}
        emptyNote="No quota is set in this scope, so nothing here caps the rate of spend. Like a budget it is a write this console does not offer; POST /v1/quotas is the only way today, and there is no CLI verb for it either."
      />

      {/* NOT A FIFTH PANEL. Four panels of data plus a fifth panel of prose reads as five things of equal
          importance, and the fifth is not data — it is what the four do not do. Same words, same testid,
          one level down. */}
      <details className="notes" data-testid="usage-ceiling-notes">
        <summary>What this screen cannot do</summary>
        <p className="muted" data-testid="usage-ceiling">
          These four panels are a <strong>read</strong> surface. Nothing here sets, raises or removes a limit,
          and nothing here is a bill: the metering surface reports consumption and caps it, and carries no
          price, no invoice and no adjustment entry. A limit is set today by calling{" "}
          <code>POST /v1/budgets</code> or <code>POST /v1/quotas</code> with a key holding the{" "}
          <code>provision</code> capability — there is no CLI verb for either.
        </p>
      </details>
    </>
  );
}
