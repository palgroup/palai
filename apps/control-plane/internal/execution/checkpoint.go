package execution

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator/recovery"
)

// CheckpointObjectStore is the object-store PUT the opaque checkpoint bytes are written through. The
// engine never sees the S3 credential (spec §24) — the control plane holds it — so this seam is a
// structural interface satisfied by the control-plane-only artifacts.Store and injected at
// composition. Declaring the interface here (not importing artifacts) keeps the same decoupling the
// changeset ArtifactWriter uses.
type CheckpointObjectStore interface {
	Put(ctx context.Context, key string, body []byte) (checksum string, size int64, err error)
}

// CheckpointMeta is the control-plane-resolved provenance the engine's OPAQUE offer does not carry
// (spec §26.2): tenant/run/attempt identity, the engine handshake identity, the effective config
// snapshot hash, and the journal boundary. The offer frame itself carries only format + the opaque
// bytes. OfferSequence is the engine frame sequence of this offer; it makes the checkpoint id STABLE
// so a retransmitted offer re-derives the same id and is rejected by the immutable-row guard rather
// than duplicated.
type CheckpointMeta struct {
	Organization        string
	Project             string
	RunID               string
	AttemptID           string
	OfferSequence       int64
	EngineDigest        string
	EngineVersion       string
	ProtocolVersion     string
	ConfigSnapshotHash  string
	TranscriptSequence  int64
	WorkspaceSnapshotID string
}

// CheckpointSink persists an engine checkpoint.offer (spec §26.1-26.2): it decodes the opaque bytes,
// SIZE-BOUNDS them before the store PUT (so an oversize offer leaves no orphan object), writes them
// under a tenant-scoped key, and records the immutable metadata row. The bytes stay opaque — the
// control plane stores and checksums them, never interpreting them (§26.2, the engine boundary).
type CheckpointSink struct {
	store   CheckpointObjectStore
	objects *recovery.Objects
}

// NewCheckpointSink binds the object store and the recovery persistence layer.
func NewCheckpointSink(store CheckpointObjectStore, objects *recovery.Objects) *CheckpointSink {
	return &CheckpointSink{store: store, objects: objects}
}

// Persist writes one checkpoint.offer's bytes + metadata. offerData is the engine frame's data map
// {format, format_version, boundary_kind, state}; state is base64 of the opaque canonical bytes. A
// retransmitted offer (same run/attempt/offer-sequence) re-derives the same id and is rejected by
// the immutable-row guard (recovery.ErrCheckpointExists), never written twice.
func (s *CheckpointSink) Persist(ctx context.Context, meta CheckpointMeta, offerData map[string]any) error {
	stateB64, _ := offerData["state"].(string)
	raw, err := base64.StdEncoding.DecodeString(stateB64)
	if err != nil {
		return fmt.Errorf("decode checkpoint state: %w", err)
	}
	// Size-bound BEFORE the PUT: an oversize checkpoint is rejected with no orphan object written
	// (spec §26.2). recovery.Persist re-checks the size on the row as defense in depth.
	if len(raw) > recovery.MaxCheckpointBytes {
		return recovery.ErrCheckpointTooLarge
	}
	checkpointID := recoveryObjectID("chk", meta.RunID, meta.AttemptID, meta.OfferSequence)
	boundaryID := recoveryObjectID("bnd", meta.RunID, meta.AttemptID, meta.OfferSequence)
	key := checkpointObjectKey(meta.Organization, meta.Project, meta.RunID, checkpointID)
	checksum, size, err := s.store.Put(ctx, key, raw)
	if err != nil {
		return fmt.Errorf("store checkpoint bytes: %w", err)
	}
	format, _ := offerData["format"].(string)
	return s.objects.Persist(ctx, recovery.PersistInput{
		CheckpointID:        checkpointID,
		BoundaryID:          boundaryID,
		Organization:        meta.Organization,
		Project:             meta.Project,
		RunID:               meta.RunID,
		AttemptID:           meta.AttemptID,
		EngineDigest:        meta.EngineDigest,
		EngineVersion:       meta.EngineVersion,
		ProtocolVersion:     meta.ProtocolVersion,
		Format:              format,
		FormatVersion:       checkpointIntField(offerData["format_version"]),
		ConfigSnapshotHash:  meta.ConfigSnapshotHash,
		TranscriptSequence:  meta.TranscriptSequence,
		WorkspaceSnapshotID: meta.WorkspaceSnapshotID,
		ContentChecksum:     checksum,
		ObjectKey:           key,
		SizeBytes:           size,
	})
}

