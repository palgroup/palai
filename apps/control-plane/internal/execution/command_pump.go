package execution

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/packages/coordinator"
)

// errRunPaused signals that a pending pause was applied at this boundary, so the orchestrator must
// stop driving the loop — the run is now waiting and its compute is released when the attempt ends
// (spec §22.3, SES-009). It is not a failure: ExecuteAttempt ends the attempt cleanly on it (like a
// terminal run), leaving every other queued command for resume to re-deliver.
var errRunPaused = errors.New("run_paused")

// pumpCommands delivers a run's queued send_message commands at a safe boundary (spec §9.2,
// §22.4). It is the orchestrator's correlate-commit-dispatch role applied to commands: it never
// rewrites the engine loop — it reads the pending set, marks each command applied (durably,
// with the applied_sequence journaled in command.applied.v1), and sends the message.deliver
// frame the engine folds into the next model request.
//
// Deliver-once and fence discipline both fall out of ApplyCommand: its state transition is
// single-winner (WHERE state='queued'), so a redelivered boundary re-reads an already-drained
// set, and it runs under the run-terminal guard, so a canceled run's stale attempt cannot
// deliver — the same guard the model/tool commits use. A command another attempt already
// claimed returns ErrCommandNotPending and is skipped.
//
// ponytail: delivered-message loss across reclaim/resume — after ApplyCommand commits, the
// delivered message lives only in the engine subprocess's memory until a model request folds it,
// and run.start history reassembles from prior-RESPONSE outputs only (history.go), never a mid-run
// user turn. Two variants survive T4, both in the reclaim class (T4 did not invent either).
// The ORIGINAL variant: a message applied at a NORMAL boundary, then the attempt crashes BEFORE the
// folding model step commits — the fresh attempt gets no redelivery (the command is already drained,
// ApplyCommand is single-winner WHERE state='queued') and command.applied.v1 claims an effect that
// is lost. The R1 variant: a message applied+folded+COMMITTED at a NORMAL boundary, then the run
// pauses and RESUMES — the committed step's answer survives, but the fresh attempt's context drops
// that user turn on every post-resume step, because run.start carries prior responses only, the
// applied message is not redelivered, and step replay keys solely on model_request_id(run,step) with
// no request_hash check (LookupModelResult), so the diverged context is never caught. Unlike the
// original, R1 needs no crash: pause→resume triggers it user-initiated, crash-free.
// T4 pause/resume does NOT hit the original: pause PRE-EMPTS the boundary (below), leaving queued
// messages unapplied so resume re-delivers them from the queue, while committed model steps replay
// from the journal (LookupModelResult). Both variants are deferred to E10's recovery ladder (plan
// §7.4). The fix that closes BOTH is a durable delivered-message row — journaled at delivery, read
// by run.start history assembly and re-deliverable on resume. The narrower "applied-undelivered
// redelivery at attempt start" idea closes only the original: in R1 the message WAS delivered and
// committed, so there is nothing undelivered to replay — only the durable row restores the lost turn.
func (o *Orchestrator) pumpCommands(ctx context.Context, st *attemptState, boundaryRequestID string) error {
	// A pending pause pre-empts the boundary (spec §22.3, SES-009): apply it and stop driving —
	// every other queued command stays queued for resume to re-deliver (faithful resume). Read
	// before the delivery set so a pause queued after a message still wins the boundary.
	switch pauseID, found, err := o.spine.PendingPauseCommand(ctx, st.tenant, string(st.attempt.RunID)); {
	case err != nil:
		return err
	case found:
		if _, err := o.spine.PauseRun(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), pauseID); err != nil {
			return err
		}
		return errRunPaused
	}

	pending, err := o.spine.PendingBoundaryCommands(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return err
	}
	for _, cmd := range pending {
		// A change_config applies at this boundary so the NEXT model step routes under the new
		// config (the normal switch — spec §9.3); it emits no engine frame, the resolver reads
		// the revision. An immediate switch normally settles in-flight (the watcher), but one
		// that missed the window degrades here, same as a missed send_message interrupt.
		if cmd.Kind == "change_config" {
			if err := o.applyBoundaryConfigChange(ctx, st, cmd); err != nil {
				return err
			}
			continue
		}
		// approve/deny transition the session's pending publication (spec §22.4-22.5, APV-001): the
		// approve drives it to a DURABLE approved state and the deny blocks it. The approval PUMP
		// (pumpApprovedPublications) then publishes an approved operation at this same boundary.
		if cmd.Kind == "approve" || cmd.Kind == "deny" {
			if err := o.applyBoundaryApproval(ctx, st, cmd); err != nil {
				return err
			}
			continue
		}
		_, err := o.spine.ApplyCommand(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), cmd.ID, boundaryRequestID)
		if errors.Is(err, coordinator.ErrCommandNotPending) {
			continue // another boundary already delivered it
		}
		if err != nil {
			return err
		}
		// The applied_sequence is journaled in command.applied.v1 (ApplyCommand); the engine
		// only needs the message and its delivery mode to fold it in at the input boundary.
		frame := o.frame(st, "message.deliver", map[string]any{
			"command_id": cmd.ID,
			"delivery":   cmd.Delivery,
			"message":    decodeMessage(cmd.Payload),
		}, "")
		if err := st.ch.Send(ctx, frame); err != nil {
			return err
		}
	}
	// After the queued commands settle (an approve may have just driven a publication to approved),
	// publish any approved publication at this same boundary (spec §30.9-30.10, APV-001). A no-op
	// without a wired publisher.
	return o.pumpApprovedPublications(ctx, st)
}

