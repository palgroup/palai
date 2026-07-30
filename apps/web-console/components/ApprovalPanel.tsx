"use client";

import { useState } from "react";

import { apiSend, RelayError } from "@/lib/api";

export interface PendingApproval {
  id: string | null;
  operation: string | null;
  branch: string | null;
  request_hash: string | null;
  display: string | null;
}

// ApprovalPanel is the EXACT approval UI (UI-002, §47.2). It renders the AUTHORITATIVE, model-independent
// detail the canonical approval.requested.v1 event ACTUALLY carries (packages/coordinator/publication.go:
// operation, branch, request_hash) as the primary content, each with its own test id. The proposal-supplied
// `display` string is shown in a SEPARATE, explicitly-labelled region and is NEVER allowed to substitute for
// the authoritative detail: approving "yes" to a soothing display is not approving the operation; the
// operator approves the exact operation/branch (bound by request_hash) they see here.
//
// Richer detail (action/args/diff/destination/risk/expiry) is NOT on the event today and there is no
// publications/approvals READ endpoint — it is a filed public-API GAP (README); the console renders those
// fields when the API emits them. Approve/deny POST a durable command (kind=approve|deny) to
// /v1/sessions/{id}/commands through the relay.
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

  return (
    <section className="panel" data-testid="approval-panel" aria-labelledby="approval-h" role="region">
      <h2 id="approval-h">Approval required</h2>

      {/* AUTHORITATIVE DETAIL — the operator approves THIS exact operation, not the display string. These are
          the only detail fields the approval.requested.v1 event carries today (publication.go). */}
      <fieldset data-testid="approval-authoritative">
        <legend>Authoritative request detail</legend>
        <dl>
          <dt>Operation</dt>
          <dd data-testid="approval-operation">{approval.operation ?? "—"}</dd>
          <dt>Branch</dt>
          <dd data-testid="approval-branch">{approval.branch ?? "—"}</dd>
          <dt>Request hash</dt>
          <dd>
            <code data-testid="approval-request-hash">{approval.request_hash ?? "—"}</code>
          </dd>
        </dl>
      </fieldset>

      {/* The proposal-supplied display string — clearly secondary, NEVER a substitute for the detail above. */}
      {approval.display ? (
        <p className="muted" data-testid="approval-display">
          <strong>Proposal summary (not authoritative):</strong> {approval.display}
        </p>
      ) : null}

      {error ? (
        <p role="alert" className="form-error" data-testid="approval-error">
          Error: {error}
        </p>
      ) : null}

      <div>
        <button type="button" className="primary" data-testid="approve-button" disabled={busy} onClick={() => resolve("approve")}>
          Approve this exact operation
        </button>{" "}
        <button type="button" data-testid="deny-button" disabled={busy} onClick={() => resolve("deny")}>
          Deny
        </button>
      </div>
    </section>
  );
}
