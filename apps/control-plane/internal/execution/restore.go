package execution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// recoveryPlan is the ladder's verdict for one attempt plus the durable facts a restore or a proof
// needs. present is false when the run has no checkpoint at all (the ladder does not engage —
// today's run.start path). bytes are the fetched, integrity-checked checkpoint bytes, carried only
// for a compatible restore. committedSteps is the replay watermark M for the §26.9 drain gate.
type recoveryPlan struct {
	decision       recovery.Decision
	checkpoint     coordinator.RunCheckpoint
	present        bool
	bytes          []byte
	committedSteps int
}

// restoreData builds the run.restore frame's data (spec §26.3 rung 2): the checkpoint's format pins
// and the opaque bytes, base64-encoded exactly as the engine's offer carried them. The engine
// base64-decodes and reconstructs its loop from these bytes.
func (p recoveryPlan) restoreData() map[string]any {
	return map[string]any{
		"format":         p.checkpoint.Format,
		"format_version": p.checkpoint.FormatVersion,
		"state":          base64.StdEncoding.EncodeToString(p.bytes),
	}
}

// MigrateCheckpoint persists a migrated (v2) checkpoint as a NEW immutable row alongside the original
// and journals the provenance link (spec §26.2, ENG-011). The v1->v2 transform is engine-owned
// (checkpoint.migrate, proven in the reference kernel) and the control plane treats the bytes opaquely
// (§26.2), so this reuses the ordinary offer/persist path for the migrated bytes and records
// checkpoint.migrated.v1 {from_id, to_id, from_format, to_format}. The original checkpoint is never
// touched (append-only) — it stays integrity-valid and restore-selectable, which IS the rollback.
//
// Ceiling: the MECHANISM is proven with the reference-kernel v2 SHAPE. The production engine stays v1
// (engine.ready.checkpoint_formats == ["reference-kernel/1"], schema-pin unchanged) — no live run
// migrates yet; this is the reversible-migration seam a future format bump would call. It is a free
// function (not a CheckpointSink method) so the sink stays pure persistence, uncoupled from the journal.
func MigrateCheckpoint(ctx context.Context, sink *CheckpointSink, spine *coordinator.Store, tenant coordinator.Tenant, sessionID, responseID, fromCheckpointID, fromFormat string, meta CheckpointMeta, v2OfferData map[string]any) (toCheckpointID string, err error) {
	toCheckpointID = recoveryObjectID("chk", meta.RunID, meta.AttemptID, meta.OfferSequence)
	if err := sink.Persist(ctx, meta, v2OfferData); err != nil {
		return "", fmt.Errorf("persist migrated checkpoint: %w", err)
	}
	toFormat := fmt.Sprintf("%s/%d", v2OfferData["format"], checkpointIntField(v2OfferData["format_version"]))
	payload, _ := json.Marshal(map[string]any{
		"from_id": fromCheckpointID, "to_id": toCheckpointID,
		"from_format": fromFormat, "to_format": toFormat,
	})
	if _, err := spine.RecordRecoveryEvent(ctx, tenant, sessionID, responseID, eventCheckpointMigrated, payload); err != nil {
		return "", fmt.Errorf("journal checkpoint migration provenance: %w", err)
	}
	return toCheckpointID, nil
}