// applyBoundaryConfigChange applies a queued change_config at a safe boundary: it resolves the
// new ConfigSnapshot and commits the revision (config.revised.v1) so the next model step routes
// under it (spec §9.3). No engine frame — the effect is the resolver reading the revision at the
// next step. A change a racing path already applied returns ErrCommandNotPending (a no-op).
func (o *Orchestrator) applyBoundaryConfigChange(ctx context.Context, st *attemptState, cmd coordinator.PendingCommand) error {
	plan, err := o.planConfigChange(ctx, st, cmd.ID, cmd.Payload)
	if err != nil {
		return err
	}
	switch _, err := o.spine.ApplyConfigChange(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), cmd.ID, plan); {
	case errors.Is(err, coordinator.ErrCommandNotPending):
		return nil
	default:
		return err
	}
}

// applyPendingSessionConfig applies, at run start (before the first model.request), any
// change_config accepted for this session that never reached a boundary in its own run — an
// idle-session change, or a single-step run whose only step had no boundary to pump at (spec
// §9.3, the cross-run config carry). It reuses applyBoundaryConfigChange, so the revision is
// committed under the current run's active guard and the first step's resolver reads it. Each
// apply is single-winner (WHERE state='queued'), so a change a boundary or the interrupt watcher
// already settled is skipped, and a reclaimed attempt re-draining is a no-op.
func (o *Orchestrator) applyPendingSessionConfig(ctx context.Context, st *attemptState) error {
	pending, err := o.spine.PendingSessionConfigCommands(ctx, st.tenant, st.sessionID)
	if err != nil {
		return err
	}
	for _, cmd := range pending {
		if err := o.applyBoundaryConfigChange(ctx, st, cmd); err != nil {
			return err
		}
	}
	return nil
}

// applyBoundaryApproval applies a queued approve/deny at a safe boundary: it transitions the session's
// pending publication (approve -> approved, deny -> denied) and marks the command applied in one
// transaction (spec §22.4-22.5, APV-001). The approve carries the one-shot request hash it authorizes; a
// stale hash (a moved head or an edited request) settles the command without approving. A command a
// racing path already applied returns ErrCommandNotPending (a no-op).
func (o *Orchestrator) applyBoundaryApproval(ctx context.Context, st *attemptState, cmd coordinator.PendingCommand) error {
	switch _, err := o.spine.ApplyApprovalDecision(ctx, st.tenant, st.sessionID, st.responseID,
		string(st.attempt.RunID), cmd.ID, cmd.Kind, decodeApprovalRequestHash(cmd.Payload)); {
	case errors.Is(err, coordinator.ErrCommandNotPending):
		return nil
	default:
		return err
	}
}

// decodeMessage reads the send_message text from a command payload ({"message": "..."}). A
// malformed payload yields the empty string; the API validates a non-empty message at accept.
func decodeMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Message
}

// decodeApprovalRequestHash reads the one-shot request hash from an approve/deny command payload
// ({"request_hash": "..."}) — the token binding the decision to the exact operation it authorizes
// (spec §22.4). Empty (a bare approve) authorizes the session's sole pending approval.
func decodeApprovalRequestHash(payload []byte) string {
	var body struct {
		RequestHash string `json:"request_hash"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.RequestHash
}
