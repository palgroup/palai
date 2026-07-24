"use client";

import { useState } from "react";

import { apiSend, RelayError } from "@/lib/api";

export interface PendingApproval {
  id: string | null;
  action: string | null;
  args: unknown;
  diff: string | null;
  destination: string | null;
  risk: string | null;
  expires_at: string | null;
  model_summary: string | null;
}

// ApprovalPanel is the EXACT approval UI (UI-002, §47.2). It renders the AUTHORITATIVE, model-independent
// detail — action, args, diff, destination, risk, expiry — as the primary content, each with its own test
// id. The model's own summary is shown in a SEPARATE, explicitly-labelled region and is NEVER allowed to
// substitute for the authoritative detail: approving "yes" to a summary is not approving the operation;
// the operator approves the exact action/args/diff/destination they see here.
//
// Approve/deny POST a durable command (kind=approve|deny) to /v1/sessions/{id}/commands through the relay.
export function ApprovalPanel({
  approval,
  sessionId,
  onResolved,
}: {
  approval: PendingApproval;
  sessionId: string;
  onResolved: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function resolve(kind: "approve" | "deny") {
    setBusy(true);
    setError(null);
    try {
      await apiSend("POST", `/sessions/${encodeURIComponent(sessionId)}/commands`, {
        command_id: `cmd_${kind}_${approval.id ?? "unknown"}`,
        kind,
      });
      onResolved();
    } catch (err) {
      setError(err instanceof RelayError ? err.problem.detail : "command failed");
      setBusy(false);
    }
  }

  const highRisk = (approval.risk ?? "").toLowerCase().includes("high");

  return (
    <section className="panel" data-testid="approval-panel" aria-labelledby="approval-h" role="region">
      <h2 id="approval-h">Approval required</h2>

      {/* AUTHORITATIVE DETAIL — the operator approves THIS, not the summary. */}
      <fieldset data-testid="approval-authoritative">
        <legend>Authoritative request detail</legend>
        <dl>
          <dt>Action</dt>
          <dd data-testid="approval-action">{approval.action ?? "—"}</dd>
          <dt>Destination</dt>
          <dd data-testid="approval-destination">{approval.destination ?? "—"}</dd>
          <dt>
            Risk
          </dt>
          <dd data-testid="approval-risk" className={highRisk ? "risk-high" : undefined}>
            {approval.risk ?? "—"}
            {highRisk ? " (high — a bare “yes” is not sufficient)" : ""}
          </dd>
          <dt>Expires</dt>
          <dd data-testid="approval-expiry">{approval.expires_at ?? "—"}</dd>
          <dt>Arguments</dt>
          <dd>
            <pre className="code" data-testid="approval-args">
              {approval.args === null || approval.args === undefined ? "—" : JSON.stringify(approval.args, null, 2)}
            </pre>
          </dd>
          <dt>Diff</dt>
          <dd>
            <pre className="code" data-testid="approval-diff">
              {approval.diff ?? "—"}
            </pre>
          </dd>
        </dl>
      </fieldset>

      {/* The model's summary — clearly secondary, NEVER a substitute for the detail above. */}
      {approval.model_summary ? (
        <p className="muted" data-testid="approval-model-summary">
          <strong>Model summary (not authoritative):</strong> {approval.model_summary}
        </p>
      ) : null}

      {error ? (
        <p role="alert" data-testid="approval-error">
          Error: {error}
        </p>
      ) : null}

      <div>
        <button type="button" data-testid="approve-button" disabled={busy} onClick={() => resolve("approve")}>
          Approve this exact action
        </button>{" "}
        <button type="button" data-testid="deny-button" disabled={busy} onClick={() => resolve("deny")}>
          Deny
        </button>
      </div>
    </section>
  );
}