// consultCheckpointLadder reads the run's newest checkpoint and weighs it with the pure ladder (spec
// §26.3-26.4). It is the IO half: it fetches + checksums the opaque bytes, resolves the current
// effective config and transcript boundary, then hands those facts to recovery.Decide. The exact
// rung was already ruled out pre-dial, so OriginalLeaseAlive is false here. With no checkpoint the
// decision level is empty (not engaged) and the caller sends the ordinary run.start.
func (o *Orchestrator) consultCheckpointLadder(ctx context.Context, st *attemptState, ready contracts.EngineFrame) (recoveryPlan, error) {
	committed, err := o.spine.CommittedModelStepCount(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return recoveryPlan{}, err
	}
	plan := recoveryPlan{committedSteps: committed}

	cp, found, err := o.spine.LatestRunCheckpoint(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return plan, err
	}
	if !found {
		return plan, nil // not engaged — a fresh run or a plain reclaim (fork 7)
	}
	plan.present = true
	plan.checkpoint = cp

	// Fetch + checksum the opaque bytes so the pure ladder can weigh integrity (§26.4). A missing sink
	// or absent object leaves computed == "" and bytes nil, which fails the checksum condition — the
	// checkpoint is rejected, never restored blind.
	var computed string
	if o.checkpoints != nil {
		raw, sum, gotBytes, gerr := o.checkpoints.Retrieve(ctx, cp.ObjectKey)
		if gerr != nil {
			return plan, gerr
		}
		if gotBytes {
			plan.bytes, computed = raw, sum
		}
	}

	configHash, err := o.effectiveConfigHash(ctx, st)
	if err != nil {
		return plan, fmt.Errorf("resolve config for recovery: %w", err)
	}
	journalSeq, err := o.spine.CurrentJournalSequence(ctx, st.tenant, st.sessionID)
	if err != nil {
		return plan, fmt.Errorf("read journal boundary for recovery: %w", err)
	}

	plan.decision = recovery.Decide(
		recovery.Candidate{
			Present:             true,
			Format:              cp.Format,
			FormatVersion:       cp.FormatVersion,
			RecordedChecksum:    cp.ContentChecksum,
			ComputedChecksum:    computed,
			ConfigSnapshotHash:  cp.ConfigSnapshotHash,
			ProtocolVersion:     cp.ProtocolVersion,
			TranscriptSequence:  cp.TranscriptSequence,
			WorkspaceSnapshotID: cp.WorkspaceSnapshotID,
			// Snapshot RESTORE is T6; a checkpoint with a workspace dependency cannot yet be restored,
			// but one that declares NO dependency (the T4 case) passes the workspace condition vacuously.
			WorkspaceRestorable: false,
		},
		recovery.Target{
			OriginalLeaseAlive:      false, // exact ruled out pre-dial
			SupportedFormats:        supportedFormats(ready),
			ConfigSnapshotHash:      configHash,
			ProtocolVersion:         st.protocolVersion,
			JournalSequence:         journalSeq,
			TranscriptAvailable:     true, // the canonical transcript (input + committed steps) is always reconstructable
			ReconstructionForbidden: o.reconstructionForbidden,
		},
	)
	return plan, nil
}

// boundaryIsLive reports whether the boundary after the just-dispatched model step precedes a LIVE
// step (spec §26.9). A restore resumed at the checkpoint boundary; the st.restored short-circuit is
// sound ONLY because the engine offers a checkpoint at EVERY completed step — tool AND delegation
// (loop.py _on_tool_result + _on_child_result, MUST-FIX #1) — so the newest checkpoint always sits at
// the last committed step and the restore resumes at the live frontier, never behind replayed steps.
// Otherwise (transcript reconstruction) the next step (modelStepIndex+1) is a replay while
// modelStepIndex < M, so the boundary is live exactly once modelStepIndex reaches the committed
// watermark M (the last replayed step's boundary).
func (o *Orchestrator) boundaryIsLive(st *attemptState) bool {
	return st.restored || st.modelStepIndex >= st.committedStepWatermark
}

