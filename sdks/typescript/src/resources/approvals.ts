import type { Palai } from "../client.ts";
import { callArgs, enc, listPath, type CallOptions, type ListParams, type Page } from "./shared.ts";

// THE HUMAN-IN-THE-LOOP SURFACE, from the SDK.
//
// ‼️ THE ROUTES SHIPPED AND THE SDK COULD NOT REACH THEM. `GET /v1/approvals`,
// `POST /v1/approvals/{id}/approve` and `POST /v1/approvals/{id}/deny` have existed since the tool-approval
// phase; `grep -rn approval sdks/typescript/src` returned NOTHING outside the generated types. So a program
// driving Palai could start a run that parks on an approval and then had no way to answer it — the run sat
// `in_progress` until its TTL expired, and the only surfaces that could decide were Slack and curl.
//
// THREE ANSWERS, and they are deliberately not one call with a flag. Approving and denying are different
// URLs server-side for the reason pause/resume are, and a denial carries a REASON that rides back to the
// model verbatim: a denial with no reason is a wall, a denial with one is an instruction the agent can act
// on. Standing authorization is a third thing again and lives on the SESSION (`sessions.setAutoApprove`),
// because it is a decision about a sitting rather than about one call.

// PendingApproval is one parked call as the approvals list renders it. The fields a caller needs to DECIDE
// are the id and the request hash; the rest is what a screen shows a human.
export interface PendingApproval {
  id: string;
  object: string;
  tool_call_id: string;
  run_id: string;
  session_id: string;
  response_id?: string;
  // request_hash is the ONE-SHOT BINDING and every decision must carry it back. It is the hash of the
  // arguments as the model asked for them, so arguments changed after a human looked leave the approval
  // they granted behind — see ApproveParams.requestHash.
  request_hash: string;
  expires_at?: string | null;
  created_at: string;
  [key: string]: unknown;
}

// ApprovalDecisionResult is the server's acknowledgement: which way it went.
export interface ApprovalDecisionResult {
  id: string;
  object: string;
  decision: string;
}

export interface ApproveParams {
  // requestHash is REQUIRED, and the requirement is the product rather than ceremony. The control plane
  // refuses a decision that does not carry it, so that a decision keyed only by an approval id cannot
  // become the one surface where the one-shot binding does not hold. Take it from the PendingApproval.
  requestHash: string;
}

export interface DenyParams extends ApproveParams {
  // reason rides back to the MODEL verbatim, so it is guidance and not bookkeeping: "not that file, use
  // the fixture under testdata" turns a refusal into the next instruction.
  reason?: string;
}

// Approvals is the /v1/approvals resource: read what is waiting, and answer it.
export class Approvals {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // list returns the tenant's pending approvals, newest first, with the shared cursor.
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<PendingApproval>> {
    const result = await this.#client.request<Page<PendingApproval>>(
      "GET",
      listPath("/v1/approvals", params),
      callArgs(options),
    );
    return result.body;
  }

  // approve lets the parked call run. Idempotent by the approval's own state machine: a second approve of
  // an already-approved row is the same answer, and one whose arguments changed is refused by the hash.
  approve(approvalID: string, params: ApproveParams, options: CallOptions = {}): Promise<ApprovalDecisionResult> {
    return this.#decide(approvalID, "approve", { request_hash: params.requestHash, reason: "" }, options);
  }

  // deny refuses it, with a reason the agent reads.
  deny(approvalID: string, params: DenyParams, options: CallOptions = {}): Promise<ApprovalDecisionResult> {
    return this.#decide(approvalID, "deny", { request_hash: params.requestHash, reason: params.reason ?? "" }, options);
  }

  async #decide(
    approvalID: string,
    verb: "approve" | "deny",
    body: { request_hash: string; reason: string },
    options: CallOptions,
  ): Promise<ApprovalDecisionResult> {
    const result = await this.#client.request<ApprovalDecisionResult>(
      "POST",
      `/v1/approvals/${enc(approvalID)}/${verb}`,
      {
        body,
        // The decision is keyed by the approval id and its request hash, and the row's state machine
        // settles once — so a connection torn after the server committed is safe to re-send.
        idempotent: true,
        ...callArgs(options),
      },
    );
    return result.body;
  }
}
