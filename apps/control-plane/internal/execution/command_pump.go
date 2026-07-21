package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
// Reclaim-crash-mid-fold (spec §26.9, E10 Task 2): a delivered message lives only in the engine
// subprocess's memory until a model request folds it, and run.start reassembles from prior-RESPONSE
// outputs only (history.go), never a mid-run user turn — so a fresh attempt used to drop it two ways.
// (variant-1) applied then the attempt crashes BEFORE the folding step commits: the command is
// drained (single-winner WHERE state='queued'), so nothing redelivered it and command.applied.v1
// claimed a lost effect. (R1) applied+folded+COMMITTED, then pause→resume: the committed answer
// survives, but the fresh attempt's context dropped that user turn on every post-resume step. BOTH
// close with one mechanism: ApplyCommand journals a durable delivered_messages row keyed by the
// boundary's deterministic model_request_id, and redeliverBoundaryMessages (below) refolds it at that
// SAME input boundary as the loop re-walks — variant-1 refolds live, R1 refolds into a replayed step's
// request, both at the input boundary (never inside a reconstructed step) and exactly once per
// reconstructed conversation. run.start is left UNCHANGED: a mid-run turn belongs at its boundary, not
// front-seeded among prior responses. Interrupt-path folds (InterruptModelStep) record no row — that
// recovery half is E10 Task 7 (ENG-012 outage ordering), not this boundary path.
func (o *Orchestrator) pumpCommands(ctx context.Context, st *attemptState, boundaryRequestID string) error {
	// Fail closed on a boundary with no model_request_id (spec §26.9, E10 Task 2): a delivered_messages
	// row written under a NULL boundary is unreachable by the redelivery read (boundary_request_id = $4
	// never matches NULL), so command.applied.v1 would silently lie again. The engine always sets the id
	// (loop.py), so an empty one is a malformed frame — fail the attempt rather than record an
	// unreachable row.
	if boundaryRequestID == "" {
		return fmt.Errorf("pump boundary carries no model_request_id: refusing to record an unreachable delivered-message row")
	}
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

	// Redeliver any message a PRIOR attempt recorded at this boundary BEFORE draining fresh queued
	// commands (spec §26.9, E10 Task 2). A crash between apply and the folding step's commit (variant-1)
	// or a pause/resume after it (R1) left the applied command drained, so only the durable
	// delivered_messages row traces it. Reading before the drain is what keeps the fold order stable
	// across reclaims: the fresh rows do not exist yet, so a message queued during the outage always
	// gets a HIGHER applied_sequence and folds AFTER the prior ones — the same [prior..., fresh] order
	// on every attempt (a flipped order would rebuild a different request for a committed step, which
	// LookupModelResult replays by id without hash-checking — a silent divergence). The engine folds at
	// flush_deliveries, so this lands at the input boundary, never inside a reconstructed step.
	if err := o.redeliverBoundaryMessages(ctx, st, boundaryRequestID); err != nil {
		return err
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
		// only needs the message and its delivery mode to fold it in at the input boundary. A fresh
		// command drained here was NOT redelivered above (its row did not exist at the read), so no
		// skip is needed — the redelivery set and the fresh-drain set are disjoint (applied vs queued).
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

// redeliverBoundaryMessages refolds, at this input boundary, every send_message a prior attempt
// durably delivered here but a crash/pause dropped from the engine's memory (spec §26.9, E10 Task 2).
// It keys on the boundary's deterministic model_request_id, so each message refolds at exactly its
// original position, in applied_sequence order. It runs BEFORE the fresh-queue drain, so it only ever
// sees prior-attempt rows (a command still queued has no row): the redelivery set and the drain set
// are disjoint, and the fold order is stable across reclaims. Sent once per boundary per attempt (the
// boundary recurs once as the loop re-walks), the reconstructed conversation carries each turn exactly
// once. Interrupt-delivered messages have no such row (E10 Task 7, ENG-012), so they are not redelivered.
func (o *Orchestrator) redeliverBoundaryMessages(ctx context.Context, st *attemptState, boundaryRequestID string) error {
	redeliver, err := o.spine.RedeliverBoundaryMessages(ctx, st.tenant, string(st.attempt.RunID), boundaryRequestID)
	if err != nil {
		return err
	}
	for _, m := range redeliver {
		frame := o.frame(st, "message.deliver", map[string]any{
			"command_id": m.CommandID,
			"delivery":   m.Delivery,
			"message":    decodeMessage(m.Payload),
		}, "")
		if err := st.ch.Send(ctx, frame); err != nil {
			return err
		}
	}
	return nil
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