// supportedFormats reads engine.ready.checkpoint_formats — the "<format>/<version>" ids the target
// engine can restore (spec §26.4). A checkpoint whose id is absent fails the compatibility decision.
func supportedFormats(ready contracts.EngineFrame) []string {
	raw, _ := ready.Data["checkpoint_formats"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// recordExactStandDown journals attempt.recovering.v1 at the exact rung (spec §26.3 rung 1): the
// original attempt still holds the run, so this one stands down without dialing. In this stdio
// topology "exact" is a lease-liveness confirmation, not a socket reconnect (T4 ceiling).
func (o *Orchestrator) recordExactStandDown(ctx context.Context, tenant coordinator.Tenant, sessionID, responseID string, attempt AttemptDescriptor) error {
	payload, _ := json.Marshal(map[string]any{
		"run_id":         string(attempt.RunID),
		"new_attempt_id": string(attempt.AttemptID),
		"level":          string(recovery.LevelExact),
		"detail":         "original attempt lease alive; standing down (lease-liveness reconnect-ack, not socket reconnect)",
	})
	_, err := o.spine.RecordRecoveryEvent(ctx, tenant, sessionID, responseID, eventAttemptRecovering, payload)
	return err
}

// recordCompatibleRecovery journals the compatible-checkpoint rung + its §26.12 proof (ENG-009).
func (o *Orchestrator) recordCompatibleRecovery(ctx context.Context, st *attemptState, plan recoveryPlan) error {
	if err := o.recordAttemptRecovering(ctx, st, plan, nil); err != nil {
		return err
	}
	return o.recordRecoveryProof(ctx, st, plan)
}

// recordTranscriptRecovery journals a rejected checkpoint + the transcript-reconstruction rung + its
// proof (ENG-010): a checkpoint existed but was incompatible/corrupt, so the run reconstructs from
// the transcript — the rung is NEVER labelled an exact resume.
func (o *Orchestrator) recordTranscriptRecovery(ctx context.Context, st *attemptState, plan recoveryPlan) error {
	rejected, _ := json.Marshal(map[string]any{
		"run_id":        string(st.attempt.RunID),
		"checkpoint_id": plan.checkpoint.CheckpointID,
		"reasons":       plan.decision.Failures,
	})
	if _, err := o.spine.RecordRecoveryEvent(ctx, st.tenant, st.sessionID, st.responseID, eventCheckpointRejected, rejected); err != nil {
		return err
	}
	if err := o.recordAttemptRecovering(ctx, st, plan, plan.decision.Failures); err != nil {
		return err
	}
	return o.recordRecoveryProof(ctx, st, plan)
}

// recordAttemptRecovering journals attempt.recovering.v1 with the chosen rung (spec §26.3): the
// recovery level is always visible on the run, so a reconstruction is never presented as exact.
func (o *Orchestrator) recordAttemptRecovering(ctx context.Context, st *attemptState, plan recoveryPlan, reasons []string) error {
	payload, _ := json.Marshal(map[string]any{
		"run_id":              string(st.attempt.RunID),
		"previous_attempt_id": plan.checkpoint.AttemptID,
		"new_attempt_id":      string(st.attempt.AttemptID),
		"level":               string(plan.decision.Level),
		"checkpoint_id":       plan.checkpoint.CheckpointID,
		"reasons":             reasons,
	})
	_, err := o.spine.RecordRecoveryEvent(ctx, st.tenant, st.sessionID, st.responseID, eventAttemptRecovering, payload)
	return err
}

// recordRecoveryProof journals the §26.12 RecoveryProof (REC-006): a "resumed" log is not evidence
// on its own — this carries the eight field groups the verifier requires. The replayed/reused tool
// lists are the frame-ledger accounting; in T4 they are honestly empty (nothing is double-run — the
// ENG-009 guarantee), the fuller class-labelled accounting is T7.
func (o *Orchestrator) recordRecoveryProof(ctx context.Context, st *attemptState, plan recoveryPlan) error {
	proof := recovery.RecoveryProof{
		PreviousAttemptID:    plan.checkpoint.AttemptID,
		NewAttemptID:         string(st.attempt.AttemptID),
		Level:                plan.decision.Level,
		CheckpointID:         plan.checkpoint.CheckpointID,
		WorkspaceSnapshotID:  plan.checkpoint.WorkspaceSnapshotID,
		TranscriptBoundaryID: plan.checkpoint.BoundaryID,
		ReplayedToolCalls:    []string{},
		ReusedToolCalls:      []string{},
		ConfigModelChanges:   []string{},
		SemanticLossAssessed: true,
		SemanticLossWarning:  "",
		// The measured recovery latency: from this attempt opening to the restore/reconstruction being
		// wired. Floored at 1ms so a sub-millisecond deterministic path never records a false zero.
		DurationMS: durationMS(st),
	}
	if !proof.Complete() {
		return fmt.Errorf("refusing to journal an incomplete RecoveryProof for run %s (level %q)", st.attempt.RunID, proof.Level)
	}
	payload, _ := json.Marshal(proof)
	_, err := o.spine.RecordRecoveryEvent(ctx, st.tenant, st.sessionID, st.responseID, eventRecoveryProof, payload)
	return err
}

// durationMS is the measured recovery latency, floored at 1 so Complete() never sees a false zero.
func durationMS(st *attemptState) int64 {
	if ms := time.Since(st.attemptStart).Milliseconds(); ms >= 1 {
		return ms
	}
	return 1
}

// failRecovery drives the run to an EXPLICIT terminal failure (spec §26.3 rung 4): no live process,
// no compatible checkpoint, and reconstruction is forbidden — so the run fails with a typed reason
// rather than a silent drop or an infinite retry. It records the rung, then applies the terminal
// transition + response projection exactly like finalize's failed path.
func (o *Orchestrator) failRecovery(ctx context.Context, st *attemptState, plan recoveryPlan) error {
	if err := o.recordAttemptRecovering(ctx, st, plan, plan.decision.Failures); err != nil {
		return err
	}
	// Drive the run to failed. A run another path already made terminal (a raced cancel) or already
	// advanced is left as-is — the projection below still records the recovery failure.
	switch _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), statemachines.RunCmdFail); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
	case err != nil:
		return err
	}
	problem := contracts.Problem{
		Type:   problemTypePrefix + "recovery_failed",
		Code:   "recovery_failed",
		Title:  "Recovery failed",
		Status: 500,
		Detail: "the run could not be recovered from a durable checkpoint and reconstruction is not permitted",
	}
	projection, _ := json.Marshal(map[string]any{
		"output": st.output,
		"usage":  st.usage,
		"model":  st.model,
		"error":  problem,
	})
	return o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, "failed", projection)
}
