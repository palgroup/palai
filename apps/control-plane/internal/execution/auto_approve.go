package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/packages/coordinator"
)

// THE STANDING AUTHORIZATION, applied (E30 T1, migration 000056, spec §22.4).
//
// Two seams, one for each approval family, and they are separate functions for the same reason they are
// separate columns: auto-approving a gated TOOL call and auto-approving a PUBLICATION are not the same
// decision, and a reader who follows one must not accidentally read the rules of the other.
//
// WHAT NEITHER OF THEM IS: a second decision path. Both END in the function the human surfaces end in —
// DecideToolApproval for a gated call, ApplyApprovalDecision for a publication — because this tree
// already found what a second path costs. E23 T8 wired a decision surface to Slack alone and an operator
// without Slack had a gate that parked the run, asked nobody, and expired half an hour later; the fix was
// to make every surface a thin caller of one throat. An auto-approval that transitioned the row itself
// would be that mistake made deliberately, and it would sit UNDERNEATH the approver policy rather than
// on top of it.

// autoDecideToolApproval answers the gated call this attempt has just written an approval row for, IF the
// session carries a standing authorization for the tool family.
//
// It returns decided=false — leaving the caller to park the run for a human — in every case where the
// authorization does not squarely apply:
//
//   - the session armed nothing, or armed only the publication family;
//   - the project's `approvers` list does not name the arming principal (ErrApproverNotAuthorized), which
//     is the no-escalation property: arming a session grants what clicking would have granted;
//   - DecideToolApproval declined to apply — a racing path already moved the row, the deadline had
//     already passed, or the hash did not match.
//
// An ERROR is returned rather than converted into a park, and the distinction matters: a park says "a
// human must answer this", while an error aborts the attempt. A failure to READ the standing
// authorization is not evidence about what the operator wanted, so it must not be silently rendered as
// either answer.
func (o *Orchestrator) autoDecideToolApproval(ctx context.Context, st *attemptState, callID, requestHash string) (bool, error) {
	auto, err := o.spine.SessionAutoApprove(ctx, st.tenant, st.sessionID)
	if err != nil {
		return false, fmt.Errorf("read the session's standing authorization: %w", err)
	}
	if !auto.Tools {
		return false, nil
	}

	// THE DECISION IS MADE AS THE HUMAN WHO ARMED THE SESSION. Not as a machine, not as "the system", and
	// not as the model: a person said in advance that this session may run its gated tools, and the row
	// this writes says so by name. It is also what keeps the authority bounded — DecideToolApproval runs
	// approverAuthorizedTx on this principal before it transitions anything, so a project that restricts
	// who may approve restricts this exactly as it restricts a click.
	applied, err := o.spine.DecideToolApproval(ctx, st.tenant, coordinator.ToolApprovalDecision{
		ToolCallID:  callID,
		RequestHash: requestHash,
		DecidedBy:   auto.SetBy,
		Approve:     true,
	})
	switch {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		// The project named an approver list and the arming principal is not on it. The session is armed
		// and this call is still a human's to answer — which is the correct, and the only safe, reading of
		// "you may not approve things here".
		return false, nil
	case err != nil:
		return false, fmt.Errorf("apply the session's standing authorization to tool call %s: %w", callID, err)
	}
	return applied, nil
}

// autoDecidePublication answers a pending PUBLICATION approval if the session carries a standing
// authorization for the publication family.
//
// THIS HALF IS DELIBERATELY HARDER TO REACH THAN THE TOOL HALF, and the asymmetry is the design rather
// than caution for its own sake. A gated tool call's effects land in the run's own workspace while an
// operator watches; a publication writes to somebody's repository, and that write outlives the session,
// the sitting and the chat window. So the two never share a flag: `auto.Publications` is a separate
// column, defaulted off, and arming the tool half leaves it off.
//
// AND ONE MEASURED HAZARD BELONGS AT THIS SEAM RATHER THAN IN A DOCUMENT. It USED to be that on a
// deployment whose GitHub App was not configured, repositoryPublisherFromEnv returned nil and the approval
// pump was a silent no-op: the row reached `approved`, approval.approved.v1 was journalled, the run woke,
// and nothing was pushed. That specific silence is closed — the publisher is built independently of the
// App, and a publication with no credential path is refused at the tool rather than approved and dropped.
//
// WHAT DOES NOT CLOSE WITH IT is the reason this half defaults off. Every OTHER way a publish can fail
// (a diverged remote, a revoked tenant token, a 405 from branch protection) still lands as a warning on
// the publication row, and with a human in the loop somebody is at least waiting for the receipt. Armed,
// there is nobody left to notice — which is why the surface that arms this half has to say so, and why
// this returns the applied flag rather than swallowing it.
func (o *Orchestrator) autoDecidePublication(ctx context.Context, st *attemptState, pub coordinator.Publication) (bool, error) {
	auto, err := o.spine.SessionAutoApprove(ctx, st.tenant, st.sessionID)
	if err != nil {
		return false, fmt.Errorf("read the session's standing authorization: %w", err)
	}
	if !auto.Publications {
		return false, nil
	}

	// IT MINTS THE COMMAND A CLICK WOULD HAVE MINTED, and this is not ceremony. A publication's decision
	// is a durable command on the run — that is how the HTTP surface (store/approvals.go:216) and the
	// Slack surface both do it, so that whichever side reaches the decision first applies the same one
	// under the same hash. An auto-approval that reached ApplyApprovalDecision without a command would be
	// the one decision in this system with no command behind it, and the boundary pump's own accounting
	// would have a hole exactly where the human's does not.
	payload, err := json.Marshal(map[string]string{"request_hash": pub.RequestHash, "approver": auto.SetBy})
	if err != nil {
		return false, err
	}
	commandID := newExecID("cmd")
	acc, err := o.spine.AcceptCommand(ctx, st.tenant, st.sessionID, coordinator.CommandInput{
		CommandID: commandID, Kind: "approve", Payload: payload,
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("accept the standing authorization's approve command: %w", err)
	case acc.SessionNotFound, acc.State != "queued":
		// Durably refused at accept — a closed session, or no pending approval by the time it committed.
		// The row stands as the audit trail of the attempt and nothing was authorized, so this parks.
		return false, nil
	}

	switch _, err := o.spine.ApplyApprovalDecision(ctx, st.tenant, st.sessionID, st.responseID,
		string(st.attempt.RunID), commandID, "approve", pub.RequestHash, auto.SetBy); {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		// The project named an approver list the arming principal is not on. Same reading as the tool
		// half: the session is armed and this publication is still a human's to answer.
		return false, nil
	case errors.Is(err, coordinator.ErrCommandNotPending):
		// The boundary pump or an expiry sweep settled the command first. Whether anything was authorized
		// is a question only the publication's own state answers — a decision that authorized nothing must
		// never be reported as applied (store/approvals.go's rule, verbatim, for the same reason).
		state, ok, serr := o.spine.PublicationState(ctx, st.tenant, pub.ID)
		if serr != nil {
			return false, fmt.Errorf("read the publication after a settled command: %w", serr)
		}
		return ok && state == "approved", nil
	case err != nil:
		return false, fmt.Errorf("apply the session's standing authorization to publication approval: %w", err)
	}
	return true, nil
}
