package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// THE SLACK-LESS APPROVAL SURFACE (E23 T9, spec §22.4, §26.7).
//
// WHY IT EXISTS, measured rather than assumed: E23 T8 wired a decision surface for gated tool calls and
// wired it to SLACK ONLY. `coordinator.DecideToolApproval` — the one throat every decision passes through
// — had exactly one production caller (internal/extensions/slack_decision.go), reachable only through
// POST /v1/slack/interactions, and no /v1/approvals route existed. So an operator running Palai in their
// own cloud WITHOUT Slack had a gate that parked the run, asked nobody, and let the expiry reaper release
// it half an hour later. It fails CLOSED, which is exactly why nobody noticed.
//
// THE DECISION GOES THROUGH DecideToolApproval AND NOTHING ELSE. That function applies the approver
// policy, journals, transitions the call and WAKES the parked run in ONE transaction; a second decision
// path that reimplemented any of it would be the defect this surface exists to prevent, so the store
// adapter behind ApprovalAPI is a thin caller and this handler is a thin caller of that.
//
// THE SCREEN IS NOT REBUILT HERE EITHER. The list projection comes from the same
// `execution.DeriveToolApprovalDisplay` (slack.DeriveApprovalDisplay) the Slack message and modal derive
// theirs from, so the HTTP surface and the Slack surface cannot disagree about what a human approved.

// approveScope is the capability a key must hold to see or decide this project's pending approvals. A key
// with an empty scope set (an admin/bootstrap key) holds it implicitly, the documented Scope.HasScope
// idiom, so no existing deployment's key loses anything.
//
// IT IS DELIBERATELY NOT `provision`, and the reason is a real bypass rather than a taxonomy preference:
// PATCH /v1/projects/{id} — the config_policy write path — is gated on `provision`, and config_policy is
// where `approvers` lives. A key that could provision could therefore add ITSELF to the approver list and
// then approve, which would make the E23 T2 policy self-grantable for every HTTP caller. A separate
// capability is what lets an operator mint a key that can ONLY decide approvals: it holds `approve`, it
// cannot rewrite the list it is checked against, and the two gates stay independent — exactly as a Slack
// connection's allowed_users is independent of config_policy.approvers.
const approveScope = "approve"

