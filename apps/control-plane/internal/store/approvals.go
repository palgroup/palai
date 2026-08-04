package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// The HTTP half of the generic approval gate (E23 T9). It is the adapter between api.ApprovalAPI and the
// coordinator, and it is deliberately thin in both directions:
//
//	GET  /v1/approvals                       → coordinator.PendingToolApprovals
//	                                         + coordinator.PendingPublicationApprovals, merged on one order
//	POST /v1/approvals/{id}/approve|deny     → coordinator.DecideToolApproval  — the tool throat
//	                                         → coordinator.ApplyApprovalDecision — the publication throat
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
	tenant, err := s.tenantOf(ctx, scope)
	if err != nil {
		return nil, err
	}
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
		if tool, ok, lerr := s.tools.LookupTool(ctx, tenant.Project, p.RunID, p.ToolName); lerr == nil && ok {
			label = tool.ApprovalLabel
		}
		display := approvalDisplayFor(p.ToolName, label, p.Arguments)
		out = append(out, api.PendingApproval{
			ID: p.ApprovalID, Object: "approval", Kind: "tool", ToolCallID: p.ToolCallID, RunID: p.RunID,
			SessionID: p.SessionID, ResponseID: p.ResponseID, RequestHash: p.RequestHash,
			ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt,
			Identity: display.Identity, OperatorLabel: display.OperatorLabel,
			Arguments: display.Arguments, Truncated: display.Truncated,
		})
	}

	// THE PUBLICATION FAMILY, which this list could not return at all before. The read above JOINs
	// tool_calls, so a push/pull-request/merge waiting on a human was invisible here while its row sat in
	// the table — measured 2026-08-01, and the console's own /approvals page told the operator to go watch
	// Live runs instead, which was the sentence that turned out to be untrue.
	pubs, err := s.spine.PendingPublicationApprovals(ctx, tenant, window)
	if err != nil {
		return nil, err
	}
	for _, p := range pubs {
		out = append(out, api.PendingApproval{
			ID: p.ApprovalID, Object: "approval", Kind: "publication", RunID: p.RunID,
			SessionID: p.SessionID, ResponseID: p.ResponseID, RequestHash: p.RequestHash,
			ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt,
			// The publication's OWN recorded sentence is the operator label; the destination fields below
			// are what it is a label FOR. There is no model prose here for the same reason the tool half
			// states at its derivation: nothing on this screen may be written by the thing being approved.
			Identity: p.Operation, OperatorLabel: p.Display, Arguments: string(p.Arguments),
			PublicationID: p.PublicationID, Operation: p.Operation,
			Remote: p.Remote, Branch: p.Branch, Base: p.Base, HeadSHA: p.HeadSHA,
			CredentialRef: p.ConnectionRef, Credential: credentialWord(p.ConnectionRef),
		})
	}

	// ONE PAGE, ONE ORDER. Both reads are ascending (created_at, id) and the caller's cursor is compared
	// against that same pair, so the merge has to restore it across the two families or a next_cursor
	// minted from the last row would skip every row of the other family with an earlier timestamp.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	// Each read was given the caller's limit, so the merge can hold up to twice it. Truncating here keeps
	// the +1 over-fetch the page envelope reads as has_more meaningful rather than doubled.
	if window.Limit > 0 && len(out) > window.Limit {
		out = out[:window.Limit]
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
	tenant, err := s.tenantOf(ctx, scope)
	if err != nil {
		return api.ApprovalOutcome{}, err
	}
	parked, found, err := s.spine.ToolApprovalByID(ctx, tenant, approvalID)
	if err != nil {
		return api.ApprovalOutcome{}, fmt.Errorf("read the approval to decide: %w", err)
	}
	if !found {
		// NOT-A-TOOL-APPROVAL IS NOT NOT-AN-APPROVAL. ToolApprovalByID joins tool_calls, so a publication
		// approval misses it and used to fall straight out of here as 404 "no such approval" — on a row
		// that exists, with the correct request_hash, measured 2026-08-01.
		return s.decidePublicationApproval(ctx, scope, tenant, approvalID, d)
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

// decidePublicationApproval applies an HTTP caller's answer to a PUBLICATION — a push, a pull request or
// a merge — and it exists because the surface above could accept one and never apply it.
//
// THE DEADLOCK IT CLOSES, measured 2026-08-01 rather than reasoned about. POST /v1/sessions/{id}/commands
// with kind=approve returned 202 and the decision was never applied: AcceptCommand only QUEUES, and the
// queue's single production drainer is command_pump.go:91 inside pumpCommands(st *attemptState) — which
// needs a live attempt. A run parked on a publication approval has none, because orchestrator.go:720-725
// ends the attempt and releases compute the moment it parks, deliberately, so no engine is held while a
// human reads. Three approve commands sat `queued` while the publication stayed `pending_approval` and the
// run stayed `waiting`; the console rendered its button, emitted command.accepted.v1, and changed nothing.
//
// AND THEN THE APPROVAL EXPIRED AND THE REAPER RELEASED THE RUN, which is why this survived so long. The
// symptom was never "approve does nothing" — it was "approve does nothing, and then thirty minutes later
// the run moves anyway". That reads as SLOW rather than BROKEN, and it is why nobody filed it. A failure
// that erases itself on a timer is the hardest kind to see: it is the same family as everything else this
// tree keeps finding — the silence, not the error.
//
// SO THIS IS NOT A NEW MECHANISM. orchestrator.go:724 already states the contract in its own words — "the
// decision (or the expiry reaper) opens a fresh attempt through the one wake" — and Slack's path honours it
// by calling ApplyApprovalDecision DIRECTLY, in one transaction that also wakes the parked run. This makes
// the HTTP surface honour the same contract. The command is still minted first, exactly as slack_decision.go
// mints it, so the boundary pump applies the SAME command under the SAME one-shot hash if it ever wins the
// race; losing that race is not failing.
//
// IT GOES THROUGH ApplyApprovalDecision AND NOTHING ELSE. That is where config_policy.approvers is
// enforced — "the one throat both surfaces pass through", asserted by an untagged guard that counts the
// production call sites of ApproverAllowed. A second path that transitioned a publication itself would be a
// privilege hole wearing a bugfix's clothes.
func (s *Store) decidePublicationApproval(ctx context.Context, scope middleware.Scope, tenant coordinator.Tenant,
	approvalID string, d api.ApprovalDecision) (api.ApprovalOutcome, error) {

	pub, found, err := s.spine.PublicationApprovalByID(ctx, tenant, approvalID)
	if err != nil {
		return api.ApprovalOutcome{}, fmt.Errorf("read the publication approval to decide: %w", err)
	}
	if !found {
		// Unknown, foreign, or no longer pending — indistinguishable, so this cannot become an oracle.
		return api.ApprovalOutcome{}, nil
	}
	// THE ONE-SHOT BINDING IS CHECKED HERE TOO, and not only inside the throat. ApplyApprovalDecision
	// compares the hash against the row it locks, but it treats a MISMATCH as "authorizes nothing and
	// settles the command" — which would burn this approval's command on a hash the caller got wrong.
	// Refusing at the edge leaves the approval decidable, which is what a mistyped hash deserves.
	if d.RequestHash != pub.RequestHash {
		return api.ApprovalOutcome{Found: true, Applied: false}, nil
	}

	kind := "deny"
	if d.Approve {
		kind = "approve"
	}
	approver := coordinator.ApproverPrincipal(coordinator.ApproverSurfaceKey, "", scope.APIKeyID)
	payload, err := json.Marshal(map[string]string{"request_hash": d.RequestHash, "approver": approver})
	if err != nil {
		return api.ApprovalOutcome{}, err
	}

	// The durable command first, the same shape and for the same reason slack_decision.go mints one: a
	// publication's decision is a command on the run so that whichever side reaches it first applies the
	// same one under the same hash.
	commandID := middleware.NewID("cmd")
	acc, err := s.spine.AcceptCommand(ctx, tenant, pub.SessionID, coordinator.CommandInput{
		CommandID: commandID, Kind: kind, Payload: payload,
	})
	switch {
	case err != nil:
		return api.ApprovalOutcome{}, fmt.Errorf("accept the publication approval command: %w", err)
	case acc.SessionNotFound:
		return api.ApprovalOutcome{}, nil
	case acc.State != "queued":
		// Durably refused at accept (a closed session, no pending approval by the time it committed). The
		// row stands as the audit trail of the attempt; nothing was authorized.
		return api.ApprovalOutcome{Found: true, Applied: false}, nil
	}

	switch _, err := s.spine.ApplyApprovalDecision(ctx, tenant, pub.SessionID, pub.ResponseID, pub.RunID,
		commandID, kind, d.RequestHash, approver); {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		return api.ApprovalOutcome{Found: true, Unauthorized: true}, nil
	case errors.Is(err, coordinator.ErrCommandNotPending):
		// The boundary pump (or an expiry sweep) settled the command first. Those are different facts and
		// only the publication's own state separates them: a decision that authorized nothing must never be
		// reported as applied. This is slack_decision.go's rule, verbatim, for the same reason.
		state, ok, serr := s.spine.PublicationState(ctx, tenant, pub.PublicationID)
		if serr != nil {
			return api.ApprovalOutcome{}, fmt.Errorf("read the publication after a settled command: %w", serr)
		}
		return api.ApprovalOutcome{Found: true, Applied: ok && state == decidedPublicationState(kind)}, nil
	case err != nil:
		return api.ApprovalOutcome{}, fmt.Errorf("apply the publication approval decision: %w", err)
	}
	return api.ApprovalOutcome{Found: true, Applied: true}, nil
}

// decidedPublicationState is the state a publication lands in for each decision — the pair
// ApplyApprovalDecision writes. It is duplicated from the Slack path's private helper rather than
// exported across that package boundary; the two are asserted to agree by the component test that drives
// both surfaces to the same terminal state.
func decidedPublicationState(kind string) string {
	if kind == "approve" {
		return "approved"
	}
	return "denied"
}

// credentialWord names the identity a publication will be written under. It is derived from whether the
// binding carries its OWN credential handle, which is the same condition RepositoryPublisher branches on —
// so the sentence on the screen and the credential the pump reaches for cannot disagree.
//
// An empty ref is not "unknown": it is the deployment App, and saying so is the whole point. A blank field
// on an approval screen reads as missing data, and an operator who cannot tell "no tenant credential" from
// "we did not look" has no basis to approve.
func credentialWord(connectionRef string) string {
	if connectionRef == "" {
		return api.CredentialDeploymentApp
	}
	return api.CredentialBindingOwn
}
