package palai

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Approvals is the /v1/approvals resource group (E23 T9, spec §22.4, §26.7): the Slack-less
// approval surface for a gated tool call or a gated publication. It mirrors Sessions's shape (a
// struct hanging off *Client, each method taking a trailing opts ...CallOption) rather than the
// brief's sketch of standalone Client methods — that sketch was Task 1's already-corrected error,
// not a fresh choice.
type Approvals struct{ client *Client }

// Approval is one open gated call as a human decides it
// (apps/control-plane/api/approvals.go's PendingApproval). Kind ("tool" | "publication") names
// which family a row belongs to: the publication-only fields (PublicationID, Operation, Remote,
// Branch, Base, HeadSHA, CredentialRef, Credential) are empty on a tool row, mirrored here exactly
// rather than split into two types so an unfamiliar Kind still decodes losslessly through Extra.
type Approval struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	ToolCallID  string `json:"tool_call_id"`
	RunID       string `json:"run_id"`
	SessionID   string `json:"session_id"`
	ResponseID  string `json:"response_id,omitempty"`
	RequestHash string `json:"request_hash"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	CreatedAt   string `json:"created_at"`

	Identity      string `json:"identity"`
	OperatorLabel string `json:"operator_label"`
	Arguments     string `json:"arguments"`
	Truncated     bool   `json:"truncated"`

	Kind string `json:"kind"`

	PublicationID string `json:"publication_id,omitempty"`
	Operation     string `json:"operation,omitempty"`
	Remote        string `json:"remote,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Base          string `json:"base,omitempty"`
	HeadSHA       string `json:"head_sha,omitempty"`

	CredentialRef string `json:"credential_ref,omitempty"`
	Credential    string `json:"credential,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (a *Approval) UnmarshalJSON(b []byte) error {
	type alias Approval
	var v alias
	if err := forwardUnmarshal(b, &v, &v.Extra); err != nil {
		return err
	}
	*a = Approval(v)
	return nil
}

func (a Approval) MarshalJSON() ([]byte, error) {
	type alias Approval
	return forwardMarshal(alias(a), a.Extra)
}

// ListApprovalsParams are the pagination + filters the list route actually honours. The shared
// parse (apps/control-plane/api/pagination.go's beginList) reads limit/after/created_after/
// created_before for every kind, but "approval" is NOT in statusFilterKinds — so a ?status= on this
// route is REJECTED with 400 invalid_request, not silently applied, and this type carries no Status
// field so a caller cannot construct that failure. A ?session_id= filter does not exist on this
// route at all (beginList never reads that key for any kind; only the separate usage-ledger handler
// does), so it is likewise omitted — an accepted-but-silently-ignored filter is the exact defect
// shape this tree already tracks.
type ListApprovalsParams struct {
	After         string
	Limit         int
	CreatedAfter  string
	CreatedBefore string
}

// approvalsListPath appends ListApprovalsParams to the list path, in the same fixed key order
// listPath (responses.go) uses for its overlapping keys. A zero/empty param adds no key.
func approvalsListPath(p ListApprovalsParams) string {
	var parts []string
	add := func(k, v string) { parts = append(parts, queryEscape(k)+"="+queryEscape(v)) }
	if p.After != "" {
		add("after", p.After)
	}
	if p.Limit != 0 {
		add("limit", strconv.Itoa(p.Limit))
	}
	if p.CreatedAfter != "" {
		add("created_after", p.CreatedAfter)
	}
	if p.CreatedBefore != "" {
		add("created_before", p.CreatedBefore)
	}
	if len(parts) == 0 {
		return "/v1/approvals"
	}
	return "/v1/approvals?" + strings.Join(parts, "&")
}

// DecisionParams is one HTTP caller's answer to a pending approval — a gated tool call or a gated
// publication (Approval.Kind). RequestHash is REQUIRED on both: the server's decide handler
// (apps/control-plane/api/approvals.go) refuses an empty one with 400 invalid_request before the
// decision ever reaches either throat — an approval id alone authorizes nothing, the decision must
// be bound to the exact call it was raised for.
//
// Reason only reaches the model on a TOOL denial: coordinator.DecideToolApproval writes it into the
// tool call's cancellation payload verbatim (packages/coordinator/approvals.go), so the model reads
// {"status":"denied","reason":...}. On a PUBLICATION denial it is dropped server-side —
// decidePublicationApproval's command payload carries only request_hash and approver
// (apps/control-plane/internal/store/approvals.go), and ApplyApprovalDecision takes no reason
// parameter at all (packages/coordinator/publication.go) — so a Reason set here is accepted by this
// SDK and by the server's strict decode, but never logged or delivered anywhere on that path. That
// is a pre-existing server gap, not this SDK's to paper over.
//
// There is deliberately no approver field — the body decode is strict (DisallowUnknownFields), and
// the deciding principal is stamped server-side from the verified API key.
type DecisionParams struct {
	RequestHash string `json:"request_hash"`
	Reason      string `json:"reason,omitempty"`
}

// ApprovalDecisionResult is the server's confirmation that a decision landed (200 on both the
// approve and deny routes).
type ApprovalDecisionResult struct {
	ID       string                     `json:"id"`
	Object   string                     `json:"object"`
	Decision string                     `json:"decision"`
	Extra    map[string]json.RawMessage `json:"-"`
}

func (r *ApprovalDecisionResult) UnmarshalJSON(b []byte) error {
	type alias ApprovalDecisionResult
	var v alias
	if err := forwardUnmarshal(b, &v, &v.Extra); err != nil {
		return err
	}
	*r = ApprovalDecisionResult(v)
	return nil
}

func (r ApprovalDecisionResult) MarshalJSON() ([]byte, error) {
	type alias ApprovalDecisionResult
	return forwardMarshal(alias(r), r.Extra)
}

// List returns this project's open gated calls, oldest first (GET /v1/approvals) — see
// ListApprovalsParams for exactly which filters the server honours.
func (a *Approvals) List(ctx context.Context, params ListApprovalsParams, opts ...CallOption) (*Page[Approval], error) {
	o := requestOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	var page Page[Approval]
	if err := a.client.doJSON(ctx, http.MethodGet, approvalsListPath(params), o, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// Approve decides a pending approval yes (POST /v1/approvals/{id}/approve). 403 not_an_approver
// surfaces the same way for both kinds (Approval.Kind). The other two codes DIFFER by kind, because
// the server dispatches on two different lookups (store.DecideApproval tries the tool lookup first,
// falls back to the publication one): ToolApprovalByID carries no state filter, so a TOOL approval
// that was already decided still resolves and this answers 409 approval_not_decidable (already
// answered, the deadline passed, or the request hash no longer matches). PublicationApprovalByID
// filters `p.state = 'pending_approval'` (storage/queries/publications.sql), so once a PUBLICATION
// is decided its row falls out of that lookup entirely and a repeat decide answers 404 "no such
// approval" instead — indistinguishable from an id that never existed.
//
// It is still marked idempotent for transport retry on both kinds: 409 and 404 are not retryable
// statuses (isRetryableStatus) and the request body is marshaled once, outside the retry loop, so a
// resend after a torn connection can never double-apply — worst case it surfaces as that kind's
// non-retried status if the first attempt actually landed, never a second decision.
func (a *Approvals) Approve(ctx context.Context, id string, p DecisionParams, opts ...CallOption) (*ApprovalDecisionResult, error) {
	return a.decide(ctx, id, "approve", p, opts)
}

// Deny decides a pending approval no (POST /v1/approvals/{id}/deny) — see Approve's doc comment for
// the 404-vs-409 split between a tool and a publication decided a second time. Reason only reaches
// the model on a TOOL denial; on a PUBLICATION denial it rides in this request but is dropped
// server-side (see DecisionParams).
func (a *Approvals) Deny(ctx context.Context, id string, p DecisionParams, opts ...CallOption) (*ApprovalDecisionResult, error) {
	return a.decide(ctx, id, "deny", p, opts)
}

func (a *Approvals) decide(ctx context.Context, id, verb string, p DecisionParams, opts []CallOption) (*ApprovalDecisionResult, error) {
	o := requestOptions{body: p, idempotent: true}
	for _, opt := range opts {
		opt(&o)
	}
	var out ApprovalDecisionResult
	path := "/v1/approvals/" + escapePathSegment(id) + "/" + verb
	if err := a.client.doJSON(ctx, http.MethodPost, path, o, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