// PendingApproval is one open gated call as an operator reads it. The first six fields are the ledger's
// own; the last four ARE the approval screen, flattened, and they carry slack.ApprovalDisplay's json tags
// verbatim so the two surfaces render the same names for the same things.
//
// WHAT IS NOT HERE is as deliberate as what is: no MCP `description`, no server-supplied `title`, no model
// prose outside the arguments. The reasoning is stated once, at the derivation
// (adapters/integrations/slack/approval_display.go), rather than restated here where it could drift.
type PendingApproval struct {
	ID          string     `json:"id"`
	Object      string     `json:"object"`
	ToolCallID  string     `json:"tool_call_id"`
	RunID       string     `json:"run_id"`
	SessionID   string     `json:"session_id"`
	ResponseID  string     `json:"response_id,omitempty"`
	RequestHash string     `json:"request_hash"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`

	Identity      string `json:"identity"`
	OperatorLabel string `json:"operator_label"`
	Arguments     string `json:"arguments"`
	Truncated     bool   `json:"truncated"`

	// Kind names WHICH FAMILY this row is, and it is not decoration: the two are decided through
	// different throats and only one of them carries a tool call. A client that rendered both without
	// distinguishing them would show `tool_call_id: ""` on every publication and call it a tool.
	// "tool" | "publication".
	Kind string `json:"kind"`

	// The publication family's destination, empty on a tool row. THIS IS THE ONLY THING ON THE SCREEN
	// THAT SAYS WHERE THE WRITE IS GOING, and an operator approving a write to somebody's repository
	// cannot make that decision from an operation name alone. None of it is the model's: remote, branch
	// and base are resolved from the run's repository binding (RunPublicationTarget), which is why "open
	// the PR against dev" is an operator's configuration and not something a model can ask for.
	PublicationID string `json:"publication_id,omitempty"`
	Operation     string `json:"operation,omitempty"`
	Remote        string `json:"remote,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Base          string `json:"base,omitempty"`
	HeadSHA       string `json:"head_sha,omitempty"`
}

// ApprovalDecision is one HTTP caller's answer. There is deliberately NO approver field: the principal is
// stamped server-side from the verified key (the contracts.CommandCreateRequest rule, E23 T2), and the
// strict decode below turns an attempt to name one into a 400 rather than a silently ignored field.
type ApprovalDecision struct {
	// RequestHash is the one-shot binding, and it is REQUIRED. The Slack button carries
	// tool_calls.request_hash so that arguments changed after the approval leave no approval;
	// DecideToolApproval skips the comparison when the field is empty (the native command surface never
	// carried one), so a decision keyed only by an approval id would make this the ONE surface where the
	// binding does not hold. It is refused at this edge instead.
	RequestHash string `json:"request_hash"`
	// Reason rides a deny back to the model verbatim. A denial with no reason is a wall; a denial with one
	// is an instruction the agent can act on.
	Reason string `json:"reason"`
	// Approve is set by the route, never by the body — /approve and /deny are different URLs for the same
	// reason POST /v1/schedules/{id}/pause and /resume are.
	Approve bool `json:"-"`
}

// ApprovalOutcome is the store's answer to a decision. Every refusal is a TYPED outcome rather than an
// error, the SlackDecisionOutcome convention: an error means the receiver failed, while a refusal means
// the receiver worked and the request authorized nothing.
type ApprovalOutcome struct {
	// Found is false for an unknown id AND for another tenant's — indistinguishable on purpose, so this
	// surface cannot become an oracle for which approval ids exist elsewhere.
	Found bool
	// Applied reports that the decision actually moved the call.
	Applied bool
	// Unauthorized is the project's approver list refusing this principal (ErrApproverNotAuthorized). It is
	// separated from a merely VOID decision because "you may not decide this" and "this is no longer
	// decidable" are different facts for the operator reading the log — collapsing them would hide a
	// misconfigured approver list behind what looks like an expired button.
	Unauthorized bool
}

// ApprovalAPI is the store seam for the approval surface; *store.Store implements it. It takes the whole
// verified middleware.Scope rather than org/project strings because the DECIDING PRINCIPAL is derived from
// the key id on it (coordinator.ApproverPrincipal), and passing only the tenant would make it possible for
// a caller to supply an identity — the AcceptCommand posture, for the same reason.
type ApprovalAPI interface {
	// ListPendingApprovals returns one page, OLDEST first — these are questions, and the oldest is the one
	// closest to its deadline. It takes the whole resolved ListQuery rather than a limit, which is what that
	// type is for: every filter beginList PARSES is then honoured, so a client that passes ?created_after=
	// cannot be handed unfiltered rows (the review SHOULD-1 rule the ?status= refusal exists for).
	ListPendingApprovals(ctx context.Context, scope middleware.Scope, q ListQuery) ([]PendingApproval, error)
	DecideApproval(ctx context.Context, scope middleware.Scope, approvalID string, d ApprovalDecision) (ApprovalOutcome, error)
}

type approvalHandler struct{ approvals ApprovalAPI }

// authorize resolves the verified scope and enforces the approve capability.
func (h *approvalHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return middleware.Scope{}, false
	}
	if !scope.HasScope(approveScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the approve capability")
		return middleware.Scope{}, false
	}
	return scope, true
}

// list returns this project's open gated calls (GET /v1/approvals), in the shared keyset page envelope.
// A human cannot decide what they cannot see, and until this route the only place a gated call was ever
// shown was a Slack thread.
//
// READING IS GATED ON THE CAPABILITY, NOT ON THE APPROVER LIST, and that is a decision rather than an
// omission. `config_policy.approvers` is defined as who may DRIVE AN APPROVE/DENY TO A DECISION
// (ConfigPolicy.ApproverAllowed) — it is not a visibility list, and its Slack counterpart is not one
// either: the message is posted into a CHANNEL, where everyone in it reads the arguments and only the
// listed users may click. `approve` is this surface's channel. An operator who needs the arguments
// invisible to a principal withholds the capability from its key.
func (h *approvalHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	q, ok := beginList(w, r, "approval", scope)
	if !ok {
		return
	}
	// The WHOLE query goes down, with the +1 over-fetch renderPage reads as has_more. Passing the cursor
	// through is not a nicety: a list that mints a next_cursor and then resumes from nothing hands a client
	// page 1 forever, with has_more still true.
	page := q
	page.Limit = q.Limit + 1
	items, err := h.approvals.ListPendingApprovals(r.Context(), scope, page)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(items))
	for _, it := range items {
		body, err := json.Marshal(it)
		if err != nil {
			middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
			return
		}
		rows = append(rows, ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	renderPage(w, r, "approval", scope, rows, q.Limit)
}

func (h *approvalHandler) approve(w http.ResponseWriter, r *http.Request) { h.decide(w, r, true) }
func (h *approvalHandler) deny(w http.ResponseWriter, r *http.Request)    { h.decide(w, r, false) }

// decide applies one answer through coordinator.DecideToolApproval — the single throat — and renders the
// outcome. The status codes are the honest split between "you may not", "it is no longer decidable" and
// "it landed", because an operator scripting this surface has to be able to tell them apart.
func (h *approvalHandler) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	var in ApprovalDecision
	dec := json.NewDecoder(bytes.NewReader(raw))
	// STRICT: an unknown field is a 400, so a body cannot smuggle an `approver` and have it silently
	// ignored. The identity is the verified key's, stamped in the store adapter.
	dec.DisallowUnknownFields()
	if len(raw) > 0 {
		if err := dec.Decode(&in); err != nil {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not a valid approval decision")
			return
		}
	}
	if in.RequestHash == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"request_hash is required: an approval id alone authorizes nothing")
		return
	}
	in.Approve = approve

	out, err := h.approvals.DecideApproval(r.Context(), scope, r.PathValue("approval_id"), in)
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
	case !out.Found:
		// Unknown or foreign, indistinguishable. Never a 403 here — that would confirm it exists.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such approval")
	case out.Unauthorized:
		middleware.WriteProblem(w, r, http.StatusForbidden, "not_an_approver",
			"this principal is not in the project's approver list")
	case !out.Applied:
		// VOID, not failed: already decided, the deadline passed, or the one-shot hash no longer matches
		// the bytes. 409 rather than 200 because a script must not read "nothing happened" as success.
		middleware.WriteProblem(w, r, http.StatusConflict, "approval_not_decidable",
			"the approval was no longer decidable: it was already answered, its deadline passed, or the request hash does not match the queued call")
	default:
		decision := "approved"
		if !approve {
			decision = "denied"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": r.PathValue("approval_id"), "object": "approval.decision", "decision": decision,
		})
	}
}