// persistCheckpoint records a checkpoint.offer at a safe boundary (spec §26.2). It resolves the
// control-plane-side provenance the opaque offer lacks — the effective config hash and the journal
// boundary — and hands the sink the bytes. With no object store wired, an offer is advisory: it is
// dropped, and no durable boundary is created (§26.5 — a checkpoint failure does not always fail the
// run; a missing sink is that "no recoverable boundary" case for a non-pausing boundary).
func (o *Orchestrator) persistCheckpoint(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	if o.checkpoints == nil {
		return nil
	}
	configHash, err := o.effectiveConfigHash(ctx, st)
	if err != nil {
		return fmt.Errorf("resolve config snapshot for checkpoint: %w", err)
	}
	transcriptSeq, err := o.spine.CurrentJournalSequence(ctx, st.tenant, st.sessionID)
	if err != nil {
		return fmt.Errorf("read journal boundary for checkpoint: %w", err)
	}
	// WorkspaceSnapshotID is empty in T1: a snapshot cut AT the boundary is T6. A checkpoint with no
	// snapshot declares no workspace dependency (spec §26.4), stored as NULL.
	return o.checkpoints.Persist(ctx, CheckpointMeta{
		Organization:       st.tenant.Organization,
		Project:            st.tenant.Project,
		RunID:              string(st.attempt.RunID),
		AttemptID:          string(st.attempt.AttemptID),
		OfferSequence:      int64(frame.Sequence),
		EngineDigest:       st.attempt.ImageDigest,
		EngineVersion:      st.engineVersion,
		ProtocolVersion:    st.protocolVersion,
		ConfigSnapshotHash: configHash,
		TranscriptSequence: transcriptSeq,
	}, frame.Data)
}

// effectiveConfigHash resolves the run's effective ConfigSnapshot at this boundary and returns its
// content hash (spec §14, §26.2) — the same layering effectiveModel/planConfigChange use, so the
// checkpoint records the config the run is actually executing under.
func (o *Orchestrator) effectiveConfigHash(ctx context.Context, st *attemptState) (string, error) {
	override, _, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID)
	if err != nil {
		return "", err
	}
	policy, err := o.spine.ProjectConfig(ctx, st.tenant)
	if err != nil {
		return "", err
	}
	snap := Resolve(ResolveInput{
		DeploymentModel:  o.route.Model,
		DeploymentSecret: string(o.route.Secret),
		ProjectTools:     policy.DefaultTools,
		SessionModel:     override.Model,
		SessionTools:     override.Tools,
	})
	return snap.Hash, nil
}

// recoveryObjectID derives a stable id for a recovery object from the offer's identity, so a
// retransmitted offer maps to the SAME id (idempotent-reject at the immutable row), not a duplicate.
func recoveryObjectID(prefix, runID, attemptID string, seq int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%s\x1f%s\x1f%d", prefix, runID, attemptID, seq)))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

// checkpointObjectKey lays out the S3 key tenant-first (defense in depth, the artifacts layout) with
// a checkpoints/ segment so checkpoint bytes never collide with artifact bytes for the same run.
func checkpointObjectKey(org, project, runID, checkpointID string) string {
	return fmt.Sprintf("%s/%s/%s/checkpoints/%s", org, project, runID, checkpointID)
}

// checkpointIntField reads an integer frame field that may have crossed a JSON boundary (numbers
// decode as float64) — format_version rides the offer as a JSON number.
func checkpointIntField(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
