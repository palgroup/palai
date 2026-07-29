package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// The HTTP half of the generic approval gate (E23 T9). It is the adapter between api.ApprovalAPI and the
// coordinator, and it is deliberately thin in both directions:
//
//	GET  /v1/approvals                       → coordinator.PendingToolApprovals + the SHARED screen
//	POST /v1/approvals/{id}/approve|deny     → coordinator.DecideToolApproval  — the single throat
//
// TWO THINGS IT MUST NOT DO, and both are the reason the file is this short. It must not reimplement any
// part of a decision: DecideToolApproval applies the approver policy, checks the deadline, checks the
// one-shot hash, transitions the call, journals it and WAKES the parked run in ONE transaction, and a
// surface that did any of that itself would be a second, weaker path to the same place. And it must not
// build the approval screen a second way: it derives the display from the SAME function the Slack message
// and the Slack modal derive theirs from, so the two surfaces cannot disagree about what a human approved.

// approvalDisplayFor is the ONE derivation, reached from here rather than duplicated. It is
// slack.DeriveApprovalDisplay — the same body execution.DeriveToolApprovalDisplay is a one-line delegation
// to (see apps/control-plane/internal/execution/approval_display.go). This package cannot call through the
// execution alias because `execution` imports `store`, so naming the implementation directly is what keeps
// there being exactly ONE of it rather than a copy on this side of the edge.
var approvalDisplayFor = slack.DeriveApprovalDisplay

// ListPendingApprovals returns the caller's project's open gated tool calls, oldest first.
//
// THE OPERATOR'S LABEL IS RESOLVED THROUGH THE SAME LOOKUP THE EXECUTOR RESOLVES THE CALL THROUGH, per
// approval row, exactly as the Slack poster resolves it (extensions/slack_approval_post.go). It is not
// read from a stored copy, because a stored display is a second copy of the truth and a second copy can
// drift — a re-published revision would leave a stale sentence on the one screen whose whole job is to be
// read exactly. A tool that no longer resolves renders as "(no operator label)" rather than dropping the
// row: the ARGUMENTS are the authority on this screen and they came off the ledger.
func (s *Store) ListPendingApprovals(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.PendingApproval, error) {
	tenant := tenantOf(scope)
	window := coordinator.ToolApprovalWindow{
		CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit,
	}
	if q.After != nil {
		window.AfterCreatedAt, window.AfterID = &q.After.CreatedAt, q.After.ID
	}
	parked, err := s.spine.PendingToolApprovals(ctx, tenant, window)
	if err != nil {
		return nil, err
	}
	out := make([]api.PendingApproval, 0, len(parked))
	for _, p := range parked {
		label := ""
		if tool, ok, lerr := s.tools.LookupTool(ctx, tenant.Organization, tenant.Project, p.RunID, p.ToolName); lerr == nil && ok {
			label = tool.ApprovalLabel
		}
		display := approvalDisplayFor(p.ToolName, label, p.Arguments)
		out = append(out, api.PendingApproval{
			ID: p.ApprovalID, Object: "approval", ToolCallID: p.ToolCallID, RunID: p.RunID,
			SessionID: p.SessionID, ResponseID: p.ResponseID, RequestHash: p.RequestHash,
			ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt,
			Identity: display.Identity, OperatorLabel: display.OperatorLabel,
			Arguments: display.Arguments, Truncated: display.Truncated,
		})
	}
	return out, nil
}

// DecideApproval applies one HTTP caller's answer through coordinator.DecideToolApproval.
//
// WHO IS DECIDING is `key:<api_key_id>` — coordinator.ApproverPrincipal's HTTP form, the same one the
// native command surface stamps (AcceptCommand). It names the KEY rather than the principal behind it
// because api_keys.principal_id carries no UNIQUE constraint, so several keys may share one principal and
// an operator who revokes a key expects to have revoked what that key could approve.
//
// THE HONEST CEILING, and it is the platform's rather than this surface's: PALAI HAS NO USER IDENTITY
// (known-gaps-1.0.md HIL-P2). A bearer principal is an API key, not a person, and nothing links a Slack
// account to a key. So "two humans approved" is not expressible here and this task does not invent it —
// a principal that could not be resolved renders empty, and ConfigPolicy.ApproverAllowed refuses an empty
// principal against any configured list.
//
// The approval id is resolved FIRST, within the tenant, so an unknown or foreign id refuses before any
// decision is attempted and the two are indistinguishable to the caller.
func (s *Store) DecideApproval(ctx context.Context, scope middleware.Scope, approvalID string, d api.ApprovalDecision) (api.ApprovalOutcome, error) {
	tenant := tenantOf(scope)
	parked, found, err := s.spine.ToolApprovalByID(ctx, tenant, approvalID)
	if err != nil {
		return api.ApprovalOutcome{}, fmt.Errorf("read the approval to decide: %w", err)
	}
	if !found {
		return api.ApprovalOutcome{}, nil
	}
	applied, err := s.spine.DecideToolApproval(ctx, tenant, coordinator.ToolApprovalDecision{
		ToolCallID:  parked.ToolCallID,
		RequestHash: d.RequestHash,
		DecidedBy:   coordinator.ApproverPrincipal(coordinator.ApproverSurfaceKey, "", scope.APIKeyID),
		Approve:     d.Approve,
		Reason:      d.Reason,
	})
	switch {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		return api.ApprovalOutcome{Found: true, Unauthorized: true}, nil
	case err != nil:
		return api.ApprovalOutcome{}, fmt.Errorf("apply the approval decision: %w", err)
	}
	return api.ApprovalOutcome{Found: true, Applied: applied}, nil
}
